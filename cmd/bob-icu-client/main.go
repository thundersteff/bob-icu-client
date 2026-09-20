package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

var (
	version = "dev"
	commit  = "unknown"
	keyRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
)

const protocolSchema = "monitor.v1"

type Config struct {
	Endpoint                 string          `json:"endpoint"`
	SourceID                 string          `json:"source_id"`
	SecretFile               string          `json:"secret_file"`
	StateDatabase            string          `json:"state_database"`
	HeartbeatIntervalSeconds int64           `json:"heartbeat_interval_seconds"`
	RequestTimeoutSeconds    int64           `json:"request_timeout_seconds"`
	MaxQueueItems            int             `json:"max_queue_items"`
	MaxParallelSensors       int             `json:"max_parallel_sensors"`
	AgentVersion             string          `json:"agent_version"`
	Services                 []ServiceConfig `json:"services"`
}

type ServiceConfig struct {
	ServiceID   string         `json:"service_id"`
	DisplayName string         `json:"display_name"`
	Sensors     []SensorConfig `json:"sensors"`
}

type SensorConfig struct {
	SensorID        string   `json:"sensor_id"`
	DisplayName     string   `json:"display_name"`
	ValueType       string   `json:"value_type"`
	Unit            string   `json:"unit,omitempty"`
	IntervalSeconds int64    `json:"interval_seconds"`
	TimeoutSeconds  int64    `json:"timeout_seconds"`
	Builtin         string   `json:"builtin,omitempty"`
	Command         []string `json:"command,omitempty"`
}

type Client struct {
	cfg        Config
	secret     string
	db         *sql.DB
	http       *http.Client
	log        *slog.Logger
	now        func() time.Time
	queueWake  chan struct{}
	stop       chan struct{}
	sensorErrs atomic.Int64
	scheduler  atomic.Bool
	sequence   atomic.Int64
	bootID     string
	wg         sync.WaitGroup
}

type Manifest struct {
	Schema      string            `json:"schema"`
	Revision    int64             `json:"revision"`
	CatalogHash string            `json:"catalog_hash"`
	Services    []ManifestService `json:"services"`
}

type ManifestService struct {
	ServiceID   string           `json:"service_id"`
	DisplayName string           `json:"display_name"`
	Sensors     []ManifestSensor `json:"sensors"`
}

type ManifestSensor struct {
	SensorID        string `json:"sensor_id"`
	DisplayName     string `json:"display_name"`
	ValueType       string `json:"value_type"`
	Unit            string `json:"unit,omitempty"`
	IntervalSeconds int64  `json:"interval_seconds"`
}

type ReadingEnvelope struct {
	Schema   string    `json:"schema"`
	Readings []Reading `json:"readings"`
}

type Reading struct {
	ServiceID  string `json:"service_id"`
	SensorID   string `json:"sensor_id"`
	ObservedAt string `json:"observed_at"`
	Value      any    `json:"value"`
	Message    string `json:"message,omitempty"`
}

type Heartbeat struct {
	Schema       string `json:"schema"`
	AgentVersion string `json:"agent_version"`
	BootID       string `json:"boot_id"`
	Sequence     int64  `json:"sequence"`
	CatalogHash  string `json:"catalog_hash"`
	Message      string `json:"message"`
}

type SensorResult struct {
	Value   any    `json:"value"`
	Message string `json:"message,omitempty"`
}

type queuedItem struct {
	ID          int64
	EventID     string
	Path        string
	Body        []byte
	Attempts    int
	NextAttempt int64
}

