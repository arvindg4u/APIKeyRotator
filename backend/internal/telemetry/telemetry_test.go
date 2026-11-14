package telemetry

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRecordRequestAggregates(t *testing.T) {
	r := NewRecorder(10)
	r.RecordRequest("tokenharbor", "POST", "/llm/tokenharbor/v1/chat/completions", 200, 120*time.Millisecond)
	r.RecordRequest("tokenharbor", "POST", "/llm/tokenharbor/v1/chat/completions", 429, 30*time.Millisecond)
	r.RecordRequest("other", "GET", "/proxy/other/x", 500, 10*time.Millisecond)

	if got := r.TotalRequests(); got != 3 {
		t.Fatalf("TotalRequests = %d, want 3", got)
	}

	counts := r.StatusCounts()
	if counts[200] != 1 || counts[429] != 1 || counts[500] != 1 {
		t.Fatalf("StatusCounts = %v", counts)
	}

	stats := r.SlugStats()
	th := stats["tokenharbor"]
	if th.Requests != 2 || th.RateLimited != 1 {
		t.Fatalf("tokenharbor stat = %+v", th)
	}
	if th.AvgLatencyMS() != 75 {
		t.Fatalf("avg latency = %d, want 75", th.AvgLatencyMS())
	}
	other := stats["other"]
	if other.Requests != 1 || other.ServerErrors != 1 {
		t.Fatalf("other stat = %+v", other)
	}

	rpm := r.RequestsPerMinute()
	if len(rpm) != rpmBuckets || rpm[len(rpm)-1] != 3 {
		t.Fatalf("rpm = %v", rpm)
	}
}

func TestRecentEventsOrderAndBound(t *testing.T) {
	r := NewRecorder(3)
	for i := 1; i <= 5; i++ {
		r.RecordEvent(Event{Kind: "test", Message: fmt.Sprintf("e%d", i)})
	}

	events := r.RecentEvents(0)
	if len(events) != 3 {
		t.Fatalf("len = %d, want 3", len(events))
	}
	if events[0].Message != "e3" || events[2].Message != "e5" {
		t.Fatalf("events = %v", events)
	}
	if events[0].ID != 3 || events[2].ID != 5 {
		t.Fatalf("ids = %d..%d", events[0].ID, events[2].ID)
	}

	limited := r.RecentEvents(2)
	if len(limited) != 2 || limited[0].Message != "e4" || limited[1].Message != "e5" {
		t.Fatalf("limited = %v", limited)
	}
}

func TestAppendLogParsesLevelAndStripsPrefix(t *testing.T) {
	r := NewRecorder(5)
	r.AppendLog("2026/11/14 10:00:00 logger.go:36: [WARNING] key thk_x is cooling")
	r.AppendLog("2026/11/14 10:00:01 main.go:10: [INFO] started")

	logs := r.RecentLogs(0)
	if len(logs) != 2 {
		t.Fatalf("len = %d, want 2", len(logs))
	}
	if logs[0].Level != "warning" || !strings.Contains(logs[0].Message, "cooling") {
		t.Fatalf("log[0] = %+v", logs[0])
	}
	if strings.Contains(logs[0].Message, "logger.go") {
		t.Fatalf("prefix not stripped: %q", logs[0].Message)
	}
	if logs[1].Level != "info" || logs[1].Message != "started" {
		t.Fatalf("log[1] = %+v", logs[1])
	}
}

func TestLogSinkSplitsLines(t *testing.T) {
	r := NewRecorder(5)
	sink := r.LogSink()

	if _, err := sink.Write([]byte("[INFO] one\n[ERROR] two\npart")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := sink.Write([]byte("ial\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	logs := r.RecentLogs(0)
	if len(logs) != 3 {
		t.Fatalf("len = %d, want 3: %+v", len(logs), logs)
	}
	if logs[0].Message != "one" || logs[1].Message != "two" || logs[2].Message != "partial" {
		t.Fatalf("logs = %+v", logs)
	}
	if logs[1].Level != "error" {
		t.Fatalf("level = %q, want error", logs[1].Level)
	}
}

func TestSubscribeReceivesFrames(t *testing.T) {
	r := NewRecorder(5)
	frames, cancel := r.Subscribe()
	defer cancel()

	r.RecordEvent(Event{Kind: "request", Message: "hello"})
	r.AppendLog("[INFO] world")

	var got []Frame
	deadline := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case f := <-frames:
			got = append(got, f)
		case <-deadline:
			t.Fatalf("timed out, got %v", got)
		}
	}
	if got[0].Name != "event" || got[1].Name != "log" {
		t.Fatalf("frames = %v", got)
	}
	ev, ok := got[0].Data.(Event)
	if !ok || ev.Message != "hello" {
		t.Fatalf("event data = %#v", got[0].Data)
	}
}

func TestUnsubscribeClosesChannel(t *testing.T) {
	r := NewRecorder(5)
	frames, cancel := r.Subscribe()
	cancel()

	if _, ok := <-frames; ok {
		t.Fatal("channel should be closed after cancel")
	}
	// Cancelling twice must not panic (Stream defers cancel as well).
	cancel()
}

func TestRecordCooldownEvent(t *testing.T) {
	r := NewRecorder(5)
	r.RecordCooldown("tokenharbor", "thk_li*****G86", "free_tier_exhausted", 85*time.Hour)

	events := r.RecentEvents(0)
	if len(events) != 1 {
		t.Fatalf("len = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Kind != "cooldown" || ev.Level != "warning" || ev.Class != "free_tier_exhausted" {
		t.Fatalf("event = %+v", ev)
	}
	if !strings.Contains(ev.Message, "85h0m0s") {
		t.Fatalf("message = %q", ev.Message)
	}
	if ev.KeyMasked != "thk_li*****G86" {
		t.Fatalf("key = %q", ev.KeyMasked)
	}
}
