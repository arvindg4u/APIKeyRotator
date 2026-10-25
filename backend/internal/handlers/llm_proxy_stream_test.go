package handlers

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"api-key-rotator/backend/internal/config"
	"api-key-rotator/backend/internal/converters"
	"api-key-rotator/backend/internal/infrastructure/cache/memory"
	"api-key-rotator/backend/internal/infrastructure/database/sqlite"
	"api-key-rotator/backend/internal/models"

	"github.com/gin-gonic/gin"
)

// TestLLMStreamNotTruncatedByProxyTimeout is the regression test for H2:
// PROXY_TIMEOUT used to be assigned to http.Client.Timeout, which is a total
// deadline. A healthy SSE stream lasting longer than that was cut mid-flight.
// It must only bound how long we wait for response headers.
func TestLLMStreamNotTruncatedByProxyTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-key" {
			t.Errorf("upstream Authorization = %q, want the injected key", got)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		chunks := []string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"chunk-1\"}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"content\":\"chunk-2\"}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"content\":\"chunk-3\"}}]}\n\n",
			"data: [DONE]\n\n",
		}
		for i, chunk := range chunks {
			if i > 0 {
				// Total stream time is deliberately > the 1s PROXY_TIMEOUT.
				time.Sleep(600 * time.Millisecond)
			}
			_, _ = io.WriteString(w, chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	repo, err := sqlite.NewSQLiteRepository(t.TempDir() + "/stream.db")
	if err != nil {
		t.Fatalf("sqlite repo: %v", err)
	}
	if err := repo.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfgRow := &models.ProxyConfig{
		Name:          "stream-test",
		Slug:          "stream-test",
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
		GlobalProxyKeys:    "proxy-secret",
		ProxyTimeout:       1,
		KeyCooldownSeconds: config.DefaultKeyCooldownSeconds,
		UpstreamUserAgent:  config.DefaultUpstreamUserAgent,
		WAFRetryUserAgent:  config.DefaultWAFRetryUserAgent,
		UpstreamWAFRetry:   false,
	}, repo.GetDB(), memory.NewMemoryCache())

	router := gin.New()
	router.Any("/llm/:slug/*action", handler.HandleLLMProxy)

	// A real HTTP server is required: gin's Context.Stream uses CloseNotify,
	// which httptest.ResponseRecorder does not implement.
	proxyServer := httptest.NewServer(router)
	defer proxyServer.Close()

	req, err := http.NewRequest(http.MethodPost, proxyServer.URL+"/llm/stream-test/v1/chat/completions",
		strings.NewReader(`{"model":"test","stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer proxy-secret")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	body := string(bodyBytes)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, body)
	}
	for _, want := range []string{"chunk-1", "chunk-2", "chunk-3", "[DONE]"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream is missing %q; body may have been truncated: %q", want, body)
		}
	}
}

type closeNotifierRecorder struct {
	*httptest.ResponseRecorder
	notify chan bool
}

func (r *closeNotifierRecorder) CloseNotify() <-chan bool { return r.notify }

type failingReader struct {
	data []byte
	err  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	return 0, r.err
}

func TestConvertedStreamPropagatesReadError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	converter, err := converters.NewConverter("openai_compatible", "anthropic_native")
	if err != nil {
		t.Fatalf("new converter: %v", err)
	}

	recorder := &closeNotifierRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		notify:           make(chan bool),
	}
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/llm/test/v1/messages", nil)

	handler := &LLMProxyHandler{}
	readErr := errors.New("upstream connection reset")
	got := handler.forwardStreamWithConversion(c, &failingReader{
		data: []byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"),
		err:  readErr,
	}, converter)

	if !errors.Is(got, readErr) {
		t.Fatalf("forwardStreamWithConversion() error = %v, want %v", got, readErr)
	}
}
