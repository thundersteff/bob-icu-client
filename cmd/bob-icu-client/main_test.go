package main

import (
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

func TestBuiltinsReturnFiniteNumbers(t *testing.T) {
	for _, name := range []string{"host.uptime_seconds", "host.load1", "host.memory_available_percent", "host.root_disk_used_percent"} {
		result, err := runBuiltin(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := normalizeValue("number", result.Value); err != nil {
			t.Fatalf("%s: %v", name, err)
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
