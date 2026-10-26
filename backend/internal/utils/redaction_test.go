package utils

import "testing"

func TestRedactSecrets(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		secrets []string
		want    string
	}{
		{
			name:    "query string",
			text:    "/proxy/demo?key=proxy-secret&token=legit",
			secrets: []string{"proxy-secret"},
			want:    "/proxy/demo?key=[REDACTED]&token=legit",
		},
		{
			name:    "multiple occurrences and keys",
			text:    "/x?one=first-secret&two=second-secret&one_again=first-secret",
			secrets: []string{"first-secret", "second-secret"},
			want:    "/x?one=[REDACTED]&two=[REDACTED]&one_again=[REDACTED]",
		},
		{
			name:    "url encoded secret",
			text:    "/proxy/demo?key=abc%2Fdef",
			secrets: []string{"abc/def"},
			want:    "/proxy/demo?key=[REDACTED]",
		},
		{
			name:    "empty secret is ignored",
			text:    "/health",
			secrets: []string{"", "proxy-secret"},
			want:    "/health",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RedactSecrets(tt.text, tt.secrets); got != tt.want {
				t.Fatalf("RedactSecrets() = %q, want %q", got, tt.want)
			}
		})
	}
}
