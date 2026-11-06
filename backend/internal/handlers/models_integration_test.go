package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"api-key-rotator/backend/internal/config"
	"api-key-rotator/backend/internal/infrastructure/cache/memory"
	"api-key-rotator/backend/internal/models"

	"github.com/gin-gonic/gin"
)

// TestListConfigModelsFetchesFromUpstreamWithInjectedKey covers the happy path:
// the handler calls the provider's /models endpoint on the configured base URL
// and injects the pooled upstream key (Bearer for OpenAI-compatible).
func TestListConfigModelsFetchesFromUpstreamWithInjectedKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var mu sync.Mutex
	var seenPath, seenAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o"},{"id":"o3-mini","context_length":200000}]}`))
	}))
	defer upstream.Close()

	repo := newCredentialTestRepo(t)
	proxyConfig := &models.ProxyConfig{
		Name:          "openai-prod",
		Slug:          "openai-prod",
		ConfigType:    "LLM",
		IsActive:      true,
		TargetBaseURL: strptr(upstream.URL + "/v1"),
		APIFormat:     strptr("openai_compatible"),
	}
	if err := repo.CreateProxyConfig(proxyConfig); err != nil {
		t.Fatalf("create proxy config: %v", err)
	}
	if err := repo.CreateAPIKey(&models.APIKey{KeyValue: "sk-upstream", IsActive: true, ProxyConfigID: proxyConfig.ID}); err != nil {
		t.Fatalf("create api key: %v", err)
	}

	handler := NewManagementHandler(testModelConfig(), repo, memory.NewMemoryCache())
	engine := gin.New()
	engine.GET("/admin/proxy-configs/:id/models", handler.ListConfigModels)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/proxy-configs/"+strconv.Itoa(int(proxyConfig.ID))+"/models", nil)
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if seenPath != "/v1/models" {
		t.Errorf("upstream path = %q, want /v1/models", seenPath)
	}
	if seenAuth != "Bearer sk-upstream" {
		t.Errorf("upstream Authorization = %q, want %q", seenAuth, "Bearer sk-upstream")
	}

	var got []ModelInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d models, want 2", len(got))
	}
	if got[0].ID != "gpt-4o" || got[0].ContextWindow != nil {
		t.Errorf("first model = %#v, want gpt-4o with nil context window", got[0])
	}
	if got[1].ID != "o3-mini" || got[1].ContextWindow == nil || *got[1].ContextWindow != 200000 {
		t.Errorf("second model = %#v, want o3-mini with context window 200000", got[1])
	}
}

// TestListConfigModelsRotatesPastRejectedKey verifies the handler falls through
// to the next pooled key when the provider rejects the first one.
func TestListConfigModelsRotatesPastRejectedKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var mu sync.Mutex
	var seenAuth []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenAuth = append(seenAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") == "Bearer bad-key" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid key"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"gemini-flash"}]}`))
	}))
	defer upstream.Close()

	repo := newCredentialTestRepo(t)
	proxyConfig := &models.ProxyConfig{
		Name:          "rotating",
		Slug:          "rotating",
		ConfigType:    "LLM",
		IsActive:      true,
		TargetBaseURL: strptr(upstream.URL + "/v1beta"),
		APIFormat:     strptr("openai_compatible"),
	}
	if err := repo.CreateProxyConfig(proxyConfig); err != nil {
		t.Fatalf("create proxy config: %v", err)
	}
	for _, key := range []string{"bad-key", "good-key"} {
		if err := repo.CreateAPIKey(&models.APIKey{KeyValue: key, IsActive: true, ProxyConfigID: proxyConfig.ID}); err != nil {
			t.Fatalf("create api key: %v", err)
		}
	}

	handler := NewManagementHandler(testModelConfig(), repo, memory.NewMemoryCache())
	engine := gin.New()
	engine.GET("/admin/proxy-configs/:id/models", handler.ListConfigModels)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/proxy-configs/"+strconv.Itoa(int(proxyConfig.ID))+"/models", nil)
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	attempts := append([]string(nil), seenAuth...)
	mu.Unlock()
	if len(attempts) != 2 {
		t.Fatalf("upstream attempts = %v, want 2", attempts)
	}
	if attempts[0] != "Bearer bad-key" || attempts[1] != "Bearer good-key" {
		t.Errorf("upstream attempts = %v, want [Bearer bad-key, Bearer good-key]", attempts)
	}
}

func testModelConfig() *config.Config {
	return &config.Config{
		ProxyTimeout:       5,
		KeyCooldownSeconds: config.DefaultKeyCooldownSeconds,
		UpstreamUserAgent:  config.DefaultUpstreamUserAgent,
	}
}
