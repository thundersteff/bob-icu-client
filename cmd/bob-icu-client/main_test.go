package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thundersteff/bob-icu-client/internal/cronstate"
)

func validTestConfig(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	secret := strings.Repeat("s", 32)
	secretPath := filepath.Join(dir, "secret")
	if err := os.WriteFile(secretPath, []byte(secret+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Endpoint:      "https://monitor.example",
		SourceID:      "test-host",
		SecretFile:    secretPath,
		StateDatabase: filepath.Join(dir, "state", "client.sqlite3"),
		Services: []ServiceConfig{{
			ServiceID: "host", DisplayName: "Host",
			Sensors: []SensorConfig{{
				SensorID: "uptime", DisplayName: "Uptime", ValueType: "number",
				IntervalSeconds: 60, Builtin: "host.uptime_seconds",
			}},
		}},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "client.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path, secret
}

func TestLoadConfigAppliesAndPersistsDefaults(t *testing.T) {
	path, secret := validTestConfig(t)
	cfg, gotSecret, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if gotSecret != secret || cfg.HeartbeatIntervalSeconds != 60 || cfg.RequestTimeoutSeconds != 20 {
		t.Fatalf("defaults or secret mismatch: %#v", cfg)
	}
	if cfg.Services[0].Sensors[0].TimeoutSeconds != 10 {
		t.Fatalf("sensor timeout default not persisted: %#v", cfg.Services[0].Sensors[0])
	}
}

func TestLoadConfigRejectsUnknownField(t *testing.T) {
	path, _ := validTestConfig(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data[:len(data)-1], []byte(`,"surprise":true}`)...)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadConfig(path); err == nil {
		t.Fatal("unknown field was accepted")
	}
}

func TestLoadConfigRejectsServiceLimit(t *testing.T) {
	path, _ := validTestConfig(t)
	var cfg Config
	if err := decodeFile(path, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Services = nil
	for i := 0; i < 65; i++ {
		cfg.Services = append(cfg.Services, ServiceConfig{ServiceID: "s" + strconv.Itoa(i), DisplayName: "Service"})
	}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), "service limit") {
		t.Fatalf("expected service limit error, got %v", err)
	}
}

func TestStandardPackagesExpandWithStableCatalog(t *testing.T) {
	path, _ := validTestConfig(t)
	var cfg Config
	if err := decodeFile(path, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Packages = []string{"linux.base.v1", "linux.systemd.v1"}
	cfg.Services = nil
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Services) != 3 {
		t.Fatalf("services=%d, want 3", len(loaded.Services))
	}
	wantCounts := map[string]int{"linux_system": 8, "linux_storage": 4, "linux_services": 5}
	for _, svc := range loaded.Services {
		if len(svc.Sensors) != wantCounts[svc.ServiceID] {
			t.Fatalf("%s sensors=%d, want %d", svc.ServiceID, len(svc.Sensors), wantCounts[svc.ServiceID])
		}
	}
}

func TestStandardPackagesRejectUnknownDuplicateAndConflictingEntries(t *testing.T) {
	tests := []struct {
		name     string
		packages []string
		services []ServiceConfig
		contains string
	}{
		{name: "unknown", packages: []string{"linux.future.v9"}, contains: "unknown package"},
		{name: "duplicate", packages: []string{"linux.base.v1", "linux.base.v1"}, contains: "duplicate package"},
		{name: "service conflict", packages: []string{"linux.base.v1"}, services: []ServiceConfig{{ServiceID: "linux_system", DisplayName: "Custom"}}, contains: "conflicts with service"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, _ := validTestConfig(t)
			var cfg Config
			if err := decodeFile(path, &cfg); err != nil {
				t.Fatal(err)
			}
			cfg.Packages = tt.packages
			cfg.Services = tt.services
			data, _ := json.Marshal(cfg)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), tt.contains) {
				t.Fatalf("expected %q error, got %v", tt.contains, err)
			}
		})
	}
}

func TestListPackages(t *testing.T) {
	var output bytes.Buffer
	if err := printPackages(&output); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"linux.base.v1", "linux.cron.v1", "linux.systemd.v1"} {
		if !strings.Contains(output.String(), id+"\t") {
			t.Fatalf("package %s missing from output: %s", id, output.String())
		}
	}
}

