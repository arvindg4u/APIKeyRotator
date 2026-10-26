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
// It now bounds response headers and idle body reads; active streams may run
// longer than the timeout as long as data keeps arriving.
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
			"data: {\"choices\":[{\"delta\":{\"content\":\"chunk-4\"}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"content\":\"chunk-5\"}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"content\":\"chunk-6\"}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"content\":\"chunk-7\"}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"content\":\"chunk-8\"}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"content\":\"chunk-9\"}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"content\":\"chunk-10\"}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"content\":\"chunk-11\"}}]}\n\n",
			"data: [DONE]\n\n",
		}
		for i, chunk := range chunks {
			if i > 0 {
				// Total stream time is deliberately > the 1s PROXY_TIMEOUT,
				// while each individual pause stays well below it so the
				// idle-body timeout does not cut a healthy stream.
				time.Sleep(120 * time.Millisecond)
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

func TestIdleTimeoutBodyAbortsStalledRead(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()

	body := newIdleTimeoutBody(reader, 50*time.Millisecond)
	defer body.Close()

	start := time.Now()
	_, err := body.Read(make([]byte, 1))
	elapsed := time.Since(start)

	if !errors.Is(err, ErrUpstreamBodyTimeout) {
		t.Fatalf("Read() error = %v, want %v", err, ErrUpstreamBodyTimeout)
	}
	if elapsed > time.Second {
		t.Fatalf("Read() took %s, idle timeout should have fired after 50ms", elapsed)
	}
}

// TestLLMProxyReturnsGatewayTimeoutOnStalledBody is the integration regression
// for H2: the upstream sends headers and then stops producing body bytes. The
// proxy must not hang forever; it returns 504 before writing a partial response.
func TestLLMProxyReturnsGatewayTimeoutOnStalledBody(t *testing.T) {
	gin.SetMode(gin.TestMode)

	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-release
	}))
	defer upstream.Close()
	defer close(release)

	repo, err := sqlite.NewSQLiteRepository(t.TempDir() + "/stalled.db")
	if err != nil {
		t.Fatalf("sqlite repo: %v", err)
	}
	if err := repo.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfgRow := &models.ProxyConfig{
		Name:          "stalled-test",
		Slug:          "stalled-test",
		ConfigType:    "LLM",
		IsActive:      true,
		TargetBaseURL: stringPtr(upstream.URL),
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

	req := httptest.NewRequest(http.MethodPost, "/llm/stalled-test/v1/chat/completions",
		strings.NewReader(`{"model":"test"}`))
	req.Header.Set("Authorization", "Bearer proxy-secret")
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	start := time.Now()
	router.ServeHTTP(recorder, req)
	elapsed := time.Since(start)

	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 (body: %s)", recorder.Code, recorder.Body.String())
	}
	if elapsed > 3*time.Second {
		t.Fatalf("stalled body took %s to fail, want about 1s", elapsed)
	}
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
