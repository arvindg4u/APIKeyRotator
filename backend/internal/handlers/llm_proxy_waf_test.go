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

func stringPtr(s string) *string {
	return &s
}

// wafUpstream 模拟一个带 WAF 的上游:
// 客户端 UA 是 "Hermes/1.0" 时返回 403 + 纯文本拦截页，其它 UA 正常放行。
type wafUpstream struct {
	server      *httptest.Server
	mu          sync.Mutex
	userAgents  []string
	authHeaders []string
}

func newWAFUpstream() *wafUpstream {
	upstream := &wafUpstream{}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.mu.Lock()
		upstream.userAgents = append(upstream.userAgents, r.Header.Get("User-Agent"))
		upstream.authHeaders = append(upstream.authHeaders, r.Header.Get("Authorization"))
		upstream.mu.Unlock()

		if r.Header.Get("User-Agent") == "Hermes/1.0" {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("Blocked"))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	return upstream
}

func (u *wafUpstream) close() {
	u.server.Close()
}

func (u *wafUpstream) seenUserAgents() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.userAgents...)
}

func (u *wafUpstream) seenAuthHeaders() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.authHeaders...)
}

func newWAFTestHandler() *LLMProxyHandler {
	return &LLMProxyHandler{
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
}

func newWAFTestProxyConfig() *models.ProxyConfig {
	return &models.ProxyConfig{
		ID:           1,
		Name:         "kilo",
		Slug:         "kilo",
		ConfigType:   "LLM",
		IsActive:     true,
		APIFormat:    stringPtr("openai_compatible"),
		OutputFormat: stringPtr("none"),
		APIKeys: []models.APIKey{
			{ID: 1, KeyValue: "sk-upstream-key", IsActive: true},
		},
	}
}

// TestLLMProxyRetriesWithFallbackUserAgentAfterWAFBlock 覆盖真实故障场景:
// 客户端 (hermes agent) 的 UA 被上游 WAF 拦掉, 代理必须自动换用兜底 UA 重试一次并成功。
func TestLLMProxyRetriesWithFallbackUserAgentAfterWAFBlock(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := newWAFUpstream()
	defer upstream.close()

	handler := newWAFTestHandler()
	proxyConfig := newWAFTestProxyConfig()

	target := &services.TargetRequest{
		Method:  http.MethodPost,
		URL:     upstream.server.URL + "/v1/chat/completions",
		Headers: map[string]string{"User-Agent": "Hermes/1.0", "Content-Type": "application/json"},
		Body:    []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`),
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/llm/kilo/chat/completions", nil)

	if err := handler.forwardLLMRequest(c, target, proxyConfig); err != nil {
		t.Fatalf("forwardLLMRequest: %v", err)
	}

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	userAgents := upstream.seenUserAgents()
	if len(userAgents) != 2 {
		t.Fatalf("upstream saw %d request(s) (%v), want 2 (original + fallback UA retry)", len(userAgents), userAgents)
	}
	if userAgents[0] != "Hermes/1.0" {
		t.Fatalf("first User-Agent = %q, want the client's %q", userAgents[0], "Hermes/1.0")
	}
	if userAgents[1] != config.DefaultWAFRetryUserAgent {
		t.Fatalf("second User-Agent = %q, want the fallback %q", userAgents[1], config.DefaultWAFRetryUserAgent)
	}

	// 重试必须用同一个 Key (WAF 拦截不是 Key 的问题)
	authHeaders := upstream.seenAuthHeaders()
	if len(authHeaders) != 2 || authHeaders[0] != authHeaders[1] {
		t.Fatalf("Authorization headers = %v, want the same upstream key on both attempts", authHeaders)
	}
	if authHeaders[0] != "Bearer sk-upstream-key" {
		t.Fatalf("Authorization = %q, want %q", authHeaders[0], "Bearer sk-upstream-key")
	}
}

// TestLLMProxyWAFRetryDisabled 确认关闭重试后行为与之前一致: 直接把 403 写回客户端。
func TestLLMProxyWAFRetryDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := newWAFUpstream()
	defer upstream.close()

	handler := newWAFTestHandler()
	handler.cfg.UpstreamWAFRetry = false
	proxyConfig := newWAFTestProxyConfig()

	target := &services.TargetRequest{
		Method:  http.MethodPost,
		URL:     upstream.server.URL + "/v1/chat/completions",
		Headers: map[string]string{"User-Agent": "Hermes/1.0", "Content-Type": "application/json"},
		Body:    []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`),
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/llm/kilo/chat/completions", nil)

	if err := handler.forwardLLMRequest(c, target, proxyConfig); err != nil {
		t.Fatalf("forwardLLMRequest: %v", err)
	}

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusForbidden)
	}
	if userAgents := upstream.seenUserAgents(); len(userAgents) != 1 {
		t.Fatalf("upstream saw %d request(s) (%v), want 1 (no fallback UA retry)", len(userAgents), userAgents)
	}
}

// TestLLMProxyWAFBlockStillRotatesKeys 上游持续拦截时, 兜底 UA 重试之后仍会正常轮换 Key,
// 并把上游的 403 原样写回客户端。
func TestLLMProxyWAFBlockStillRotatesKeys(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var mu sync.Mutex
	var seen int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("Blocked"))
	}))
	defer upstream.Close()

	handler := newWAFTestHandler()
	proxyConfig := newWAFTestProxyConfig()
	proxyConfig.APIKeys = append(proxyConfig.APIKeys, models.APIKey{ID: 2, KeyValue: "sk-second-key", IsActive: true})

	target := &services.TargetRequest{
		Method:  http.MethodPost,
		URL:     upstream.URL + "/v1/chat/completions",
		Headers: map[string]string{"User-Agent": "Hermes/1.0", "Content-Type": "application/json"},
		Body:    []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`),
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/llm/kilo/chat/completions", nil)

	if err := handler.forwardLLMRequest(c, target, proxyConfig); err != nil {
		t.Fatalf("forwardLLMRequest: %v", err)
	}

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusForbidden)
	}
	if recorder.Body.String() != "Blocked" {
		t.Fatalf("body = %q, want the upstream block page %q", recorder.Body.String(), "Blocked")
	}

	mu.Lock()
	defer mu.Unlock()
	// 兜底 UA 只重试一次 (拦截与 Key 无关):
	// key1 原始 UA + key1 兜底 UA 重试 + key2 (已经是兜底 UA) = 3 次
	if seen != 3 {
		t.Fatalf("upstream saw %d request(s), want 3 (key1 + fallback UA retry + key2)", seen)
	}
}