func TestCronPackageExpandsRegisteredJobs(t *testing.T) {
	path, _ := validTestConfig(t)
	var cfg Config
	if err := decodeFile(path, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Packages = []string{"linux.cron.v1"}
	cfg.CronStateDirectory = filepath.Join(t.TempDir(), "cron")
	cfg.CronJobs = []CronJobConfig{{
		JobID: "nightly_backup", DisplayName: "Nächtliche Sicherung",
		ExpectedIntervalSeconds: 86400, GraceSeconds: 1800, MaxRuntimeSeconds: 7200,
	}}
	cfg.Services = nil
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Services) != 1 || loaded.Services[0].ServiceID != "linux_cron_jobs" || len(loaded.Services[0].Sensors) != 1 {
		t.Fatalf("unexpected cron catalog: %#v", loaded.Services)
	}
	sensor := loaded.Services[0].Sensors[0]
	if sensor.SensorID != "nightly_backup" || sensor.Builtin != "linux.cron_job_state" || sensor.IntervalSeconds != 60 {
		t.Fatalf("unexpected cron sensor: %#v", sensor)
	}
	if sensor.BuiltinOptions["expected_interval_seconds"] != "86400" || sensor.BuiltinOptions["max_runtime_seconds"] != "7200" {
		t.Fatalf("unexpected cron options: %#v", sensor.BuiltinOptions)
	}
}

func TestCronPackageRejectsUnsafeDefinitions(t *testing.T) {
	tests := []struct {
		name     string
		packages []string
		jobs     []CronJobConfig
		contains string
	}{
		{name: "jobs without package", jobs: []CronJobConfig{{JobID: "job", DisplayName: "Job", ExpectedIntervalSeconds: 60}}, contains: "require package"},
		{name: "package without jobs", packages: []string{"linux.cron.v1"}, contains: "requires at least one"},
		{name: "unsafe id", packages: []string{"linux.cron.v1"}, jobs: []CronJobConfig{{JobID: "../job", DisplayName: "Job", ExpectedIntervalSeconds: 60}}, contains: "invalid or duplicate"},
		{name: "interval too short", packages: []string{"linux.cron.v1"}, jobs: []CronJobConfig{{JobID: "job", DisplayName: "Job", ExpectedIntervalSeconds: 30}}, contains: "invalid expected interval"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, _ := validTestConfig(t)
			var cfg Config
			if err := decodeFile(path, &cfg); err != nil {
				t.Fatal(err)
			}
			cfg.Packages, cfg.CronJobs = tt.packages, tt.jobs
			cfg.Services = nil
			data, _ := json.Marshal(cfg)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), tt.contains) {
				t.Fatalf("expected %q error, got %v", tt.contains, err)
			}
		})
	}
}

func TestCronJobStateClassification(t *testing.T) {
	directory := t.TempDir()
	options := cronSensorOptions{
		jobID: "backup", stateDirectory: directory,
		expectedInterval: time.Hour, grace: 5 * time.Minute, maximumRuntime: 10 * time.Minute,
	}
	now := time.Date(2026, 9, 21, 8, 0, 0, 0, time.UTC)
	if got := cronJobStateAt(options, now); got.Value != "warning" {
		t.Fatalf("missing state=%#v", got)
	}
	exitZero, duration := 0, int64(2500)
	state := cronstate.State{
		Schema: cronstate.Schema, JobID: "backup", LastStartedAt: now.Add(-30 * time.Minute).Format(time.RFC3339Nano),
		LastFinishedAt: now.Add(-29 * time.Minute).Format(time.RFC3339Nano), LastSuccessAt: now.Add(-29 * time.Minute).Format(time.RFC3339Nano),
		LastExitCode: &exitZero, LastDurationMS: &duration,
	}
	if err := cronstate.WriteAtomic(directory, state); err != nil {
		t.Fatal(err)
	}
	if got := cronJobStateAt(options, now); got.Value != "healthy" || !strings.Contains(got.Message, "Laufzeit") {
		t.Fatalf("healthy state=%#v", got)
	}
	state.LastSuccessAt = now.Add(-2 * time.Hour).Format(time.RFC3339Nano)
	if err := cronstate.WriteAtomic(directory, state); err != nil {
		t.Fatal(err)
	}
	if got := cronJobStateAt(options, now); got.Value != "critical" || !strings.Contains(got.Message, "erwartet") {
		t.Fatalf("overdue state=%#v", got)
	}
	exitSeven := 7
	state.LastFinishedAt = now.Add(-time.Minute).Format(time.RFC3339Nano)
	state.LastExitCode = &exitSeven
	if err := cronstate.WriteAtomic(directory, state); err != nil {
		t.Fatal(err)
	}
	if got := cronJobStateAt(options, now); got.Value != "critical" || !strings.Contains(got.Message, "Exit 7") {
		t.Fatalf("failed state=%#v", got)
	}
	state.Running = true
	state.LastStartedAt = now.Add(-5 * time.Minute).Format(time.RFC3339Nano)
	if err := cronstate.WriteAtomic(directory, state); err != nil {
		t.Fatal(err)
	}
	if got := cronJobStateAt(options, now); got.Value != "healthy" || !strings.Contains(got.Message, "Läuft") {
		t.Fatalf("running state=%#v", got)
	}
	state.LastStartedAt = now.Add(-20 * time.Minute).Format(time.RFC3339Nano)
	if err := cronstate.WriteAtomic(directory, state); err != nil {
		t.Fatal(err)
	}
	if got := cronJobStateAt(options, now); got.Value != "critical" || !strings.Contains(got.Message, "Maximum") {
		t.Fatalf("stuck state=%#v", got)
	}
}

