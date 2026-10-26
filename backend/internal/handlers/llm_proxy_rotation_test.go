package handlers

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"api-key-rotator/backend/internal/config"
	"api-key-rotator/backend/internal/infrastructure/cache/memory"
	"api-key-rotator/backend/internal/infrastructure/database/sqlite"
	"api-key-rotator/backend/internal/models"
	"api-key-rotator/backend/internal/services"

	"github.com/gin-gonic/gin"
)

type rotationFixture struct {
	router   *gin.Engine
	cfg      *config.Config
	cfgRow   *models.ProxyConfig
	cache    *memory.Cache
	mu       sync.Mutex
	attempts []string
}

// newRotationFixture creates a real LLM proxy with nine keys. The upstream
// returns 429 for keys 1-4 and 200 for keys 5-9.
func newRotationFixture(t *testing.T) *rotationFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)

	f := &rotationFixture{}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		f.attempts = append(f.attempts, key)
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		var n int
		if _, err := fmt.Sscanf(key, "key-%d", &n); err != nil {
			t.Errorf("upstream received unexpected key %q", key)
		}
		if n <= 4 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"daily quota exhausted"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(upstream.Close)

	repo, err := sqlite.NewSQLiteRepository(t.TempDir() + "/rotation.db")
	if err != nil {
		t.Fatalf("sqlite repo: %v", err)
	}
	if err := repo.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	f.cfgRow = &models.ProxyConfig{
		Name:          "rotate-test",
		Slug:          "rotate-test",
		ConfigType:    "LLM",
		IsActive:      true,
		TargetBaseURL: stringPtr(upstream.URL + "/v1"),
		APIFormat:     stringPtr("openai_compatible"),
		OutputFormat:  stringPtr("none"),
	}
	if err := repo.GetDB().Create(f.cfgRow).Error; err != nil {
		t.Fatalf("create config: %v", err)
	}
	for i := 1; i <= 9; i++ {
		if err := repo.GetDB().Create(&models.APIKey{
			ProxyConfigID: f.cfgRow.ID,
			KeyValue:      fmt.Sprintf("key-%d", i),
			IsActive:      true,
		}).Error; err != nil {
			t.Fatalf("create key %d: %v", i, err)
		}
	}

	f.cache = memory.NewMemoryCache()
	f.cfg = &config.Config{
		GlobalProxyKeys:       "proxy-secret",
		ProxyTimeout:          5,
		KeyCooldownSeconds:    60,
		KeyMaxCooldownSeconds: 1800,
		UpstreamUserAgent:     config.DefaultUpstreamUserAgent,
		WAFRetryUserAgent:     config.DefaultWAFRetryUserAgent,
		UpstreamWAFRetry:      false,
	}
	handler := NewLLMProxyHandler(f.cfg, repo.GetDB(), f.cache)

	f.router = gin.New()
	f.router.Any("/llm/:slug/*action", handler.HandleLLMProxy)
	return f
}

func (f *rotationFixture) send() (*httptest.ResponseRecorder, []string) {
	req := httptest.NewRequest(http.MethodPost, "/llm/rotate-test/v1/chat/completions",
		strings.NewReader(`{"model":"test"}`))
	req.Header.Set("Authorization", "Bearer proxy-secret")
	req.Header.Set("Content-Type", "application/json")

	f.mu.Lock()
	f.attempts = nil
	f.mu.Unlock()

	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	f.mu.Lock()
	attempts := append([]string(nil), f.attempts...)
	f.mu.Unlock()
	return rec, attempts
}

