package handlers

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"api-key-rotator/backend/internal/config"
	"api-key-rotator/backend/internal/infrastructure/cache/memory"
	"api-key-rotator/backend/internal/infrastructure/database/sqlite"
	"api-key-rotator/backend/internal/models"

	"github.com/gin-gonic/gin"
)

type observedRequest struct {
	auth       string
	xpk        string
	xapi       string
	proxyAuth  string
	adminToken string
	referer    string
	custom     string
	query      string
	params     url.Values
}

func newCredentialTestRepo(t *testing.T) *sqlite.Repository {
	t.Helper()
	repo, err := sqlite.NewSQLiteRepository(t.TempDir() + "/credentials.db")
	if err != nil {
		t.Fatalf("sqlite repo: %v", err)
	}
	if err := repo.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return repo
}

func newCredentialTestHandler(t *testing.T, repo *sqlite.Repository) *ProxyHandler {
	t.Helper()
	return NewProxyHandler(&config.Config{
		GlobalProxyKeys:    "proxy-secret",
		ProxyTimeout:       5,
		KeyCooldownSeconds: config.DefaultKeyCooldownSeconds,
		UpstreamUserAgent:  config.DefaultUpstreamUserAgent,
		WAFRetryUserAgent:  config.DefaultWAFRetryUserAgent,
		UpstreamWAFRetry:   true,
	}, repo.GetDB(), memory.NewMemoryCache())
}

// TestGenericProxyHeaderConfigDoesNotLeakClientCredentials covers H1: the
// client's proxy credential must never reach a third-party upstream, while the
// real upstream key must be injected into the configured header.
func TestGenericProxyHeaderConfigDoesNotLeakClientCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var mu sync.Mutex
	var seen []observedRequest
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, observedRequest{
			auth:       r.Header.Get("Authorization"),
			xpk:        r.Header.Get("X-Proxy-Key"),
			xapi:       r.Header.Get("X-Api-Key"),
			proxyAuth:  r.Header.Get("Proxy-Authorization"),
			adminToken: r.Header.Get("X-Admin-Token"),
			referer:    r.Header.Get("Referer"),
			custom:     r.Header.Get("X-Custom-Credential"),
			query:      r.URL.RawQuery,
			params:     r.URL.Query(),
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	repo := newCredentialTestRepo(t)
	keyName := "X-Api-Key"
	keyLoc := "header"
	cfgRow := &models.ProxyConfig{
		Name:           "header-probe",
		Slug:           "header-probe",
		ConfigType:     "GENERIC",
		IsActive:       true,
		Method:         stringPtr("GET"),
		TargetURL:      stringPtr(upstream.URL + "/v1/resource"),
		APIKeyName:     &keyName,
		APIKeyLocation: &keyLoc,
	}
	if err := repo.GetDB().Create(cfgRow).Error; err != nil {
		t.Fatalf("create config: %v", err)
	}
	if err := repo.GetDB().Create(&models.APIKey{
		ProxyConfigID: cfgRow.ID,
		KeyValue:      "sk-real-upstream-key",
		IsActive:      true,
	}).Error; err != nil {
		t.Fatalf("create key: %v", err)
	}

	handler := newCredentialTestHandler(t, repo)
	router := gin.New()
	router.Any("/proxy/*slug", handler.HandleGenericProxy)

	req := httptest.NewRequest(http.MethodGet, "/proxy/header-probe/v1/resource?key=proxy-secret&foo=proxy-secret&token=legit", nil)
	req.Header.Set("X-Proxy-Key", "proxy-secret")
	req.Header.Set("Authorization", "Bearer proxy-secret")
	req.Header.Set("Proxy-Authorization", "Basic proxy-secret")
	req.Header.Set("X-Admin-Token", "admin-token")
	req.Header.Set("Referer", "https://client.example/?key=proxy-secret")
	req.Header.Set("X-Custom-Credential", "Bearer proxy-secret")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", recorder.Code, recorder.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("upstream was never called")
	}
	last := seen[len(seen)-1]
	if last.auth == "Bearer proxy-secret" {
		t.Error("client proxy key leaked upstream as Authorization")
	}
	if last.xpk == "proxy-secret" {
		t.Error("client proxy key leaked upstream as X-Proxy-Key")
	}
	if strings.Contains(last.query, "proxy-secret") {
		t.Errorf("client proxy key leaked upstream in the query string: %q", last.query)
	}
	if last.params.Get("key") != "" || last.params.Get("foo") != "" {
		t.Errorf("proxy credential query parameter was forwarded: %v", last.params)
	}
	if last.params.Get("token") != "legit" {
		t.Errorf("ordinary query parameter was lost: %v", last.params)
	}
	if last.proxyAuth != "" {
		t.Errorf("Proxy-Authorization leaked upstream: %q", last.proxyAuth)
	}
	if last.adminToken != "" {
		t.Errorf("X-Admin-Token leaked upstream: %q", last.adminToken)
	}
	if strings.Contains(last.referer, "proxy-secret") {
		t.Errorf("Referer leaked the proxy key: %q", last.referer)
	}
	if strings.Contains(last.custom, "proxy-secret") {
		t.Errorf("custom header leaked the proxy key: %q", last.custom)
	}
	if last.xapi != "sk-real-upstream-key" {
		t.Errorf("upstream key not injected: X-Api-Key=%q, want %q", last.xapi, "sk-real-upstream-key")
	}
}

// TestGenericProxyQueryConfigDoesNotLeakClientCredentials covers the query-key
// flavour of H1: the configured key parameter is replaced, and a client-supplied
// proxy credential never travels alongside the real key.
func TestGenericProxyQueryConfigDoesNotLeakClientCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var mu sync.Mutex
	var lastQuery url.Values
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		lastQuery = r.URL.Query()
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	repo := newCredentialTestRepo(t)
	keyName := "api_key"
	keyLoc := "query"
	cfgRow := &models.ProxyConfig{
		Name:           "query-probe",
		Slug:           "query-probe",
		ConfigType:     "GENERIC",
		IsActive:       true,
		Method:         stringPtr("GET"),
		TargetURL:      stringPtr(upstream.URL + "/resource"),
		APIKeyName:     &keyName,
		APIKeyLocation: &keyLoc,
	}
	if err := repo.GetDB().Create(cfgRow).Error; err != nil {
		t.Fatalf("create config: %v", err)
	}
	if err := repo.GetDB().Create(&models.APIKey{
		ProxyConfigID: cfgRow.ID,
		KeyValue:      "real-query-key",
		IsActive:      true,
	}).Error; err != nil {
		t.Fatalf("create key: %v", err)
	}

	handler := newCredentialTestHandler(t, repo)
	router := gin.New()
	router.Any("/proxy/*slug", handler.HandleGenericProxy)

	req := httptest.NewRequest(http.MethodGet,
		"/proxy/query-probe?key=proxy-secret&api_key=client-value&q=hello", nil)
	req.Header.Set("X-Proxy-Key", "proxy-secret")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", recorder.Code, recorder.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if lastQuery == nil {
		t.Fatal("upstream was never called")
	}
	if got := lastQuery.Get("api_key"); got != "real-query-key" {
		t.Errorf("api_key = %q, want the injected upstream key", got)
	}
	if got := lastQuery.Get("key"); got != "" {
		t.Errorf("client proxy key leaked upstream as ?key=%s", got)
	}
	if got := lastQuery.Get("q"); got != "hello" {
		t.Errorf("ordinary query parameter q = %q, want %q", got, "hello")
	}
}
