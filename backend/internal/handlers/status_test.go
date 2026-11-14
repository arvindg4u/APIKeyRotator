package handlers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"api-key-rotator/backend/internal/config"
	"api-key-rotator/backend/internal/infrastructure/cache/memory"
	"api-key-rotator/backend/internal/middleware"
	"api-key-rotator/backend/internal/models"
	"api-key-rotator/backend/internal/services"
	"api-key-rotator/backend/internal/telemetry"

	"github.com/gin-gonic/gin"
)

func newStatusTestConfig() *config.Config {
	return &config.Config{
		GlobalProxyKeys:            "proxy-secret-key",
		KeyCooldownSeconds:         config.DefaultKeyCooldownSeconds,
		KeyMaxCooldownSeconds:      config.DefaultMaxKeyCooldownSeconds,
		FreeTierCooldownSeconds:    config.DefaultFreeTierCooldownSeconds,
		FreeTierMaxCooldownSeconds: config.DefaultFreeTierMaxCooldownSeconds,
		CacheType:                  "memory",
		DBType:                     "sqlite",
	}
}

// newStatusTestHandler builds a handler backed by a temp sqlite DB with one
// LLM config that has two active keys.
func newStatusTestHandler(t *testing.T) (*StatusHandler, *memory.Cache, int32) {
	t.Helper()
	repo := newCredentialTestRepo(t)

	cfgRow := &models.ProxyConfig{
		Name:          "svc",
		Slug:          "svc",
		ConfigType:    "LLM",
		IsActive:      true,
		TargetBaseURL: stringPtr("https://upstream.example/v1"),
		APIFormat:     stringPtr("openai_compatible"),
		OutputFormat:  stringPtr("none"),
	}
	if err := repo.GetDB().Create(cfgRow).Error; err != nil {
		t.Fatalf("create config: %v", err)
	}
	for _, key := range []string{"k-1", "k-2"} {
		if err := repo.GetDB().Create(&models.APIKey{
			ProxyConfigID: cfgRow.ID,
			KeyValue:      key,
			IsActive:      true,
		}).Error; err != nil {
			t.Fatalf("create key: %v", err)
		}
	}

	cacheClient := memory.NewMemoryCache()
	return NewStatusHandler(newStatusTestConfig(), repo.GetDB(), cacheClient, telemetry.NewRecorder(50)), cacheClient, cfgRow.ID
}

func TestStatusOverviewCountsAndCooling(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, cacheClient, configID := newStatusTestHandler(t)

	// Cool one key on the same cache the handler reads from.
	rotator := services.NewKeyRotator(newStatusTestConfig(), cacheClient, "status-test")
	rotator.MarkRateLimited(configID, "k-1", time.Hour)

	router := gin.New()
	router.GET("/status/overview", handler.Overview)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/status/overview", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	var resp overviewResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Configs != 1 || resp.ActiveConfigs != 1 {
		t.Fatalf("config counts = %d/%d", resp.Configs, resp.ActiveConfigs)
	}
	if resp.Keys != 2 || resp.ActiveKeys != 2 {
		t.Fatalf("key counts = %d/%d", resp.Keys, resp.ActiveKeys)
	}
	if resp.CoolingKeys != 1 {
		t.Fatalf("cooling = %d, want 1", resp.CoolingKeys)
	}
	if len(resp.PerConfig) != 1 || resp.PerConfig[0].CoolingKeys != 1 {
		t.Fatalf("per_config = %+v", resp.PerConfig)
	}
	if resp.CacheType != "memory" || resp.DBType != "sqlite" {
		t.Fatalf("runtime fields = %s/%s", resp.CacheType, resp.DBType)
	}
}

func TestStatusTraffic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, _, _ := newStatusTestHandler(t)

	handler.recorder.RecordRequest("svc", "POST", "/llm/svc/v1/chat/completions", 200, 50*time.Millisecond)
	handler.recorder.RecordRequest("svc", "POST", "/llm/svc/v1/chat/completions", 429, 20*time.Millisecond)
	handler.recorder.RecordRequest("other", "GET", "/proxy/other/x", 500, 5*time.Millisecond)

	router := gin.New()
	router.GET("/status/traffic", handler.Traffic)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/status/traffic", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	var resp trafficResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.TotalRequests != 3 {
		t.Fatalf("total = %d, want 3", resp.TotalRequests)
	}
	if resp.StatusClasses["2xx"] != 1 || resp.StatusClasses["4xx"] != 1 || resp.StatusClasses["5xx"] != 1 {
		t.Fatalf("classes = %v", resp.StatusClasses)
	}
	var found bool
	for _, slug := range resp.PerSlug {
		if slug.Slug == "svc" {
			found = true
			if slug.Requests != 2 || slug.RateLimited != 1 {
				t.Fatalf("svc stat = %+v", slug)
			}
		}
	}
	if !found {
		t.Fatalf("svc not present in %+v", resp.PerSlug)
	}
}

func TestStatusLogsRedactsSecrets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, _, _ := newStatusTestHandler(t)

	handler.recorder.AppendLog("[INFO] upstream call used proxy-secret-key as required")

	router := gin.New()
	router.GET("/status/logs", handler.Logs)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/status/logs", nil))

	body := recorder.Body.String()
	if strings.Contains(body, "proxy-secret-key") {
		t.Fatalf("secret leaked in logs response: %s", body)
	}
	if !strings.Contains(body, "[REDACTED]") {
		t.Fatalf("expected redaction marker, got: %s", body)
	}
}

func TestWriteSSEFrame(t *testing.T) {
	var buf bytes.Buffer
	if err := writeSSEFrame(&buf, 7, "event", []byte(`{"a":1}`)); err != nil {
		t.Fatalf("writeSSEFrame: %v", err)
	}
	want := "id: 7\nevent: event\ndata: {\"a\":1}\n\n"
	if buf.String() != want {
		t.Fatalf("frame = %q, want %q", buf.String(), want)
	}
}

func TestStatusRoutesRequireAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, _, _ := newStatusTestHandler(t)

	router := gin.New()
	group := router.Group("/admin", middleware.RequireAdmin("test-secret"))
	group.GET("/status/overview", handler.Overview)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/status/overview", nil))

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
}

func TestStatusStreamReplaysAndStreamsLive(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, _, _ := newStatusTestHandler(t)

	handler.recorder.RecordEvent(telemetry.Event{Kind: "test", Message: "replay-me"})

	router := gin.New()
	router.GET("/status/stream", handler.Stream)
	server := httptest.NewServer(router)
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/status/stream", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	if buffering := resp.Header.Get("X-Accel-Buffering"); buffering != "no" {
		t.Fatalf("X-Accel-Buffering = %q, want no", buffering)
	}

	lines := make(chan string, 128)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()

	if !waitForSubstring(lines, "replay-me", 3*time.Second) {
		t.Fatal("did not receive the replayed event")
	}

	handler.recorder.RecordEvent(telemetry.Event{Kind: "test", Message: "live-event"})
	if !waitForSubstring(lines, "live-event", 3*time.Second) {
		t.Fatal("did not receive the live event")
	}
}

func waitForSubstring(lines <-chan string, substr string, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return false
			}
			if strings.Contains(line, substr) {
				return true
			}
		case <-deadline:
			return false
		}
	}
}
