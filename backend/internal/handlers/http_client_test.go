package handlers

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestWrapUpstreamBodySkipsEventStream(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		wantWrapped bool
	}{
		{name: "event stream", contentType: "text/event-stream", wantWrapped: false},
		{name: "event stream with charset", contentType: "text/event-stream; charset=utf-8", wantWrapped: false},
		{name: "json", contentType: "application/json", wantWrapped: true},
		{name: "empty content type", contentType: "", wantWrapped: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{
				Header: http.Header{"Content-Type": []string{tt.contentType}},
				Body:   io.NopCloser(strings.NewReader("body")),
			}

			wrapUpstreamBody(resp, 1)
			defer resp.Body.Close()

			_, wrapped := resp.Body.(*idleTimeoutBody)
			if wrapped != tt.wantWrapped {
				t.Fatalf("body wrapped = %v, want %v for Content-Type %q", wrapped, tt.wantWrapped, tt.contentType)
			}
		})
	}
}
