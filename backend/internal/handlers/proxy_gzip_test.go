package handlers

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"api-key-rotator/backend/internal/config"
	"api-key-rotator/backend/internal/infrastructure/cache/memory"
	"api-key-rotator/backend/internal/models"
	"api-key-rotator/backend/internal/services"

	"github.com/gin-gonic/gin"
)

func gzipHandlerTestBody(t *testing.T, body []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(body); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return compressed.Bytes()
}

// TestGenericProxyRetryableGzipResponseKeepsBodyReadable covers H4/gzip
// buffering: a client that negotiated gzip receives a compressed 429. The
// retry buffer must either decompress it or retain Content-Encoding; it must
// never emit gzip bytes as plain text.
func TestGenericProxyRetryableGzipResponseKeepsBodyReadable(t *testing.T) {
	gin.SetMode(gin.TestMode)

	compressed := gzipHandlerTestBody(t, []byte(`{"error":{"message":"quota exhausted"}}`))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write(compressed)
	}))
	defer upstream.Close()

	handler := &ProxyHandler{
		cfg: &config.Config{
			GlobalProxyKeys:    "proxy-secret",
			ProxyTimeout:       5,
			KeyCooldownSeconds: config.DefaultKeyCooldownSeconds,
			UpstreamUserAgent:  config.DefaultUpstreamUserAgent,
			WAFRetryUserAgent:  config.DefaultWAFRetryUserAgent,
			UpstreamWAFRetry:   false,
		},
		cacheClient: memory.NewMemoryCache(),
	}
	proxyConfig := &models.ProxyConfig{
		ID:             1,
		Name:           "gzip-test",
		Slug:           "gzip-test",
		ConfigType:     "GENERIC",
		IsActive:       true,
		Method:         stringPtr(http.MethodGet),
		TargetURL:      stringPtr(upstream.URL + "/resource"),
		APIKeyName:     stringPtr("X-Api-Key"),
		APIKeyLocation: stringPtr("header"),
		APIKeys: []models.APIKey{
			{ID: 1, KeyValue: "upstream-key", IsActive: true},
		},
	}
	target := &services.TargetRequest{
		Method: http.MethodGet,
		URL:    upstream.URL + "/resource",
		Headers: map[string]string{
			// Force the transport not to transparently decompress: a real
			// client that sent Accept-Encoding: gzip gets the raw bytes.
			"Accept-Encoding": "gzip",
		},
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/proxy/gzip-test/resource", nil)
	c.Set("fullPath", "gzip-test")

	if err := handler.forwardRequest(c, target, proxyConfig); err != nil {
		t.Fatalf("forwardRequest: %v", err)
	}

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body: %s)", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty after decompression", got)
	}
	if got := recorder.Body.String(); got != `{"error":{"message":"quota exhausted"}}` {
		t.Fatalf("body = %q, want the decompressed JSON error", got)
	}
}

func TestLLMResponseGzipRemovesContentEncoding(t *testing.T) {
	gin.SetMode(gin.TestMode)

	want := []byte(`{"ok":true}`)
	compressed := gzipHandlerTestBody(t, want)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":     []string{"application/json"},
			"Content-Encoding": []string{"gzip"},
		},
		Body: io.NopCloser(bytes.NewReader(compressed)),
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	handler := &LLMProxyHandler{}
	if err := handler.writeLLMResponse(c, resp, nil, false); err != nil {
		t.Fatalf("writeLLMResponse: %v", err)
	}

	if got := recorder.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty after decompression", got)
	}
	if !bytes.Equal(recorder.Body.Bytes(), want) {
		t.Fatalf("body = %q, want %q", recorder.Body.Bytes(), want)
	}
}
