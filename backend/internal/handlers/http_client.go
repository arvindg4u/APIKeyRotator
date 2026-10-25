package handlers

import (
	"net/http"
	"time"
)

// newUpstreamHTTPClient builds the client used for proxied upstream requests.
//
// PROXY_TIMEOUT is an inactivity/headers timeout, not a total request deadline.
// Setting http.Client.Timeout here would abort the body read after that many
// seconds, which silently truncates long SSE/LLM streams that are still
// actively producing tokens. ResponseHeaderTimeout still catches an upstream
// that never starts responding, while leaving an established stream alone.
func newUpstreamHTTPClient(timeoutSeconds int) *http.Client {
	client := &http.Client{}
	if timeoutSeconds <= 0 {
		return client
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = time.Duration(timeoutSeconds) * time.Second
	client.Transport = transport
	return client
}