func TestBuiltinsReturnFiniteNumbers(t *testing.T) {
	for _, name := range []string{
		"host.uptime_seconds", "host.load1", "host.memory_available_percent", "host.root_disk_used_percent",
		"linux.uptime_seconds", "linux.cpu_used_percent", "linux.load1", "linux.load5", "linux.load15",
		"linux.memory_used_percent", "linux.memory_available_bytes", "linux.swap_used_percent",
		"linux.root_disk_used_percent", "linux.root_disk_free_bytes", "linux.root_inode_used_percent",
	} {
		result, err := runBuiltin(context.Background(), name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := normalizeValue("number", result.Value); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestRootMountStateBuiltinReturnsText(t *testing.T) {
	result, err := runBuiltin(context.Background(), "linux.root_mount_state")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := normalizeValue("text", result.Value); err != nil {
		t.Fatal(err)
	}
}

func TestRootMountReadOnlyUsesTargetNamespace(t *testing.T) {
	for _, tc := range []struct {
		name, options string
		want          bool
	}{{"rw", "rw,relatime", false}, {"ro", "ro,relatime", true}} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "mountinfo")
			line := "36 25 8:1 / / " + tc.options + " shared:1 - ext4 /dev/root rw\n"
			if err := os.WriteFile(path, []byte(line), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := rootMountReadOnly(path)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestSystemdBuiltinsReturnTypedValuesWhenAvailable(t *testing.T) {
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		t.Skip("systemd is not running")
	}
	tests := []struct {
		name string
		kind string
	}{
		{"linux.systemd_state", "text"},
		{"linux.systemd_failed_units_count", "number"},
		{"linux.cron_daemon_state", "text"},
		{"linux.time_sync_state", "text"},
		{"linux.reboot_required_state", "text"},
	}
	for _, tt := range tests {
		result, err := runBuiltin(context.Background(), tt.name)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if _, err := normalizeValue(tt.kind, result.Value); err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
	}
}

func TestSendSignsExactBody(t *testing.T) {
	secret := strings.Repeat("k", 32)
	fixed := time.Unix(1_800_000_000, 0)
	var checked bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		digest := sha256.Sum256(body)
		canonical := strings.Join([]string{
			"POST", r.URL.EscapedPath(), r.Header.Get("X-Monitor-Timestamp"),
			r.Header.Get("X-Monitor-Event"), hex.EncodeToString(digest[:]),
		}, "\n")
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write([]byte(canonical))
		if !hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(r.Header.Get("X-Monitor-Signature"))) {
			t.Error("invalid signature")
		}
		if r.Header.Get("X-Monitor-Source") != "test-host" || r.Header.Get("X-Monitor-Timestamp") != "1800000000" {
			t.Error("invalid authentication headers")
		}
		checked = true
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	c := &Client{
		cfg: Config{Endpoint: server.URL, SourceID: "test-host"}, secret: secret,
		http: server.Client(), now: func() time.Time { return fixed },
	}
	status, err := c.send(context.Background(), queuedItem{EventID: "event-1", Path: "/v1/heartbeat", Body: []byte(`{"schema":"monitor.v1"}`)})
	if err != nil || status != http.StatusAccepted || !checked {
		t.Fatalf("status=%d checked=%v err=%v", status, checked, err)
	}
}

func TestQueueCoalescesHeartbeat(t *testing.T) {
	path, secret := validTestConfig(t)
	cfg, _, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := newClient(cfg, secret, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.db.Close()
	for i := 1; i <= 2; i++ {
		if err := c.enqueueJSON("/v1/heartbeat", Heartbeat{Schema: protocolSchema, Sequence: int64(i)}, true); err != nil {
			t.Fatal(err)
		}
	}
	if depth, err := c.queueDepth(); err != nil || depth != 1 {
		t.Fatalf("depth=%d err=%v", depth, err)
	}
	item, err := c.nextItem()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(item.Body), `"sequence":2`) {
		t.Fatalf("latest heartbeat not retained: %s", item.Body)
	}
}

func TestOpenClawPackageExpandsAccountsAndModels(t *testing.T) {
	cfg := Config{Packages: []string{"openclaw.standard.v1"}, OpenClawAccounts: []OpenClawAccountConfig{{Channel: "whatsapp", AccountID: "default", DisplayName: "WhatsApp"}}, OpenClawModels: []OpenClawModelConfig{{ModelID: "provider/model", DisplayName: "Primary", ActiveProbe: true}}}
	if err := expandPackages(&cfg); err != nil {
		t.Fatal(err)
	}
	if err := expandOpenClaw(&cfg); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, service := range cfg.Services {
		counts[service.ServiceID] = len(service.Sensors)
	}
	if counts["openclaw_channels"] != 3 || counts["openclaw_llm"] != 4 {
		t.Fatalf("unexpected sensors: %#v", counts)
	}
}
