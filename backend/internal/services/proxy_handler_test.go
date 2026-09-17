package services

import (
	"net/http"
	"testing"

	"api-key-rotator/backend/internal/models"
)

func strPtr(s string) *string {
	return &s
}

func TestActiveAPIKeys(t *testing.T) {
	cfg := &models.ProxyConfig{
		APIKeys: []models.APIKey{
			{KeyValue: "key-1", IsActive: true},
			{KeyValue: "key-2", IsActive: false},
			{KeyValue: "key-3", IsActive: true},
		},
	}

	active := ActiveAPIKeys(cfg)
	if len(active) != 2 {
		t.Fatalf("expected 2 active keys, got %d", len(active))
	}
	if active[0].KeyValue != "key-1" || active[1].KeyValue != "key-3" {
		t.Fatalf("unexpected active keys: %+v", active)
	}

	if got := ActiveAPIKeys(&models.ProxyConfig{}); len(got) != 0 {
		t.Fatalf("expected 0 active keys, got %d", len(got))
	}
}

func TestIsRetryableStatus(t *testing.T) {
	retryable := []int{
		http.StatusTooManyRequests, // 429: rate limited / quota exhausted
		http.StatusUnauthorized,    // 401: key invalid
		http.StatusForbidden,       // 403: key disabled / no permission
	}
	for _, code := range retryable {
		if !IsRetryableStatus(code) {
			t.Errorf("expected status %d to be retryable", code)
		}
	}

	notRetryable := []int{
		http.StatusOK,
		http.StatusCreated,
		http.StatusBadRequest,
		http.StatusNotFound,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
	}
	for _, code := range notRetryable {
		if IsRetryableStatus(code) {
			t.Errorf("expected status %d to NOT be retryable", code)
		}
	}
}

func TestInjectUpstreamKey(t *testing.T) {
	tests := []struct {
		name         string
		apiFormat    string
		keyName      *string
		keyLocation  *string
		target       *TargetRequest
		wantHeaders  map[string]string
		wantParams   map[string]string
	}{
		{
			name:      "openai default header with Bearer prefix",
			apiFormat: "openai_compatible",
			target: &TargetRequest{
				Headers: map[string]string{"Authorization": "Bearer old-key"},
			},
			wantHeaders: map[string]string{"Authorization": "Bearer new-key"},
		},
		{
			name:        "openai custom query location uses raw key",
			apiFormat:   "openai_compatible",
			keyName:     strPtr("api_key"),
			keyLocation: strPtr("query"),
			target: &TargetRequest{
				Params: map[string]string{"api_key": "old-key"},
			},
			wantParams: map[string]string{"api_key": "new-key"},
		},
		{
			name:      "gemini default header uses raw key",
			apiFormat: "gemini_native",
			target: &TargetRequest{
				Headers: map[string]string{"x-goog-api-key": "old-key"},
			},
			wantHeaders: map[string]string{"x-goog-api-key": "new-key"},
		},
		{
			name:        "anthropic custom header name uses raw key",
			apiFormat:   "anthropic_native",
			keyName:     strPtr("x-custom-key"),
			keyLocation: strPtr("header"),
			target: &TargetRequest{
				Headers: map[string]string{"x-custom-key": "old-key"},
			},
			wantHeaders: map[string]string{"x-custom-key": "new-key"},
		},
		{
			name:        "generic header uses raw key",
			apiFormat:   "generic",
			keyName:     strPtr("X-Api-Token"),
			keyLocation: strPtr("header"),
			target: &TargetRequest{
				Headers: map[string]string{"X-Api-Token": "old-key"},
			},
			wantHeaders: map[string]string{"X-Api-Token": "new-key"},
		},
		{
			name:        "generic query uses raw key",
			apiFormat:   "generic",
			keyName:     strPtr("token"),
			keyLocation: strPtr("query"),
			target: &TargetRequest{
				Params: map[string]string{"token": "old-key"},
			},
			wantParams: map[string]string{"token": "new-key"},
		},
		{
			name:      "generic without config is a no-op",
			apiFormat: "generic",
			target: &TargetRequest{
				Headers: map[string]string{},
				Params:  map[string]string{},
			},
			wantHeaders: map[string]string{},
			wantParams:  map[string]string{},
		},
		{
			name:      "nil maps are initialized",
			apiFormat: "gemini_native",
			target:    &TargetRequest{},
			wantHeaders: map[string]string{
				"x-goog-api-key": "new-key",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &models.ProxyConfig{
				APIKeyName:     tt.keyName,
				APIKeyLocation: tt.keyLocation,
			}
			InjectUpstreamKey(tt.target, cfg, tt.apiFormat, "new-key")

			for k, want := range tt.wantHeaders {
				if got := tt.target.Headers[k]; got != want {
					t.Errorf("header %q: expected %q, got %q", k, want, got)
				}
			}
			for k, want := range tt.wantParams {
				if got := tt.target.Params[k]; got != want {
					t.Errorf("param %q: expected %q, got %q", k, want, got)
				}
			}
			if len(tt.wantHeaders) == 0 && len(tt.target.Headers) != 0 {
				t.Errorf("expected no headers, got %+v", tt.target.Headers)
			}
			if len(tt.wantParams) == 0 && len(tt.target.Params) != 0 {
				t.Errorf("expected no params, got %+v", tt.target.Params)
			}
		})
	}
}
