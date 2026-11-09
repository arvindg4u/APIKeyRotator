package handlers

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"api-key-rotator/backend/internal/egress"
)

// egressOutcome maps an upstream status to an egress health signal.
// A 429 is usually key/account scoped, not IP scoped, so it does not burn an
// egress unless EGRESS_COOLDOWN_ON_429 is explicitly enabled.
func egressOutcome(status int) egress.Outcome {
	switch {
	case status == http.StatusTooManyRequests:
		return egress.OutcomeRateLimited
	case status == http.StatusForbidden:
		return egress.OutcomeForbidden
	case status >= 500:
		return egress.OutcomeServerError
	default:
		return egress.OutcomeSuccess
	}
}

// newUpstreamHTTPClient builds the client used for proxied upstream requests.
//
// PROXY_TIMEOUT is an inactivity/headers timeout, not a total request deadline.
// Setting http.Client.Timeout here would abort the body read after that many
// seconds, which silently truncates long SSE/LLM streams that are still
// actively producing tokens. ResponseHeaderTimeout catches an upstream that
// never starts responding; wrapUpstreamBody separately closes a response body
// that stops producing bytes for the same interval.
//
// proxyURL is the explicitly selected egress endpoint. When it is nil the
// transport ignores HTTP_PROXY/HTTPS_PROXY from the environment: the egress
// layer owns routing and a global env proxy must not silently take over.
func newUpstreamHTTPClient(timeoutSeconds int, proxyURL *url.URL) *http.Client {
	client := &http.Client{}
	if timeoutSeconds <= 0 {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		if proxyURL != nil {
			transport.Proxy = http.ProxyURL(proxyURL)
		} else {
			transport.Proxy = nil
		}
		client.Transport = transport
		return client
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = time.Duration(timeoutSeconds) * time.Second
	if proxyURL != nil {
		transport.Proxy = http.ProxyURL(proxyURL)
	} else {
		transport.Proxy = nil
	}
	client.Transport = transport
	return client
}

// wrapUpstreamBody applies the idle-body timeout to a network response.
// BufferedResponse bodies are in-memory and are not wrapped.
//
// SSE/event-stream bodies are deliberately excluded: a model can legitimately
// pause for longer than PROXY_TIMEOUT while thinking or waiting for a tool,
// and closing the body mid-stream truncates a healthy response. Streams are
// left to the upstream and to client disconnect detection.
func wrapUpstreamBody(resp *http.Response, timeoutSeconds int) {
	if resp == nil || resp.Body == nil || timeoutSeconds <= 0 {
		return
	}
	if isEventStream(resp.Header.Get("Content-Type")) {
		return
	}
	resp.Body = newIdleTimeoutBody(resp.Body, time.Duration(timeoutSeconds)*time.Second)
}

func isEventStream(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}
