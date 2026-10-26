package handlers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"api-key-rotator/backend/internal/config"
	"api-key-rotator/backend/internal/infrastructure/cache/memory"
	"api-key-rotator/backend/internal/models"

	"github.com/gin-gonic/gin"
)

type observedLLMRequest struct {
	headers http.Header
	query   url.Values
	body    string
}

// TestLLMProxyRedactsClientCredentialsForEveryFormat covers H1 across all LLM
// adapters. The extended credential locations (Proxy-Authorization,
// X-Admin-Token, Referer, custom headers and arbitrary query keys) used to
// reach the upstream even though the adapters stripped the standard auth
// headers.
func TestLLMProxyRedactsClientCredentialsForEveryFormat(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var mu sync.Mutex
	var seen observedLLMRequest
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream request body: %v", err)
		}
		mu.Lock()
		seen = observedLLMRequest{
			headers: r.Header.Clone(),
			query:   r.URL.Query(),
			body:    string(body),
		}
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	repo := newCredentialTestRepo(t)
	formats := []struct {
		name           string
		slug           string
		apiFormat      string
		action         string
		authHeader     string
		authValue      string
		body           string
		upstreamHeader string
		upstreamValue  string
	}{
		{
			name:           "openai",
			slug:           "credentials-openai",
			apiFormat:      "openai_compatible",
			action:         "v1/chat/completions",
			authHeader:     "Authorization",
			authValue:      "Bearer proxy-secret",
			body:           `{"model":"test","api_key":"proxy-secret"}`,
			upstreamHeader: "Authorization",
			upstreamValue:  "Bearer upstream-key",
		},
		{
			name:           "anthropic",
			slug:           "credentials-anthropic",
			apiFormat:      "anthropic_native",
			action:         "v1/messages",
			authHeader:     "x-api-key",
			authValue:      "proxy-secret",
			body:           `{}`,
			upstreamHeader: "x-api-key",
			upstreamValue:  "upstream-key",
		},
		{
			name:           "gemini",
			slug:           "credentials-gemini",
			apiFormat:      "gemini_native",
			action:         "v1beta/models/test:generateContent",
			authHeader:     "x-goog-api-key",
			authValue:      "proxy-secret",
			body:           `{}`,
			upstreamHeader: "x-goog-api-key",
			upstreamValue:  "upstream-key",
		},
	}

	for _, format := range formats {
		cfgRow := &models.ProxyConfig{
			Name:          format.name,
			Slug:          format.slug,
			ConfigType:    "LLM",
			IsActive:      true,
			TargetBaseURL: stringPtr(upstream.URL),
			APIFormat:     stringPtr(format.apiFormat),
			OutputFormat:  stringPtr("none"),
		}
		if err := repo.GetDB().Create(cfgRow).Error; err != nil {
			t.Fatalf("create config %s: %v", format.name, err)
		}
		if err := repo.GetDB().Create(&models.APIKey{
			ProxyConfigID: cfgRow.ID,
			KeyValue:      "upstream-key",
			IsActive:      true,
		}).Error; err != nil {
			t.Fatalf("create key %s: %v", format.name, err)
		}
	}

	handler := NewLLMProxyHandler(&config.Config{
		GlobalProxyKeys:       "proxy-secret",
		ProxyTimeout:          5,
		KeyCooldownSeconds:    config.DefaultKeyCooldownSeconds,
		KeyMaxCooldownSeconds: config.DefaultMaxKeyCooldownSeconds,
		UpstreamUserAgent:     config.DefaultUpstreamUserAgent,
		WAFRetryUserAgent:     config.DefaultWAFRetryUserAgent,
		UpstreamWAFRetry:      false,
	}, repo.GetDB(), memory.NewMemoryCache())

	router := gin.New()
	router.Any("/llm/:slug/*action", handler.HandleLLMProxy)

	for _, format := range formats {
		t.Run(format.name, func(t *testing.T) {
			requestURL := "/llm/" + format.slug + "/" + format.action + "?key=proxy-secret&foo=proxy-secret&legit=1"
			req := httptest.NewRequest(http.MethodPost, requestURL, strings.NewReader(format.body))
			req.Header.Set(format.authHeader, format.authValue)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Proxy-Key", "proxy-secret")
			req.Header.Set("X-Admin-Token", "admin-token")
			req.Header.Set("Proxy-Authorization", "Basic proxy-secret")
			req.Header.Set("Referer", "https://client.example/?key=proxy-secret")
			req.Header.Set("X-Custom-Credential", "Bearer proxy-secret")

			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", recorder.Code, recorder.Body.String())
			}

			mu.Lock()
			got := observedLLMRequest{
				headers: seen.headers.Clone(),
				query:   seen.query,
				body:    seen.body,
			}
			mu.Unlock()

			for name, values := range got.headers {
				for _, value := range values {
					if strings.Contains(value, "proxy-secret") {
						t.Errorf("header %s leaked the proxy key: %q", name, value)
					}
					if strings.Contains(value, "admin-token") {
						t.Errorf("header %s leaked the admin token: %q", name, value)
					}
				}
			}
			for name, values := range got.query {
				for _, value := range values {
					if strings.Contains(value, "proxy-secret") {
						t.Errorf("query parameter %s leaked the proxy key: %q", name, value)
					}
				}
			}
			if strings.Contains(got.body, "proxy-secret") {
				t.Errorf("request body leaked the proxy key: %s", got.body)
			}
			if got.query.Get("legit") != "1" {
				t.Errorf("ordinary query parameter was lost: %v", got.query)
			}
			if value := got.headers.Get(format.upstreamHeader); value != format.upstreamValue {
				t.Errorf("upstream key header %s = %q, want %q", format.upstreamHeader, value, format.upstreamValue)
			}
		})
	}
}
