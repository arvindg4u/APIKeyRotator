package handlers

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"api-key-rotator/backend/internal/config"
	"api-key-rotator/backend/internal/infrastructure/cache/memory"
	"api-key-rotator/backend/internal/models"

	"github.com/gin-gonic/gin"
)

func closedListenerURL(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return "http://" + address
}

// TestLLMProxyMapsAllNetworkErrorsToBadGateway covers the network-error status
// mapping finding: when every key attempt fails at the transport layer, the
// client must receive 502, not a misleading "all keys are rate limited" 429.
func TestLLMProxyMapsAllNetworkErrorsToBadGateway(t *testing.T) {
	gin.SetMode(gin.TestMode)

	repo := newCredentialTestRepo(t)
	targetURL := closedListenerURL(t)
	cfgRow := &models.ProxyConfig{
		Name:          "network-error",
		Slug:          "network-error",
		ConfigType:    "LLM",
		IsActive:      true,
		TargetBaseURL: stringPtr(targetURL),
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
		GlobalProxyKeys:    "proxy-secret",
		ProxyTimeout:       2,
		KeyCooldownSeconds: config.DefaultKeyCooldownSeconds,
		UpstreamUserAgent:  config.DefaultUpstreamUserAgent,
		WAFRetryUserAgent:  config.DefaultWAFRetryUserAgent,
		UpstreamWAFRetry:   false,
	}, repo.GetDB(), memory.NewMemoryCache())

	router := gin.New()
	router.Any("/llm/:slug/*action", handler.HandleLLMProxy)

	req := httptest.NewRequest(http.MethodPost, "/llm/network-error/v1/chat/completions",
		strings.NewReader(`{"model":"test"}`))
	req.Header.Set("Authorization", "Bearer proxy-secret")
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body: %s)", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "All API keys are rate limited") {
		t.Fatalf("network failure was reported as a rate limit: %s", recorder.Body.String())
	}
}

// TestGenericProxyMapsAllNetworkErrorsToBadGateway is the same regression for
// the generic /proxy path, which has its own retry loop.
func TestGenericProxyMapsAllNetworkErrorsToBadGateway(t *testing.T) {
	gin.SetMode(gin.TestMode)

	repo := newCredentialTestRepo(t)
	targetURL := closedListenerURL(t)
	keyName := "X-Api-Key"
	keyLocation := "header"
	cfgRow := &models.ProxyConfig{
		Name:           "generic-network-error",
		Slug:           "generic-network-error",
		ConfigType:     "GENERIC",
		IsActive:       true,
		Method:         stringPtr(http.MethodGet),
		TargetURL:      stringPtr(targetURL + "/resource"),
		APIKeyName:     &keyName,
		APIKeyLocation: &keyLocation,
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

	handler := newCredentialTestHandler(t, repo)
	router := gin.New()
	router.Any("/proxy/*slug", handler.HandleGenericProxy)

	req := httptest.NewRequest(http.MethodGet, "/proxy/generic-network-error/resource", nil)
	req.Header.Set("X-Proxy-Key", "proxy-secret")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body: %s)", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "All API keys are rate limited") {
		t.Fatalf("network failure was reported as a rate limit: %s", recorder.Body.String())
	}
}
