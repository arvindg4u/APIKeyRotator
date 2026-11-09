package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"api-key-rotator/backend/internal/config"
	"api-key-rotator/backend/internal/infrastructure/cache/memory"
	"api-key-rotator/backend/internal/models"

	"github.com/gin-gonic/gin"
)

// TestLLMProxyFallsBackFromFreeModel covers the tokenharbor free_tier_limit_reached
// flow: the same account still has paid access, so an optional one-shot fallback
// rewrites "model: x:free" to "model: x" and retries the request.
func TestLLMProxyFallsBackFromFreeModel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var mu sync.Mutex
	var modelsSeen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &payload)
		mu.Lock()
		modelsSeen = append(modelsSeen, payload.Model)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(payload.Model, ":free") {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"Your free allowance for ':free' models is used up. It comes back on 13 Nov 2026 at 08:59 UTC.","type":"free_tier_limit_reached","code":"free_tier_limit_reached"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	repo := newCredentialTestRepo(t)
	cfgRow := &models.ProxyConfig{
		Name:          "free-fallback",
		Slug:          "free-fallback",
		ConfigType:    "LLM",
		IsActive:      true,
		TargetBaseURL: stringPtr(upstream.URL + "/v1"),
		APIFormat:     stringPtr("openai_compatible"),
		OutputFormat:  stringPtr("none"),
	}
	if err := repo.GetDB().Create(cfgRow).Error; err != nil {
		t.Fatalf("create config: %v", err)
	}
	if err := repo.GetDB().Create(&models.APIKey{
		ProxyConfigID: cfgRow.ID,
		KeyValue:      "upstream-key",
		IsActive:      true,
	}).Error; err != nil {
		t.Fatalf("create key: %v", err)
	}

	handler := NewLLMProxyHandler(&config.Config{
		GlobalProxyKeys:            "proxy-secret",
		ProxyTimeout:               5,
		KeyCooldownSeconds:         config.DefaultKeyCooldownSeconds,
		KeyMaxCooldownSeconds:      config.DefaultMaxKeyCooldownSeconds,
		FreeTierCooldownSeconds:    config.DefaultFreeTierCooldownSeconds,
		FreeTierMaxCooldownSeconds: config.DefaultFreeTierMaxCooldownSeconds,
		FreeModelFallback:          true,
		UpstreamUserAgent:          config.DefaultUpstreamUserAgent,
		WAFRetryUserAgent:          config.DefaultWAFRetryUserAgent,
		UpstreamWAFRetry:           false,
	}, repo.GetDB(), memory.NewMemoryCache())

	router := gin.New()
	router.Any("/llm/:slug/*action", handler.HandleLLMProxy)

	req := httptest.NewRequest(http.MethodPost, "/llm/free-fallback/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4.1-flash:free","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer proxy-secret")
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", recorder.Code, recorder.Body.String())
	}

	mu.Lock()
	seen := append([]string(nil), modelsSeen...)
	mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("upstream saw %v, want at least a free attempt and a paid retry", seen)
	}
	if seen[0] != "deepseek-v4.1-flash:free" {
		t.Fatalf("first model = %q, want the free variant", seen[0])
	}
	if seen[len(seen)-1] != "deepseek-v4.1-flash" {
		t.Fatalf("last model = %q, want the paid fallback", seen[len(seen)-1])
	}
}

// TestLLMProxyFreeModelFallbackDisabled verifies the default is still strict:
// without FREE_MODEL_FALLBACK the 429 is returned instead of spending balance.
func TestLLMProxyFreeModelFallbackDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"free allowance used","type":"free_tier_limit_reached","code":"free_tier_limit_reached"}}`))
	}))
	defer upstream.Close()

	repo := newCredentialTestRepo(t)
	cfgRow := &models.ProxyConfig{
		Name:          "free-no-fallback",
		Slug:          "free-no-fallback",
		ConfigType:    "LLM",
		IsActive:      true,
		TargetBaseURL: stringPtr(upstream.URL + "/v1"),
		APIFormat:     stringPtr("openai_compatible"),
		OutputFormat:  stringPtr("none"),
	}
	if err := repo.GetDB().Create(cfgRow).Error; err != nil {
		t.Fatalf("create config: %v", err)
	}
	if err := repo.GetDB().Create(&models.APIKey{
		ProxyConfigID: cfgRow.ID,
		KeyValue:      "upstream-key",
		IsActive:      true,
	}).Error; err != nil {
		t.Fatalf("create key: %v", err)
	}

	handler := NewLLMProxyHandler(&config.Config{
		GlobalProxyKeys:            "proxy-secret",
		ProxyTimeout:               5,
		KeyCooldownSeconds:         config.DefaultKeyCooldownSeconds,
		FreeTierCooldownSeconds:    config.DefaultFreeTierCooldownSeconds,
		FreeTierMaxCooldownSeconds: config.DefaultFreeTierMaxCooldownSeconds,
		FreeModelFallback:          false,
		UpstreamUserAgent:          config.DefaultUpstreamUserAgent,
		WAFRetryUserAgent:          config.DefaultWAFRetryUserAgent,
		UpstreamWAFRetry:           false,
	}, repo.GetDB(), memory.NewMemoryCache())

	router := gin.New()
	router.Any("/llm/:slug/*action", handler.HandleLLMProxy)

	req := httptest.NewRequest(http.MethodPost, "/llm/free-no-fallback/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4.1-flash:free","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer proxy-secret")
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "free_tier_limit_reached") {
		t.Fatalf("body = %s, want the upstream free-tier error", recorder.Body.String())
	}
}