// TestLLMRotatesAwayFromExhaustedKey is the deterministic small case: key-1 is
// exhausted and the request must move to key-2 instead of retrying key-1. It
// then makes key-2 fail too, to prove rotation is not a one-shot: the next
// request must fall back to key-1 even though key-1 is still cooling.
func TestLLMRotatesAwayFromExhaustedKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var mu sync.Mutex
	var attempts []string
	phase := 0 // 0: key-1 fails; 1: everything succeeds; 2: key-2 fails
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		attempts = append(attempts, key)
		currentPhase := phase
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		fail := (currentPhase == 0 && key == "key-1") || (currentPhase == 2 && key == "key-2")
		if fail {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"quota exhausted"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	repo, err := sqlite.NewSQLiteRepository(t.TempDir() + "/two-keys.db")
	if err != nil {
		t.Fatalf("sqlite repo: %v", err)
	}
	if err := repo.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfgRow := &models.ProxyConfig{
		Name:          "rotate-two",
		Slug:          "rotate-two",
		ConfigType:    "LLM",
		IsActive:      true,
		TargetBaseURL: stringPtr(upstream.URL + "/v1"),
		APIFormat:     stringPtr("openai_compatible"),
		OutputFormat:  stringPtr("none"),
	}
	if err := repo.GetDB().Create(cfgRow).Error; err != nil {
		t.Fatalf("create config: %v", err)
	}
	for i := 1; i <= 2; i++ {
		if err := repo.GetDB().Create(&models.APIKey{
			ProxyConfigID: cfgRow.ID,
			KeyValue:      fmt.Sprintf("key-%d", i),
			IsActive:      true,
		}).Error; err != nil {
			t.Fatalf("create key %d: %v", i, err)
		}
	}

	cacheClient := memory.NewMemoryCache()
	cfg := &config.Config{
		GlobalProxyKeys:    "proxy-secret",
		ProxyTimeout:       5,
		KeyCooldownSeconds: 60,
		UpstreamUserAgent:  config.DefaultUpstreamUserAgent,
		WAFRetryUserAgent:  config.DefaultWAFRetryUserAgent,
		UpstreamWAFRetry:   false,
	}
	handler := NewLLMProxyHandler(cfg, repo.GetDB(), cacheClient)
	router := gin.New()
	router.Any("/llm/:slug/*action", handler.HandleLLMProxy)

	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/llm/rotate-two/v1/chat/completions",
			strings.NewReader(`{"model":"test"}`))
		req.Header.Set("Authorization", "Bearer proxy-secret")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	first := send()
	if first.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200 (body: %s)", first.Code, first.Body.String())
	}

	mu.Lock()
	firstAttempts := append([]string(nil), attempts...)
	attempts = nil
	mu.Unlock()

	if len(firstAttempts) != 2 || firstAttempts[0] != "key-1" || firstAttempts[1] != "key-2" {
		t.Fatalf("first request attempts = %v, want [key-1 key-2]", firstAttempts)
	}

	rotator := services.NewKeyRotator(cfg, cacheClient, "rotation test")
	if remaining := rotator.RemainingCooldown(cfgRow.ID, "key-1"); remaining <= 0 {
		t.Fatal("key-1 returned 429 but is not cooling down")
	}
	if remaining := rotator.RemainingCooldown(cfgRow.ID, "key-2"); remaining != 0 {
		t.Fatalf("key-2 succeeded but is cooling down for %s", remaining)
	}

	mu.Lock()
	phase = 1
	mu.Unlock()

	second := send()
	if second.Code != http.StatusOK {
		t.Fatalf("second request status = %d, want 200 (body: %s)", second.Code, second.Body.String())
	}
	mu.Lock()
	secondAttempts := append([]string(nil), attempts...)
	attempts = nil
	phase = 2
	mu.Unlock()
	if len(secondAttempts) != 1 || secondAttempts[0] != "key-2" {
		t.Fatalf("second request attempts = %v, want [key-2]: the exhausted key must stay skipped", secondAttempts)
	}

	// key-2 now fails as well. Both keys are cooling, but a request must still
	// rotate through them (key-1 is the sooner-recovering one) rather than give
	// up after the first fallback failed.
	third := send()
	if third.Code != http.StatusOK {
		t.Fatalf("third request status = %d, want 200 (body: %s)", third.Code, third.Body.String())
	}
	mu.Lock()
	thirdAttempts := append([]string(nil), attempts...)
	mu.Unlock()
	if len(thirdAttempts) != 2 || thirdAttempts[0] != "key-2" || thirdAttempts[1] != "key-1" {
		t.Fatalf("third request attempts = %v, want [key-2 key-1]: rotation must keep working after the fallback key fails", thirdAttempts)
	}
}

// TestLLMRotationWithNineKeyPool covers the larger "4 to 9" pool: several keys
// are exhausted, the request still succeeds on a healthy key, and keys that
// returned 429 are not immediately reused by the next request.
func TestLLMRotationWithNineKeyPool(t *testing.T) {
	f := newRotationFixture(t)

	first, firstAttempts := f.send()
	if first.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200 (body: %s)", first.Code, first.Body.String())
	}
	if len(firstAttempts) < 2 {
		t.Fatalf("first request attempts = %v; expected at least one 429 followed by a retry", firstAttempts)
	}
	seen := make(map[string]bool)
	for _, key := range firstAttempts {
		if seen[key] {
			t.Fatalf("first request retried key %q: %v", key, firstAttempts)
		}
		seen[key] = true
	}

	rotator := services.NewKeyRotator(f.cfg, f.cache, "rotation test")
	cooling := make(map[string]bool)
	for i := 1; i <= 4; i++ {
		key := fmt.Sprintf("key-%d", i)
		if seen[key] {
			if remaining := rotator.RemainingCooldown(f.cfgRow.ID, key); remaining <= 0 {
				t.Errorf("%s returned 429 but is not cooling down", key)
			}
			cooling[key] = true
		}
	}

	second, secondAttempts := f.send()
	if second.Code != http.StatusOK {
		t.Fatalf("second request status = %d, want 200 (body: %s)", second.Code, second.Body.String())
	}
	for _, key := range secondAttempts {
		if cooling[key] {
			t.Fatalf("second request reused exhausted key %q: %v", key, secondAttempts)
		}
	}
}
