package handlers

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// ErrUpstreamBodyTimeout is returned when an upstream response body stalls for
// longer than PROXY_TIMEOUT. It is mapped to HTTP 504 by the proxy handlers.
var ErrUpstreamBodyTimeout = errors.New("upstream response body idle timeout")

// idleTimeoutBody closes the underlying response body when no bytes arrive for
// the configured interval. This is intentionally an idle timeout, not a total
// deadline: a healthy SSE stream may run for hours as long as it keeps
// producing data.
type idleTimeoutBody struct {
	body      io.ReadCloser
	timeout   time.Duration
	timer     *time.Timer
	timedOut  atomic.Bool
	closed    atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

func newIdleTimeoutBody(body io.ReadCloser, timeout time.Duration) io.ReadCloser {
	if body == nil || timeout <= 0 {
		return body
	}

	b := &idleTimeoutBody{
		body:    body,
		timeout: timeout,
	}
	b.timer = time.AfterFunc(timeout, func() {
		b.timedOut.Store(true)
		_ = b.closeUnderlying()
	})
	return b
}

func (b *idleTimeoutBody) Read(p []byte) (int, error) {
	if b.closed.Load() {
		return 0, fmt.Errorf("%w after %s", ErrUpstreamBodyTimeout, b.timeout)
	}

	b.timer.Reset(b.timeout)
	n, err := b.body.Read(p)
	b.timer.Stop()

	if b.timedOut.Load() {
		if n > 0 {
			// Data arrived just as the timer fired. Return it instead of
			// losing the final bytes; the next Read will report the timeout.
			return n, nil
		}
		return 0, fmt.Errorf("%w after %s", ErrUpstreamBodyTimeout, b.timeout)
	}

	return n, err
}

func (b *idleTimeoutBody) Close() error {
	if b.timer != nil {
		b.timer.Stop()
	}
	return b.closeUnderlying()
}

func (b *idleTimeoutBody) closeUnderlying() error {
	b.closed.Store(true)
	b.closeOnce.Do(func() {
		b.closeErr = b.body.Close()
	})
	return b.closeErr
}
