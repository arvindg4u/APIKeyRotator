package handlers

import (
	"testing"

	"api-key-rotator/backend/internal/models"
)

func strptr(s string) *string { return &s }

func TestParseModelsResponseProviders(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantID     string
		wantName   string
		wantCtx    int64
		wantHasCtx bool
	}{
		{
			name:     "openai without context window",
			body:     `{"object":"list","data":[{"id":"gpt-4o","object":"model","owned_by":"openai"}]}`,
			wantID:   "gpt-4o",
			wantName: "gpt-4o",
		},
		{
			name:       "openrouter style context_length",
			body:       `{"data":[{"id":"anthropic/claude-3.5-sonnet","context_length":200000}]}`,
			wantID:     "anthropic/claude-3.5-sonnet",
			wantName:   "anthropic/claude-3.5-sonnet",
			wantCtx:    200000,
			wantHasCtx: true,
		},
		{
			name:       "gemini native inputTokenLimit and display name",
			body:       `{"models":[{"name":"models/gemini-2.5-flash","displayName":"Gemini 2.5 Flash","inputTokenLimit":1048576,"outputTokenLimit":8192}]}`,
			wantID:     "gemini-2.5-flash",
			wantName:   "Gemini 2.5 Flash",
			wantCtx:    1048576,
			wantHasCtx: true,
		},
		{
			name:     "anthropic native display_name no context",
			body:     `{"data":[{"type":"model","id":"claude-3-5-sonnet-20241022","display_name":"Claude 3.5 Sonnet"}],"has_more":false}`,
			wantID:   "claude-3-5-sonnet-20241022",
			wantName: "Claude 3.5 Sonnet",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseModelsResponse([]byte(tt.body))
			if err != nil {
				t.Fatalf("parseModelsResponse() error = %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("parseModelsResponse() returned %d entries, want 1: %#v", len(got), got)
			}
			if got[0].ID != tt.wantID {
				t.Errorf("ID = %q, want %q", got[0].ID, tt.wantID)
			}
			if got[0].Name != tt.wantName {
				t.Errorf("Name = %q, want %q", got[0].Name, tt.wantName)
			}
			if tt.wantHasCtx {
				if got[0].ContextWindow == nil || *got[0].ContextWindow != tt.wantCtx {
					t.Errorf("ContextWindow = %v, want %d", got[0].ContextWindow, tt.wantCtx)
				}
			} else if got[0].ContextWindow != nil {
				t.Errorf("ContextWindow = %v, want nil", *got[0].ContextWindow)
			}
		})
	}
}

func TestParseModelsResponseEmptyList(t *testing.T) {
	got, err := parseModelsResponse([]byte(`{"data":[]}`))
	if err != nil {
		t.Fatalf("parseModelsResponse() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("parseModelsResponse() = %#v, want empty", got)
	}
}

func TestParseModelsResponseDeduplicates(t *testing.T) {
	got, err := parseModelsResponse([]byte(`{"data":[{"id":"a"},{"id":"a"},{"id":"b"}]}`))
	if err != nil {
		t.Fatalf("parseModelsResponse() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("parseModelsResponse() returned %d entries, want 2", len(got))
	}
}

func TestParseModelsResponseRejectsNonList(t *testing.T) {
	if _, err := parseModelsResponse([]byte(`{"error":{"message":"nope"}}`)); err == nil {
		t.Fatal("parseModelsResponse() error = nil, want error for a non-list response")
	}
}

func TestModelsURL(t *testing.T) {
	if _, err := modelsURL(&models.ProxyConfig{}); err == nil {
		t.Fatal("modelsURL() error = nil, want error when base URL is empty")
	}

	url, err := modelsURL(&models.ProxyConfig{TargetBaseURL: strptr("https://api.openai.com/v1/")})
	if err != nil {
		t.Fatalf("modelsURL() error = %v", err)
	}
	if url != "https://api.openai.com/v1/models" {
		t.Fatalf("modelsURL() = %q, want %q", url, "https://api.openai.com/v1/models")
	}
}
