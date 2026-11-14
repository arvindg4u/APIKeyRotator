// Package telemetry keeps bounded, process-local runtime telemetry for the
// admin status page: request counters, per-slug traffic, and ring buffers of
// structured events and application log lines.
//
// Everything here is intentionally in-memory. The deployment uses the memory
// cache (CACHE_TYPE=memory), so counters and buffers reset when the container
// restarts; that is the documented behaviour of this page.
package telemetry

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	// DefaultBufferSize is how many recent events/log lines are kept in memory.
	DefaultBufferSize = 500
	// subscriberBacklog bounds a single SSE client's queue. A client that falls
	// further behind drops frames instead of growing memory without limit.
	subscriberBacklog = 64
	// maxPartialLogLine caps a line that never receives a newline.
	maxPartialLogLine = 8192
	// rpmBuckets is the number of one-minute buckets kept for the traffic graph.
	rpmBuckets = 60
)

// Event is one structured telemetry record (request, cooldown, or system note).
type Event struct {
	ID        int64     `json:"id"`
	Time      time.Time `json:"time"`
	Level     string    `json:"level"`
	Kind      string    `json:"kind"`
	Slug      string    `json:"slug,omitempty"`
	Method    string    `json:"method,omitempty"`
	Path      string    `json:"path,omitempty"`
	Status    int       `json:"status,omitempty"`
	LatencyMS int64     `json:"latency_ms,omitempty"`
	KeyMasked string    `json:"key_masked,omitempty"`
	Class     string    `json:"class,omitempty"`
	Message   string    `json:"message,omitempty"`
}

// LogLine is one captured application log line.
type LogLine struct {
	ID      int64     `json:"id"`
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

// Frame is a single SSE payload: a structured event or an application log line.
type Frame struct {
	Name string
	Data any
}

// SlugStat aggregates traffic for one proxy config slug.
type SlugStat struct {
	Requests       int64 `json:"requests"`
	RateLimited    int64 `json:"rate_limited"`
	ClientErrors   int64 `json:"client_errors"`
	ServerErrors   int64 `json:"server_errors"`
	TotalLatencyMS int64 `json:"-"`
}

// AvgLatencyMS returns the mean request latency in milliseconds.
func (s SlugStat) AvgLatencyMS() int64 {
	if s.Requests == 0 {
		return 0
	}
	return s.TotalLatencyMS / s.Requests
}

// Recorder keeps bounded in-memory telemetry.
type Recorder struct {
	mu sync.RWMutex

	size      int
	startedAt time.Time

	total    int64
	statuses map[int]int64
	slugs    map[string]*SlugStat

	events     []Event
	eventNext  int
	eventCount int64

	logs     []LogLine
	logNext  int
	logCount int64

	rpm       []int64
	rpmMinute time.Time

	subs map[chan Frame]struct{}

	// sinkMu guards sinkTail, the partial line accumulated by the log writer.
	// It is deliberately separate from mu: the writer calls AppendLog (which
	// takes mu) while holding sinkMu, and only the writer takes sinkMu.
	sinkMu   sync.Mutex
	sinkTail []byte
}

var (
	globalOnce sync.Once
	global     *Recorder
)

// Default returns the process-wide recorder used by the router, handlers and
// the captured logger. It is created lazily so logger.Setup can install the
// sink before configuration is loaded.
func Default() *Recorder {
	globalOnce.Do(func() {
		global = NewRecorder(DefaultBufferSize)
	})
	return global
}

// NewRecorder creates a recorder with buffers of the given size. Sizes below 1
// fall back to DefaultBufferSize.
func NewRecorder(size int) *Recorder {
	if size <= 0 {
		size = DefaultBufferSize
	}
	now := time.Now()
	return &Recorder{
		size:      size,
		startedAt: now,
		statuses:  make(map[int]int64),
		slugs:     make(map[string]*SlugStat),
		events:    make([]Event, size),
		logs:      make([]LogLine, size),
		rpm:       make([]int64, rpmBuckets),
		rpmMinute: now.Truncate(time.Minute),
		subs:      make(map[chan Frame]struct{}),
	}
}

// StartedAt reports when the recorder (i.e. the process) started.
func (r *Recorder) StartedAt() time.Time { return r.startedAt }

// Uptime reports how long the process has been running.
func (r *Recorder) Uptime() time.Duration { return time.Since(r.startedAt) }

// GinMiddleware records every request that passes through it.
//
// It is mounted on the proxy groups only. Do not mount it on the admin group:
// the status endpoints would record their own polling requests and feed an
// endless stream of noise back into the stats.
func (r *Recorder) GinMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		r.RecordRequest(normalizeSlug(c.Param("slug")), c.Request.Method, c.Request.URL.Path, c.Writer.Status(), time.Since(start))
	}
}

