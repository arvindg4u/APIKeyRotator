package handlers

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"api-key-rotator/backend/internal/config"
	"api-key-rotator/backend/internal/infrastructure/cache/memory"
	"api-key-rotator/backend/internal/models"
	"api-key-rotator/backend/internal/services"

	"github.com/gin-gonic/gin"
)

// TestGenericProxyRetriesWithFallbackUserAgentAfterWAFBlock 通用代理 (/proxy/:slug) 也要有同样的兜底:
// 客户端 UA 被 WAF 拦掉时, 自动换用兜底 UA 重试一次。
func TestGenericProxyRetriesWithFallbackUserAgentAfterWAFBlock(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var mu sync.Mutex
	var userAgents []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		userAgents = append(userAgents, r.Header.Get("User-Agent"))
		mu.Unlock()

		if r.Header.Get("User-Agent") == "Hermes/1.0" {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("Blocked"))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	handler := &ProxyHandler{
		cfg: &config.Config{
			GlobalProxyKeys:    "proxy-key",
			ProxyTimeout:       5,
			KeyCooldownSeconds: config.DefaultKeyCooldownSeconds,
			UpstreamUserAgent:  config.DefaultUpstreamUserAgent,
			WAFRetryUserAgent:  config.DefaultWAFRetryUserAgent,
			UpstreamWAFRetry:   true,
		},
		cacheClient: memory.NewMemoryCache(),
	}

	proxyConfig := &models.ProxyConfig{
		ID:             2,
		Name:           "generic",
		Slug:           "generic",
		ConfigType:     "GENERIC",
		IsActive:       true,
		Method:         stringPtr("POST"),
		APIKeyName:     stringPtr("X-Api-Key"),
		APIKeyLocation: stringPtr("header"),
		APIKeys: []models.APIKey{
			{ID: 1, KeyValue: "sk-upstream-key", IsActive: true},
		},
	}

	target := &services.TargetRequest{
		Method:  http.MethodPost,
		URL:     upstream.URL + "/v1/resource",
		Headers: map[string]string{"User-Agent": "Hermes/1.0", "Content-Type": "application/json"},
		Body:    []byte(`{"ping":true}`),
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/proxy/generic/v1/resource", nil)
	c.Set("fullPath", "generic/v1/resource")

	if err := handler.forwardRequest(c, target, proxyConfig); err != nil {
		t.Fatalf("forwardRequest: %v", err)
	}

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(userAgents) != 2 {
		t.Fatalf("upstream saw %d request(s) (%v), want 2 (original + fallback UA retry)", len(userAgents), userAgents)
	}
	if userAgents[1] != config.DefaultWAFRetryUserAgent {
		t.Fatalf("second User-Agent = %q, want the fallback %q", userAgents[1], config.DefaultWAFRetryUserAgent)
	}
}
