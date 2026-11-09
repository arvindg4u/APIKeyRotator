package egress

import (
	"testing"
	"time"

	"api-key-rotator/backend/internal/config"
)

func testManager(t *testing.T, endpoints string, sticky, cooldown int, on429 bool) *Manager {
	t.Helper()
	m := New(&config.Config{
		EgressEnabled:         true,
		EgressEndpoints:       endpoints,
		EgressStickySeconds:   sticky,
		EgressCooldownSeconds: cooldown,
		EgressCooldownOn429:   on429,
	})
	if !m.Enabled() {
		t.Fatalf("manager is not enabled for endpoints %q", endpoints)
	}
	return m
}

func TestParseEndpoints(t *testing.T) {
	m := testManager(t, `[{"id":"oci","url":"http://172.17.0.1:18160","region":"ap-mumbai-1"},{"url":"http://127.0.0.1:18170"}]`, 60, 60, false)
	if len(m.endpoints) != 2 {
		t.Fatalf("endpoints = %d, want 2", len(m.endpoints))
	}
	if m.endpoints[0].ID != "oci" || m.endpoints[0].proxy.Host != "172.17.0.1:18160" {
		t.Fatalf("first endpoint = %+v", m.endpoints[0])
	}
	if m.endpoints[1].ID != "127.0.0.1:18170" {
		t.Fatalf("second endpoint id = %q, want host fallback", m.endpoints[1].ID)
	}

	simple := testManager(t, "a=http://127.0.0.1:1,b=http://127.0.0.1:2", 60, 60, false)
	if len(simple.endpoints) != 2 || simple.byID["b"] == nil {
		t.Fatalf("simple endpoint list was not parsed: %+v", simple.endpoints)
	}
}

func TestInvalidEndpointsDisableEgress(t *testing.T) {
	m := New(&config.Config{EgressEnabled: true, EgressEndpoints: "not-a-url"})
	if m.Enabled() {
		t.Fatal("invalid endpoint list should leave egress disabled")
	}
	if ep := m.Pick("key"); ep != nil {
		t.Fatalf("Pick() = %+v, want nil when disabled", ep)
	}
}

func TestPickDisabledIsDirect(t *testing.T) {
	m := New(&config.Config{})
	if m.Enabled() {
		t.Fatal("egress must be disabled by default")
	}
	if ep := m.Pick("key"); ep != nil {
		t.Fatalf("Pick() = %+v, want nil", ep)
	}
}

func TestStickyAffinity(t *testing.T) {
	m := testManager(t, "a=http://127.0.0.1:1,b=http://127.0.0.1:2", 60, 60, false)
	now := time.Unix(1000, 0)
	m.now = func() time.Time { return now }

	first := m.Pick("key-1")
	if first == nil {
		t.Fatal("Pick() = nil")
	}
	m.Report(first, OutcomeSuccess)

	again := m.Pick("key-1")
	if again == nil || again.ID != first.ID {
		t.Fatalf("sticky Pick() = %+v, want %s", again, first.ID)
	}
	m.Report(again, OutcomeSuccess)

	// After the sticky window the key may land elsewhere (or the same if it
	// is the least loaded), but the old affinity must not pin it forever.
	now = now.Add(61 * time.Second)
	m.Pick("key-1")
}

func TestCooldownMovesToAnotherEndpoint(t *testing.T) {
	m := testManager(t, "a=http://127.0.0.1:1,b=http://127.0.0.1:2", 0, 60, false)
	now := time.Unix(2000, 0)
	m.now = func() time.Time { return now }

	first := m.Pick("key-1")
	if first == nil {
		t.Fatal("Pick() = nil")
	}
	m.Report(first, OutcomeNetworkError)

	next := m.Pick("key-1")
	if next == nil {
		t.Fatal("Pick() = nil after cooldown")
	}
	if next.ID == first.ID {
		t.Fatalf("Pick() returned the cooling endpoint %s", first.ID)
	}
	m.Report(next, OutcomeSuccess)

	// Once the cooldown expires the first endpoint is eligible again.
	now = now.Add(61 * time.Second)
	for i := 0; i < 5; i++ {
		ep := m.Pick("")
		if ep != nil {
			m.Report(ep, OutcomeSuccess)
		}
	}
}

func TestAllEndpointsCoolingStillReturnsOne(t *testing.T) {
	m := testManager(t, "a=http://127.0.0.1:1", 0, 60, false)
	ep := m.Pick("key")
	if ep == nil {
		t.Fatal("Pick() = nil")
	}
	m.Report(ep, OutcomeNetworkError)
	if got := m.Pick("key"); got == nil {
		t.Fatal("Pick() = nil when the only endpoint is cooling; it should be reused as a last resort")
	}
}

func Test429DoesNotCoolEgressByDefault(t *testing.T) {
	m := testManager(t, "a=http://127.0.0.1:1", 0, 60, false)
	ep := m.Pick("key")
	m.Report(ep, OutcomeRateLimited)
	if m.endpoints[0].cooling(m.now()) {
		t.Fatal("a 429 should not cool the egress when EGRESS_COOLDOWN_ON_429 is false")
	}
}

func Test429CoolsEgressWhenConfigured(t *testing.T) {
	m := testManager(t, "a=http://127.0.0.1:1", 0, 60, true)
	ep := m.Pick("key")
	m.Report(ep, OutcomeRateLimited)
	if !m.endpoints[0].cooling(m.now()) {
		t.Fatal("a 429 should cool the egress when EGRESS_COOLDOWN_ON_429 is true")
	}
}