func normalizeSlug(slug string) string {
	slug = strings.TrimPrefix(slug, "/")
	if idx := strings.IndexByte(slug, '/'); idx >= 0 {
		slug = slug[:idx]
	}
	return slug
}

// RecordRequest updates the counters and appends a request event.
func (r *Recorder) RecordRequest(slug, method, path string, status int, latency time.Duration) {
	ms := latency.Milliseconds()
	now := time.Now()

	r.mu.Lock()
	defer r.mu.Unlock()

	r.total++
	r.statuses[status]++

	key := slug
	if key == "" {
		key = "-"
	}
	stat := r.slugs[key]
	if stat == nil {
		stat = &SlugStat{}
		r.slugs[key] = stat
	}
	stat.Requests++
	stat.TotalLatencyMS += ms
	switch {
	case status == 429:
		stat.RateLimited++
	case status >= 500:
		stat.ServerErrors++
	case status >= 400:
		stat.ClientErrors++
	}

	r.bumpMinuteLocked(now)

	r.recordLocked(Event{
		Time:      now,
		Level:     levelForStatus(status),
		Kind:      "request",
		Slug:      slug,
		Method:    method,
		Path:      path,
		Status:    status,
		LatencyMS: ms,
		Message:   fmt.Sprintf("%s %s -> %d (%dms)", method, path, status, ms),
	})
}

// RecordCooldown records why an upstream key was taken out of rotation.
func (r *Recorder) RecordCooldown(slug, keyMasked, class string, cooldown time.Duration) {
	msg := fmt.Sprintf("key %s cooling (default) (%s)", keyMasked, class)
	if cooldown > 0 {
		msg = fmt.Sprintf("key %s cooling %s (%s)", keyMasked, cooldown.Round(time.Second), class)
	}
	r.RecordEvent(Event{
		Level:     "warning",
		Kind:      "cooldown",
		Slug:      slug,
		KeyMasked: keyMasked,
		Class:     class,
		Message:   msg,
	})
}