func main() {
	configPath := flag.String("config", "/etc/bob-icu/client.json", "configuration file")
	check := flag.Bool("check-config", false, "validate configuration, secret and state database")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Printf("bob-icu-client %s (%s) %s/%s\n", version, commit, runtime.GOOS, runtime.GOARCH)
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, secret, err := loadConfig(*configPath)
	if err != nil {
		logger.Error("configuration rejected", "error", err)
		os.Exit(1)
	}
	client, err := newClient(cfg, secret, logger)
	if err != nil {
		logger.Error("startup failed", "error", err)
		os.Exit(1)
	}
	defer client.db.Close()
	if *check {
		logger.Info("configuration valid", "source_id", cfg.SourceID, "services", len(cfg.Services), "database", cfg.StateDatabase)
		return
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	if err := client.run(ctx); err != nil {
		logger.Error("client stopped", "error", err)
		os.Exit(1)
	}
}

func loadConfig(path string) (Config, string, error) {
	var cfg Config
	if err := decodeFile(path, &cfg); err != nil {
		return cfg, "", err
	}
	if cfg.HeartbeatIntervalSeconds == 0 {
		cfg.HeartbeatIntervalSeconds = 60
	}
	if cfg.RequestTimeoutSeconds == 0 {
		cfg.RequestTimeoutSeconds = 20
	}
	if cfg.MaxQueueItems == 0 {
		cfg.MaxQueueItems = 10000
	}
	if cfg.MaxParallelSensors == 0 {
		cfg.MaxParallelSensors = 4
	}
	if cfg.AgentVersion == "" {
		cfg.AgentVersion = version
	}
	if !keyRE.MatchString(cfg.SourceID) {
		return cfg, "", errors.New("invalid source_id")
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" {
		return cfg, "", errors.New("endpoint must be an HTTPS origin without a path")
	}
	if cfg.StateDatabase == "" || cfg.SecretFile == "" {
		return cfg, "", errors.New("state_database and secret_file are required")
	}
	if cfg.HeartbeatIntervalSeconds < 10 || cfg.HeartbeatIntervalSeconds > 3600 {
		return cfg, "", errors.New("heartbeat interval must be 10..3600 seconds")
	}
	if cfg.RequestTimeoutSeconds < 2 || cfg.RequestTimeoutSeconds > 120 {
		return cfg, "", errors.New("request timeout must be 2..120 seconds")
	}
	if cfg.MaxQueueItems < 100 || cfg.MaxQueueItems > 1000000 {
		return cfg, "", errors.New("max_queue_items must be 100..1000000")
	}
	if cfg.MaxParallelSensors < 1 || cfg.MaxParallelSensors > 32 {
		return cfg, "", errors.New("max_parallel_sensors must be 1..32")
	}
	if len(cfg.Services) > 64 {
		return cfg, "", errors.New("service limit exceeded")
	}
	services := map[string]bool{}
	for serviceIndex := range cfg.Services {
		svc := &cfg.Services[serviceIndex]
		if !keyRE.MatchString(svc.ServiceID) || strings.TrimSpace(svc.DisplayName) == "" || services[svc.ServiceID] {
			return cfg, "", fmt.Errorf("invalid or duplicate service %q", svc.ServiceID)
		}
		services[svc.ServiceID] = true
		if len(svc.Sensors) > 32 {
			return cfg, "", fmt.Errorf("sensor limit exceeded for %s", svc.ServiceID)
		}
		sensors := map[string]bool{}
		for sensorIndex := range svc.Sensors {
			s := &svc.Sensors[sensorIndex]
			if !keyRE.MatchString(s.SensorID) || strings.TrimSpace(s.DisplayName) == "" || sensors[s.SensorID] {
				return cfg, "", fmt.Errorf("invalid or duplicate sensor in %s", svc.ServiceID)
			}
			sensors[s.SensorID] = true
			if s.ValueType != "number" && s.ValueType != "text" {
				return cfg, "", fmt.Errorf("invalid value_type for %s.%s", svc.ServiceID, s.SensorID)
			}
			if s.IntervalSeconds < 10 || s.IntervalSeconds > 86400 {
				return cfg, "", fmt.Errorf("invalid interval for %s.%s", svc.ServiceID, s.SensorID)
			}
			if s.TimeoutSeconds == 0 {
				s.TimeoutSeconds = 10
			}
			if s.TimeoutSeconds < 1 || s.TimeoutSeconds >= s.IntervalSeconds {
				return cfg, "", fmt.Errorf("invalid timeout for %s.%s", svc.ServiceID, s.SensorID)
			}
			if (s.Builtin == "") == (len(s.Command) == 0) {
				return cfg, "", fmt.Errorf("sensor %s.%s needs exactly one builtin or command", svc.ServiceID, s.SensorID)
			}
			if s.Builtin != "" && !knownBuiltin(s.Builtin) {
				return cfg, "", fmt.Errorf("unknown builtin %q", s.Builtin)
			}
		}
	}
	secretBytes, err := os.ReadFile(cfg.SecretFile)
	if err != nil {
		return cfg, "", err
	}
	secret := strings.TrimSpace(string(secretBytes))
	if len(secret) < 32 {
		return cfg, "", errors.New("client secret must contain at least 32 characters")
	}
	return cfg, secret, nil
}

func decodeFile(path string, dst any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return fmt.Errorf("decode %s: trailing data", path)
	}
	return nil
}

