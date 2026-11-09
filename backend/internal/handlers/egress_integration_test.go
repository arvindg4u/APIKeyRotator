package handlers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"api-key-rotator/backend/internal/config"
	"api-key-rotator/backend/internal/infrastructure/cache/memory"
	"api-key-rotator/backend/internal/models"

	"github.com/gin-gonic/gin"
)

// TestNewUpstreamHTTPClientIgnoresEnvProxy documents that the egress layer is
// the only thing allowed to choose an upstream proxy. A stray HTTP_PROXY in
// the environment must not silently reroute production traffic.
func TestNewUpstreamHTTPClientIgnoresEnvProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:9")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")

	client := newUpstreamHTTPClient(5, nil)
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("transport.Proxy is set; want nil when egress is disabled")
	}
}

// TestGenericProxyUsesEgressEndpoint proves the request path goes through the
// selected HTTP CONNECT/absolute-form proxy instead of connecting directly.
func TestGenericProxyUsesEgressEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var proxyHits, upstreamHits int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&upstreamHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&proxyHits, 1)

		// Absolute-form proxied HTTP request: fetch the target without a proxy.
		outReq, err := http.NewRequest(r.Method, r.URL.String(), r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		for key, values := range r.Header {
			for _, value := range values {
				outReq.Header.Add(key, value)
			}
		}
		client := &http.Client{Transport: &http.Transport{Proxy: nil}}
		resp, err := client.Do(outReq)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for key, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer proxy.Close()

	repo := newCredentialTestRepo(t)
	keyName := "X-Api-Key"
	keyLocation := "header"
	cfgRow := &models.ProxyConfig{
		Name:           "egress-probe",
		Slug:           "egress-probe",
		ConfigType:     "GENERIC",
		IsActive:       true,
		Method:         stringPtr("GET"),
		TargetURL:      stringPtr(upstream.URL),
		APIKeyName:     &keyName,
		APIKeyLocation: &keyLocation,
	}
	if err := repo.GetDB().Create(cfgRow).Error; err != nil {
		t.Fatalf("create config: %v", err)
	}
	if err := repo.GetDB().Create(&models.APIKey{
		ProxyConfigID: cfgRow.ID,
		KeyValue:      "sk-upstream",
		IsActive:      true,
	}).Error; err != nil {
		t.Fatalf("create key: %v", err)
	}

	handler := NewProxyHandler(&config.Config{
		GlobalProxyKeys:       "proxy-secret",
		ProxyTimeout:          5,
		KeyCooldownSeconds:    config.DefaultKeyCooldownSeconds,
		UpstreamUserAgent:     config.DefaultUpstreamUserAgent,
		WAFRetryUserAgent:     config.DefaultWAFRetryUserAgent,
		UpstreamWAFRetry:      false,
		EgressEnabled:         true,
		EgressEndpoints:       proxy.URL,
		EgressStickySeconds:   60,
		EgressCooldownSeconds: 60,
	}, repo.GetDB(), memory.NewMemoryCache())

	router := gin.New()
	router.Any("/proxy/*slug", handler.HandleGenericProxy)

	req := httptest.NewRequest(http.MethodGet, "/proxy/egress-probe", nil)
	req.Header.Set("X-Proxy-Key", "proxy-secret")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", recorder.Code, recorder.Body.String())
	}
	if got := atomic.LoadInt64(&proxyHits); got != 1 {
		t.Fatalf("proxy hits = %d, want 1", got)
	}
	if got := atomic.LoadInt64(&upstreamHits); got != 1 {
		t.Fatalf("upstream hits = %d, want 1", got)
	}
	if !strings.Contains(recorder.Body.String(), `"ok":true`) {
		t.Fatalf("body = %q, want upstream payload", recorder.Body.String())
	}
}

// TestGenericProxyReturnsBadGatewayWhenEgressIsDown makes sure a dead egress
// endpoint is treated as a network failure, not as a silent direct request.
func TestGenericProxyReturnsBadGatewayWhenEgressIsDown(t *testing.T) {
	gin.SetMode(gin.TestMode)

	repo := newCredentialTestRepo(t)
	keyName := "X-Api-Key"
	keyLocation := "header"
	cfgRow := &models.ProxyConfig{
		Name:           "egress-dead",
		Slug:           "egress-dead",
		ConfigType:     "GENERIC",
		IsActive:       true,
		Method:         stringPtr("GET"),
		TargetURL:      stringPtr("http://127.0.0.1:1/resource"),
		APIKeyName:     &keyName,
		APIKeyLocation: &keyLocation,
	}
	if err := repo.GetDB().Create(cfgRow).Error; err != nil {
		t.Fatalf("create config: %v", err)
	}
	if err := repo.GetDB().Create(&models.APIKey{
		ProxyConfigID: cfgRow.ID,
		KeyValue:      "sk-upstream",
		IsActive:      true,
	}).Error; err != nil {
		t.Fatalf("create key: %v", err)
	}

	handler := NewProxyHandler(&config.Config{
		GlobalProxyKeys:       "proxy-secret",
		ProxyTimeout:          2,
		KeyCooldownSeconds:    config.DefaultKeyCooldownSeconds,
		UpstreamUserAgent:     config.DefaultUpstreamUserAgent,
		WAFRetryUserAgent:     config.DefaultWAFRetryUserAgent,
		UpstreamWAFRetry:      false,
		EgressEnabled:         true,
		EgressEndpoints:       "http://127.0.0.1:1",
		EgressStickySeconds:   60,
		EgressCooldownSeconds: 60,
	}, repo.GetDB(), memory.NewMemoryCache())

	router := gin.New()
	router.Any("/proxy/*slug", handler.HandleGenericProxy)

	req := httptest.NewRequest(http.MethodGet, "/proxy/egress-dead", nil)
	req.Header.Set("X-Proxy-Key", "proxy-secret")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body: %s)", recorder.Code, recorder.Body.String())
	}
}