// RecordEvent appends an arbitrary structured event.
func (r *Recorder) RecordEvent(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if e.Level == "" {
		e.Level = "info"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recordLocked(e)
}

func (r *Recorder) recordLocked(e Event) {
	r.eventCount++
	e.ID = r.eventCount
	r.events[r.eventNext] = e
	r.eventNext = (r.eventNext + 1) % r.size
	r.broadcastLocked(Frame{Name: "event", Data: e})
}

// AppendLog stores one application log line, keeping only the message and
// level (the stdlib timestamp/file prefix is stripped).
func (r *Recorder) AppendLog(line string) {
	line = strings.TrimRight(line, "\r")
	if strings.TrimSpace(line) == "" {
		return
	}
	level, message := parseLogLine(line)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.logCount++
	l := LogLine{ID: r.logCount, Time: time.Now(), Level: level, Message: message}
	r.logs[r.logNext] = l
	r.logNext = (r.logNext + 1) % r.size
	r.broadcastLocked(Frame{Name: "log", Data: l})
}

// LogSink returns an io.Writer that can be plugged into log.SetOutput. It
// buffers partial writes and appends complete lines to the ring buffer.
func (r *Recorder) LogSink() io.Writer { return &logSink{rec: r} }

type logSink struct{ rec *Recorder }

func (s *logSink) Write(p []byte) (int, error) {
	s.rec.sinkMu.Lock()
	defer s.rec.sinkMu.Unlock()

	s.rec.sinkTail = append(s.rec.sinkTail, p...)
	for {
		idx := bytes.IndexByte(s.rec.sinkTail, '\n')
		if idx < 0 {
			break
		}
		line := string(s.rec.sinkTail[:idx])
		s.rec.sinkTail = s.rec.sinkTail[idx+1:]
		s.rec.AppendLog(line)
	}
	if len(s.rec.sinkTail) > maxPartialLogLine {
		s.rec.sinkTail = s.rec.sinkTail[len(s.rec.sinkTail)-maxPartialLogLine:]
	}
	return len(p), nil
}

var logPrefixRe = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} \S+:\d+: `)

// parseLogLine strips the stdlib log prefix and maps the level tag.
func parseLogLine(line string) (level, message string) {
	line = logPrefixRe.ReplaceAllString(line, "")
	for _, candidate := range []struct{ tag, level string }{
		{"[FATAL]", "error"},
		{"[ERROR]", "error"},
		{"[WARNING]", "warning"},
		{"[WARN]", "warning"},
		{"[INFO]", "info"},
		{"[DEBUG]", "debug"},
	} {
		if idx := strings.Index(line, candidate.tag); idx >= 0 {
			return candidate.level, strings.TrimSpace(line[idx+len(candidate.tag):])
		}
	}
	return "info", strings.TrimSpace(line)
}

func levelForStatus(status int) string {
	switch {
	case status >= 500:
		return "error"
	case status >= 400:
		return "warning"
	default:
		return "info"
	}
}

func (r *Recorder) bumpMinuteLocked(now time.Time) {
	minute := now.Truncate(time.Minute)
	if r.rpmMinute.IsZero() {
		r.rpmMinute = minute
	}
	gap := int(minute.Sub(r.rpmMinute) / time.Minute)
	if gap <= 0 {
		r.rpm[len(r.rpm)-1]++
		return
	}
	if gap >= len(r.rpm) {
		for i := range r.rpm {
			r.rpm[i] = 0
		}
	} else {
		for i := 0; i < gap; i++ {
			copy(r.rpm, r.rpm[1:])
			r.rpm[len(r.rpm)-1] = 0
		}
	}
	r.rpmMinute = minute
	r.rpm[len(r.rpm)-1]++
}

// TotalRequests returns the number of requests recorded since start.
func (r *Recorder) TotalRequests() int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.total
}

// StatusCounts returns a copy of the raw status-code counters.
func (r *Recorder) StatusCounts() map[int]int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[int]int64, len(r.statuses))
	for code, count := range r.statuses {
		out[code] = count
	}
	return out
}

// SlugStats returns a copy of the per-slug aggregates.
func (r *Recorder) SlugStats() map[string]SlugStat {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]SlugStat, len(r.slugs))
	for slug, stat := range r.slugs {
		out[slug] = *stat
	}
	return out
}

// RequestsPerMinute returns the last 60 one-minute buckets, oldest first.
func (r *Recorder) RequestsPerMinute() []int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]int64, len(r.rpm))
	copy(out, r.rpm)
	return out
}

// RecentEvents returns up to limit events, oldest first.
func (r *Recorder) RecentEvents(limit int) []Event {
	r.mu.RLock()
	defer r.mu.RUnlock()

	n := int(minInt64(r.eventCount, int64(r.size)))
	if limit > 0 && limit < n {
		n = limit
	}
	if n == 0 {
		return nil
	}
	start := (r.eventNext - n + r.size) % r.size
	out := make([]Event, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, r.events[(start+i)%r.size])
	}
	return out
}

// RecentLogs returns up to limit log lines, oldest first.
func (r *Recorder) RecentLogs(limit int) []LogLine {
	r.mu.RLock()
	defer r.mu.RUnlock()

	n := int(minInt64(r.logCount, int64(r.size)))
	if limit > 0 && limit < n {
		n = limit
	}
	if n == 0 {
		return nil
	}
	start := (r.logNext - n + r.size) % r.size
	out := make([]LogLine, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, r.logs[(start+i)%r.size])
	}
	return out
}

// Subscribe registers an SSE client. The returned cancel function must be
// called exactly once; it unsubscribes and closes the channel.
func (r *Recorder) Subscribe() (<-chan Frame, func()) {
	ch := make(chan Frame, subscriberBacklog)

	r.mu.Lock()
	r.subs[ch] = struct{}{}
	r.mu.Unlock()

	cancel := func() {
		r.mu.Lock()
		if _, ok := r.subs[ch]; ok {
			delete(r.subs, ch)
			close(ch)
		}
		r.mu.Unlock()
	}
	return ch, cancel
}

func (r *Recorder) broadcastLocked(f Frame) {
	for ch := range r.subs {
		select {
		case ch <- f:
		default:
			// Slow client: drop this frame. The client keeps receiving later
			// frames and can resync from the overview/log endpoints. Dropping
			// here is what keeps one stuck browser from blocking the proxy.
		}
	}
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