func newClient(cfg Config, secret string, logger *slog.Logger) (*Client, error) {
	if err := os.MkdirAll(filepath.Dir(cfg.StateDatabase), 0750); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", cfg.StateDatabase)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	c := &Client{cfg: cfg, secret: secret, db: db, log: logger, now: time.Now, queueWake: make(chan struct{}, 1), stop: make(chan struct{}), bootID: randomID()}
	c.http = &http.Client{Timeout: time.Duration(cfg.RequestTimeoutSeconds) * time.Second}
	if err := c.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if err := os.Chmod(cfg.StateDatabase, 0600); err != nil {
		db.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) migrate() error {
	_, err := c.db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000; PRAGMA synchronous=NORMAL; PRAGMA auto_vacuum=INCREMENTAL;
CREATE TABLE IF NOT EXISTS queue(id INTEGER PRIMARY KEY AUTOINCREMENT,event_id TEXT NOT NULL UNIQUE,path TEXT NOT NULL,body BLOB NOT NULL,created_at_ms INTEGER NOT NULL,attempts INTEGER NOT NULL DEFAULT 0,next_attempt_ms INTEGER NOT NULL,last_error TEXT);
CREATE INDEX IF NOT EXISTS queue_due ON queue(id,next_attempt_ms);
CREATE TABLE IF NOT EXISTS dead_letters(id INTEGER PRIMARY KEY AUTOINCREMENT,event_id TEXT NOT NULL,path TEXT NOT NULL,body BLOB NOT NULL,failed_at_ms INTEGER NOT NULL,http_status INTEGER,last_error TEXT);
CREATE TABLE IF NOT EXISTS sensor_state(service_id TEXT NOT NULL,sensor_id TEXT NOT NULL,last_started_ms INTEGER,last_finished_ms INTEGER,last_success_ms INTEGER,last_error TEXT,next_due_ms INTEGER,PRIMARY KEY(service_id,sensor_id));
CREATE TABLE IF NOT EXISTS metadata(key TEXT PRIMARY KEY,value TEXT NOT NULL);`)
	return err
}

func (c *Client) run(ctx context.Context) error {
	manifest, hash, err := c.buildManifest()
	if err != nil {
		return err
	}
	if err := c.enqueueJSON("/v1/manifest", manifest, true); err != nil {
		return err
	}
	c.scheduler.Store(true)
	c.wg.Add(4)
	go func() { defer c.wg.Done(); c.transportLoop(ctx) }()
	go func() { defer c.wg.Done(); c.heartbeatLoop(ctx, hash) }()
	go func() { defer c.wg.Done(); c.schedulerLoop(ctx) }()
	go func() { defer c.wg.Done(); c.watchdogLoop(ctx) }()
	c.log.Info("BOB ICU client started", "source_id", c.cfg.SourceID, "services", len(c.cfg.Services), "version", c.cfg.AgentVersion, "boot_id", c.bootID)
	<-ctx.Done()
	c.scheduler.Store(false)
	close(c.stop)
	c.wg.Wait()
	return nil
}

func (c *Client) buildManifest() (Manifest, string, error) {
	m := Manifest{Schema: protocolSchema, Revision: c.now().Unix()}
	for _, svc := range c.cfg.Services {
		ms := ManifestService{ServiceID: svc.ServiceID, DisplayName: svc.DisplayName}
		for _, s := range svc.Sensors {
			ms.Sensors = append(ms.Sensors, ManifestSensor{SensorID: s.SensorID, DisplayName: s.DisplayName, ValueType: s.ValueType, Unit: s.Unit, IntervalSeconds: s.IntervalSeconds})
		}
		m.Services = append(m.Services, ms)
	}
	b, err := json.Marshal(m.Services)
	if err != nil {
		return m, "", err
	}
	sum := sha256.Sum256(b)
	m.CatalogHash = "sha256:" + hex.EncodeToString(sum[:])
	return m, m.CatalogHash, nil
}

func (c *Client) heartbeatLoop(ctx context.Context, catalogHash string) {
	send := func() {
		depth, _ := c.queueDepth()
		message := fmt.Sprintf("scheduler=%t queue=%d sensor_errors=%d", c.scheduler.Load(), depth, c.sensorErrs.Load())
		hb := Heartbeat{Schema: protocolSchema, AgentVersion: c.cfg.AgentVersion, BootID: c.bootID, Sequence: c.sequence.Add(1), CatalogHash: catalogHash, Message: message}
		if err := c.enqueueJSON("/v1/heartbeat", hb, true); err != nil {
			c.log.Error("heartbeat enqueue failed", "error", err)
		}
	}
	send()
	t := time.NewTicker(time.Duration(c.cfg.HeartbeatIntervalSeconds) * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			send()
		}
	}
}

type scheduledSensor struct {
	service ServiceConfig
	sensor  SensorConfig
	next    time.Time
}

func (c *Client) schedulerLoop(ctx context.Context) {
	var items []*scheduledSensor
	now := c.now()
	for _, svc := range c.cfg.Services {
		for _, s := range svc.Sensors {
			items = append(items, &scheduledSensor{service: svc, sensor: s, next: now.Add(deterministicJitter(c.cfg.SourceID+"/"+svc.ServiceID+"/"+s.SensorID, 3*time.Second))})
		}
	}
	sem := make(chan struct{}, c.cfg.MaxParallelSensors)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now = <-tick.C:
			for _, item := range items {
				if now.Before(item.next) {
					continue
				}
				item.next = now.Add(time.Duration(item.sensor.IntervalSeconds) * time.Second)
				sem <- struct{}{}
				go func(it *scheduledSensor) {
					defer func() { <-sem }()
					c.runSensor(ctx, it.service, it.sensor)
				}(item)
			}
		}
	}
}

func (c *Client) runSensor(parent context.Context, svc ServiceConfig, s SensorConfig) {
	started := c.now()
	_, _ = c.db.Exec(`INSERT INTO sensor_state(service_id,sensor_id,last_started_ms,next_due_ms) VALUES(?,?,?,?) ON CONFLICT(service_id,sensor_id) DO UPDATE SET last_started_ms=excluded.last_started_ms,next_due_ms=excluded.next_due_ms`, svc.ServiceID, s.SensorID, started.UnixMilli(), started.Add(time.Duration(s.IntervalSeconds)*time.Second).UnixMilli())
	timeout := s.TimeoutSeconds
	if timeout == 0 {
		timeout = 10
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(timeout)*time.Second)
	defer cancel()
	result, err := executeSensor(ctx, s)
	finished := c.now()
	if err != nil {
		c.sensorErrs.Add(1)
		_, _ = c.db.Exec(`UPDATE sensor_state SET last_finished_ms=?,last_error=? WHERE service_id=? AND sensor_id=?`, finished.UnixMilli(), truncate(err.Error(), 500), svc.ServiceID, s.SensorID)
		c.log.Warn("sensor failed", "service", svc.ServiceID, "sensor", s.SensorID, "error", err)
		return
	}
	value, err := normalizeValue(s.ValueType, result.Value)
	if err != nil {
		c.sensorErrs.Add(1)
		_, _ = c.db.Exec(`UPDATE sensor_state SET last_finished_ms=?,last_error=? WHERE service_id=? AND sensor_id=?`, finished.UnixMilli(), truncate(err.Error(), 500), svc.ServiceID, s.SensorID)
		c.log.Warn("sensor value rejected", "service", svc.ServiceID, "sensor", s.SensorID, "error", err)
		return
	}
	envelope := ReadingEnvelope{Schema: protocolSchema, Readings: []Reading{{ServiceID: svc.ServiceID, SensorID: s.SensorID, ObservedAt: finished.Format(time.RFC3339Nano), Value: value, Message: result.Message}}}
	if err := c.enqueueJSON("/v1/readings", envelope, false); err != nil {
		c.log.Error("reading enqueue failed", "service", svc.ServiceID, "sensor", s.SensorID, "error", err)
		return
	}
	_, _ = c.db.Exec(`UPDATE sensor_state SET last_finished_ms=?,last_success_ms=?,last_error=NULL WHERE service_id=? AND sensor_id=?`, finished.UnixMilli(), finished.UnixMilli(), svc.ServiceID, s.SensorID)
}

func executeSensor(ctx context.Context, s SensorConfig) (SensorResult, error) {
	if s.Builtin != "" {
		return runBuiltin(s.Builtin)
	}
	cmd := exec.CommandContext(ctx, s.Command[0], s.Command[1:]...)
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}
	var stdout, stderr limitedBuffer
	stdout.max = 65536
	stderr.max = 65536
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return SensorResult{}, fmt.Errorf("command failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var result SensorResult
	if err := strictJSON(stdout.Bytes(), &result); err != nil {
		return SensorResult{}, fmt.Errorf("invalid sensor JSON: %w", err)
	}
	return result, nil
}

func knownBuiltin(name string) bool {
	switch name {
	case "host.uptime_seconds", "host.load1", "host.memory_available_percent", "host.root_disk_used_percent":
		return true
	}
	return false
}
func runBuiltin(name string) (SensorResult, error) {
	switch name {
	case "host.uptime_seconds":
		b, e := os.ReadFile("/proc/uptime")
		if e != nil {
			return SensorResult{}, e
		}
		f := strings.Fields(string(b))
		v, e := strconv.ParseFloat(f[0], 64)
		return SensorResult{Value: v, Message: "OK"}, e
	case "host.load1":
		b, e := os.ReadFile("/proc/loadavg")
		if e != nil {
			return SensorResult{}, e
		}
		f := strings.Fields(string(b))
		v, e := strconv.ParseFloat(f[0], 64)
		return SensorResult{Value: v, Message: "OK"}, e
	case "host.memory_available_percent":
		b, e := os.ReadFile("/proc/meminfo")
		if e != nil {
			return SensorResult{}, e
		}
		vals := map[string]float64{}
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				v, _ := strconv.ParseFloat(f[1], 64)
				vals[strings.TrimSuffix(f[0], ":")] = v
			}
		}
		if vals["MemTotal"] <= 0 {
			return SensorResult{}, errors.New("MemTotal missing")
		}
		return SensorResult{Value: 100 * vals["MemAvailable"] / vals["MemTotal"], Message: "OK"}, nil
	case "host.root_disk_used_percent":
		var st syscall.Statfs_t
		if e := syscall.Statfs("/", &st); e != nil {
			return SensorResult{}, e
		}
		total := float64(st.Blocks)
		if total <= 0 {
			return SensorResult{}, errors.New("filesystem block count is zero")
		}
		return SensorResult{Value: 100 * (total - float64(st.Bavail)) / total, Message: "OK"}, nil
	}
	return SensorResult{}, errors.New("unknown builtin")
}

func normalizeValue(kind string, v any) (any, error) {
	switch kind {
	case "number":
		switch n := v.(type) {
		case float64:
			if math.IsNaN(n) || math.IsInf(n, 0) {
				return nil, errors.New("number must be finite")
			}
			return n, nil
		case json.Number:
			f, e := n.Float64()
			return f, e
		default:
			return nil, errors.New("expected numeric value")
		}
	case "text":
		s, ok := v.(string)
		if !ok {
			return nil, errors.New("expected text value")
		}
		return s, nil
	}
	return nil, errors.New("unknown value type")
}

func (c *Client) enqueueJSON(path string, payload any, coalesce bool) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	event := randomID()
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if coalesce {
		if _, err = tx.Exec(`DELETE FROM queue WHERE path=?`, path); err != nil {
			return err
		}
	}
	var count int
	if err = tx.QueryRow(`SELECT count(*) FROM queue`).Scan(&count); err != nil {
		return err
	}
	if count >= c.cfg.MaxQueueItems {
		if _, err = tx.Exec(`DELETE FROM queue WHERE id=(SELECT id FROM queue WHERE path='/v1/heartbeat' ORDER BY id LIMIT 1)`); err != nil {
			return err
		}
		if err = tx.QueryRow(`SELECT count(*) FROM queue`).Scan(&count); err != nil {
			return err
		}
		if count >= c.cfg.MaxQueueItems {
			return errors.New("queue capacity reached")
		}
	}
	now := c.now().UnixMilli()
	if _, err = tx.Exec(`INSERT INTO queue(event_id,path,body,created_at_ms,next_attempt_ms) VALUES(?,?,?,?,?)`, event, path, body, now, now); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	select {
	case c.queueWake <- struct{}{}:
	default:
	}
	return nil
}

func (c *Client) transportLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		item, err := c.nextItem()
		if errors.Is(err, sql.ErrNoRows) {
			select {
			case <-ctx.Done():
				return
			case <-c.queueWake:
				continue
			case <-time.After(time.Second):
				continue
			}
		}
		if err != nil {
			c.log.Error("queue read failed", "error", err)
			sleepContext(ctx, time.Second)
			continue
		}
		wait := time.Until(time.UnixMilli(item.NextAttempt))
		if wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-c.queueWake:
				continue
			case <-time.After(minDuration(wait, time.Second)):
				continue
			}
		}
		status, sendErr := c.send(ctx, item)
		if sendErr == nil && (status >= 200 && status < 300 || status == 409) {
			_, _ = c.db.Exec(`DELETE FROM queue WHERE id=?`, item.ID)
			continue
		}
		if status == 401 || status == 413 || status == 422 {
			c.log.Error("server rejected queued item", "path", item.Path, "status", status, "event_id", item.EventID)
		}
		attempts := item.Attempts + 1
		delay := retryDelay(attempts)
		message := ""
		if sendErr != nil {
			message = sendErr.Error()
		} else {
			message = fmt.Sprintf("HTTP %d", status)
		}
		_, _ = c.db.Exec(`UPDATE queue SET attempts=?,next_attempt_ms=?,last_error=? WHERE id=?`, attempts, c.now().Add(delay).UnixMilli(), truncate(message, 500), item.ID)
		sleepContext(ctx, minDuration(delay, time.Second))
	}
}

func (c *Client) nextItem() (queuedItem, error) {
	var q queuedItem
	err := c.db.QueryRow(`SELECT id,event_id,path,body,attempts,next_attempt_ms FROM queue ORDER BY id LIMIT 1`).Scan(&q.ID, &q.EventID, &q.Path, &q.Body, &q.Attempts, &q.NextAttempt)
	return q, err
}
func (c *Client) send(ctx context.Context, q queuedItem) (int, error) {
	ts := strconv.FormatInt(c.now().Unix(), 10)
	sum := sha256.Sum256(q.Body)
	canonical := strings.Join([]string{"POST", q.Path, ts, q.EventID, hex.EncodeToString(sum[:])}, "\n")
	mac := hmac.New(sha256.New, []byte(c.secret))
	mac.Write([]byte(canonical))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.cfg.Endpoint, "/")+q.Path, bytes.NewReader(q.Body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Monitor-Source", c.cfg.SourceID)
	req.Header.Set("X-Monitor-Timestamp", ts)
	req.Header.Set("X-Monitor-Event", q.EventID)
	req.Header.Set("X-Monitor-Signature", hex.EncodeToString(mac.Sum(nil)))
	res, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 65536))
	return res.StatusCode, nil
}

func (c *Client) queueDepth() (int, error) {
	var n int
	err := c.db.QueryRow(`SELECT count(*) FROM queue`).Scan(&n)
	return n, err
}
func (c *Client) watchdogLoop(ctx context.Context) {
	interval := 30 * time.Second
	if wd := os.Getenv("WATCHDOG_USEC"); wd != "" {
		if usec, err := strconv.ParseInt(wd, 10, 64); err == nil && usec > 0 {
			interval = time.Duration(usec) * time.Microsecond / 3
		}
	}
	notify("READY=1")
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			notify("STOPPING=1")
			return
		case <-t.C:
			notify("WATCHDOG=1")
		}
	}
}
func notify(message string) {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return
	}
	if strings.HasPrefix(socket, "@") {
		socket = "\x00" + socket[1:]
	}
	addr := &net.UnixAddr{Name: socket, Net: "unixgram"}
	conn, err := net.DialUnix("unixgram", nil, addr)
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = conn.Write([]byte(message))
}

type limitedBuffer struct {
	bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.max {
		return 0, errors.New("output limit exceeded")
	}
	return b.Buffer.Write(p)
}
func strictJSON(data []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}
func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 8 {
		attempt = 8
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second
}
func deterministicJitter(key string, max time.Duration) time.Duration {
	sum := sha256.Sum256([]byte(key))
	n := int64(sum[0])<<8 | int64(sum[1])
	return time.Duration(n % int64(max))
}
func sleepContext(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
