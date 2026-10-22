package services

import (
	"net/http"
	"testing"

	"api-key-rotator/backend/internal/config"
	"api-key-rotator/backend/internal/models"
)

func TestParseExtraHeaders(t *testing.T) {
	tests := []struct {
		name string
		raw  *string
		want map[string]string
	}{
		{
			name: "nil",
			raw:  nil,
			want: map[string]string{},
		},
		{
			name: "empty",
			raw:  strPtr("   "),
			want: map[string]string{},
		},
		{
			name: "json object",
			raw:  strPtr(`{"User-Agent": "curl/8.4.0", "X-Client": "hermes"}`),
			want: map[string]string{"User-Agent": "curl/8.4.0", "X-Client": "hermes"},
		},
		{
			name: "one header per line",
			raw:  strPtr("User-Agent: curl/8.4.0\nX-Client: hermes\n"),
			want: map[string]string{"User-Agent": "curl/8.4.0", "X-Client": "hermes"},
		},
		{
			name: "comments and blank lines are skipped",
			raw:  strPtr("# set a UA so the WAF lets us through\nUser-Agent: curl/8.4.0\n\nignored-line\n"),
			want: map[string]string{"User-Agent": "curl/8.4.0"},
		},
		{
			name: "broken json falls back to line parsing",
			raw:  strPtr("{\nUser-Agent: curl/8.4.0\n"),
			want: map[string]string{"User-Agent": "curl/8.4.0"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseExtraHeaders(tt.raw)
			if len(got) != len(tt.want) {
				t.Fatalf("ParseExtraHeaders() = %v, want %v", got, tt.want)
			}
			for name, value := range tt.want {
				if got[name] != value {
					t.Fatalf("header %q = %q, want %q", name, got[name], value)
				}
			}
		})
	}
}

func TestApplyUpstreamHeadersSendsConfiguredUserAgent(t *testing.T) {
	// 客户端没有带 UA 时，必须补上默认的 UA，
	// 否则 Go 会发出 "Go-http-client/1.1" 被上游 WAF 直接拦掉
	target := &TargetRequest{Headers: map[string]string{"Content-Type": "application/json"}}
	proxyConfig := &models.ProxyConfig{}
	cfg := &config.Config{UpstreamUserAgent: "Mozilla/5.0 (compatible; APIKeyRotator/1.0)"}

	ApplyUpstreamHeaders(target, proxyConfig, cfg)

	if got := target.Headers["User-Agent"]; got != cfg.UpstreamUserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, cfg.UpstreamUserAgent)
	}
}

func TestApplyUpstreamHeadersKeepsClientUserAgent(t *testing.T) {
	// 客户端带了 UA 就原样转发，不被默认值覆盖
	target := &TargetRequest{Headers: map[string]string{"User-Agent": "python-httpx/0.27.0"}}
	cfg := &config.Config{UpstreamUserAgent: "fallback-agent"}

	ApplyUpstreamHeaders(target, &models.ProxyConfig{}, cfg)

	if got := target.Headers["User-Agent"]; got != "python-httpx/0.27.0" {
		t.Fatalf("User-Agent = %q, want the client's %q", got, "python-httpx/0.27.0")
	}
}

func TestApplyUpstreamHeadersExtraHeadersWin(t *testing.T) {
	// extra_headers 优先级最高，且不能留下只差大小写的重复项
	target := &TargetRequest{Headers: map[string]string{"user-agent": "python-httpx/0.27.0"}}
	proxyConfig := &models.ProxyConfig{ExtraHeaders: strPtr("User-Agent: curl/8.4.0")}
	cfg := &config.Config{UpstreamUserAgent: "fallback-agent"}

	ApplyUpstreamHeaders(target, proxyConfig, cfg)

	if got := target.Headers["User-Agent"]; got != "curl/8.4.0" {
		t.Fatalf("User-Agent = %q, want %q from extra_headers", got, "curl/8.4.0")
	}
	if len(target.Headers) != 1 {
		t.Fatalf("headers = %v, want exactly one entry (no case-duplicates)", target.Headers)
	}
}

func TestApplyUpstreamHeadersNilTarget(t *testing.T) {
	// 防御: target 为空时不能 panic
	ApplyUpstreamHeaders(nil, &models.ProxyConfig{}, &config.Config{UpstreamUserAgent: "agent"})
}

func TestLooksLikeBlockPage(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       []byte
		want       bool
	}{
		{
			name:       "html block page",
			statusCode: http.StatusForbidden,
			body:       []byte("<html><head><title>403 Forbidden</title></head><body>Access denied</body></html>"),
			want:       true,
		},
		{
			name:       "plain text block message",
			statusCode: http.StatusForbidden,
			body:       []byte("error code: 1010"),
			want:       true,
		},
		{
			name:       "json api error is not a block page",
			statusCode: http.StatusForbidden,
			body:       []byte(`{"error":{"message":"invalid api key","type":"invalid_request_error"}}`),
			want:       false,
		},
		{
			name:       "empty body",
			statusCode: http.StatusForbidden,
			body:       []byte("   "),
			want:       false,
		},
		{
			name:       "successful html response is not a block page",
			statusCode: http.StatusOK,
			body:       []byte("<html>hi</html>"),
			want:       false,
		},
		{
			name:       "rate limit json is not a block page",
			statusCode: http.StatusTooManyRequests,
			body:       []byte(`{"error":{"message":"rate limited"}}`),
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LooksLikeBlockPage(tt.statusCode, tt.body); got != tt.want {
				t.Fatalf("LooksLikeBlockPage(%d, %q) = %v, want %v", tt.statusCode, tt.body, got, tt.want)
			}
		})
	}
}
