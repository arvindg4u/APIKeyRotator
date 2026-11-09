// Package egress selects the outbound network path for upstream requests.
//
// Each endpoint is an HTTP CONNECT proxy (for example a userspace WireGuard
// client such as wireproxy). The manager keeps a sticky affinity between a
// provider key and an endpoint, tracks failures, and applies cooldowns so a
// burned egress IP is not reused immediately.
package egress

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"api-key-rotator/backend/internal/config"
	"api-key-rotator/backend/internal/logger"
)

// Outcome classifies the result of an attempt that used an endpoint.
type Outcome int

const (
	OutcomeSuccess Outcome = iota
	OutcomeNetworkError
	OutcomeRateLimited
	OutcomeForbidden
	OutcomeServerError
)

// Endpoint is one outbound proxy.
type Endpoint struct {
	ID     string `json:"id"`
	URL    string `json:"url"`
	Region string `json:"region,omitempty"`
	ASN    string `json:"asn,omitempty"`
	Weight int    `json:"weight,omitempty"`

	proxy         *url.URL
	cooldownUntil time.Time
	failures      int
	inFlight      int
}

// ProxyURL returns the parsed proxy URL, or nil for a direct request.
func (e *Endpoint) ProxyURL() *url.URL {
	if e == nil {
		return nil
	}
	return e.proxy
}

type affinityEntry struct {
	endpointID string
	expiresAt  time.Time
}

// Manager holds the endpoint pool and the affinity/cooldown state.
type Manager struct {
	mu sync.Mutex

	enabled       bool
	endpoints     []*Endpoint
	byID          map[string]*Endpoint
	sticky        time.Duration
	cooldownBase  time.Duration
	cooldownMax   time.Duration
	cooldownOn429 bool

	affinity map[string]affinityEntry
	rr       uint64
	now      func() time.Time
}

// New builds a manager from configuration. A malformed endpoint list disables
// egress rather than failing the request path.
func New(cfg *config.Config) *Manager {
	m := &Manager{
		byID:          make(map[string]*Endpoint),
		affinity:      make(map[string]affinityEntry),
		cooldownBase:  time.Duration(cfg.EgressCooldownSeconds) * time.Second,
		cooldownMax:   time.Hour,
		cooldownOn429: cfg.EgressCooldownOn429,
		now:           time.Now,
	}
	if m.cooldownBase <= 0 {
		m.cooldownBase = 5 * time.Minute
	}
	if cfg.EgressStickySeconds > 0 {
		m.sticky = time.Duration(cfg.EgressStickySeconds) * time.Second
	}
	if !cfg.EgressEnabled {
		return m
	}

	endpoints, err := parseEndpoints(cfg.EgressEndpoints)
	if err != nil {
		logger.Warningf("egress: disabling egress because the endpoint list is invalid: %v", err)
		return m
	}
	if len(endpoints) == 0 {
		logger.Warningf("egress: EGRESS_ENABLED=true but EGRESS_ENDPOINTS is empty; using direct connections")
		return m
	}

	for _, ep := range endpoints {
		m.byID[ep.ID] = ep
	}
	m.endpoints = endpoints
	m.enabled = true
	logger.Infof("egress: loaded %d endpoint(s), sticky %s, cooldown base %s", len(endpoints), m.sticky, m.cooldownBase)
	return m
}

// Enabled reports whether at least one endpoint is configured.
func (m *Manager) Enabled() bool {
	return m != nil && m.enabled
}

// Pick chooses an endpoint for an affinity key (usually provider + hashed key).
// It returns nil when egress is disabled, which means "connect directly".
func (m *Manager) Pick(key string) *Endpoint {
	if m == nil || !m.enabled {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()

	if key != "" {
		if entry, ok := m.affinity[key]; ok {
			if now.Before(entry.expiresAt) {
				if ep := m.byID[entry.endpointID]; ep != nil && !ep.cooling(now) {
					ep.inFlight++
					return ep
				}
			}
			delete(m.affinity, key)
		}
	}

	var best *Endpoint
	var cooling *Endpoint
	for _, ep := range m.endpoints {
		if ep.cooling(now) {
			if cooling == nil || ep.cooldownUntil.Before(cooling.cooldownUntil) {
				cooling = ep
			}
			continue
		}
		if best == nil {
			best = ep
			continue
		}
		if ep.inFlight < best.inFlight ||
			(ep.inFlight == best.inFlight && ep.Weight > best.Weight) {
			best = ep
		}
	}
	if best == nil {
		// Every endpoint is cooling. Reuse the one that recovers soonest
		// instead of failing the request outright.
		best = cooling
	}
	if best == nil {
		return nil
	}

	best.inFlight++
	if key != "" && m.sticky > 0 {
		m.affinity[key] = affinityEntry{endpointID: best.ID, expiresAt: now.Add(m.sticky)}
	}
	return best
}

// Report records the result of using an endpoint.
func (m *Manager) Report(ep *Endpoint, outcome Outcome) {
	if m == nil || !m.enabled || ep == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if ep.inFlight > 0 {
		ep.inFlight--
	}

	switch outcome {
	case OutcomeSuccess:
		ep.failures = 0
		ep.cooldownUntil = time.Time{}
	case OutcomeNetworkError, OutcomeForbidden:
		ep.failures++
		ep.cooldownUntil = m.now().Add(m.backoff(ep.failures))
	case OutcomeRateLimited:
		if m.cooldownOn429 {
			ep.failures++
			ep.cooldownUntil = m.now().Add(m.backoff(ep.failures))
		}
	case OutcomeServerError:
		// A 5xx is not evidence that the egress IP is burned; leave it in rotation.
	}
}

func (m *Manager) backoff(failures int) time.Duration {
	d := m.cooldownBase
	for i := 1; i < failures; i++ {
		d *= 2
		if d >= m.cooldownMax {
			return m.cooldownMax
		}
	}
	if d > m.cooldownMax {
		return m.cooldownMax
	}
	return d
}

func (e *Endpoint) cooling(now time.Time) bool {
	return !e.cooldownUntil.IsZero() && now.Before(e.cooldownUntil)
}

// AffinityKey builds a stable, non-secret key for the sticky map.
func AffinityKey(scope, secret string) string {
	if secret == "" {
		return scope
	}
	// Hash the secret so process dumps and logs never contain a raw key.
	sum := sha256.Sum256([]byte(secret))
	return scope + "|" + hex.EncodeToString(sum[:8])
}

func parseEndpoints(raw string) ([]*Endpoint, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	var endpoints []*Endpoint
	if strings.HasPrefix(raw, "[") {
		if err := json.Unmarshal([]byte(raw), &endpoints); err != nil {
			return nil, fmt.Errorf("invalid EGRESS_ENDPOINTS JSON: %w", err)
		}
	} else {
		for _, item := range strings.Split(raw, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			ep := &Endpoint{}
			if id, target, ok := strings.Cut(item, "="); ok {
				ep.ID = strings.TrimSpace(id)
				ep.URL = strings.TrimSpace(target)
			} else {
				ep.URL = item
			}
			endpoints = append(endpoints, ep)
		}
	}

	for i, ep := range endpoints {
		if ep.URL == "" {
			return nil, fmt.Errorf("endpoint %d has no url", i)
		}
		parsed, err := url.Parse(ep.URL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return nil, fmt.Errorf("endpoint %q is not a valid http(s) proxy URL", ep.URL)
		}
		ep.proxy = parsed
		if ep.ID == "" {
			ep.ID = parsed.Host
		}
		if ep.Weight == 0 {
			ep.Weight = 1
		}
	}
	return endpoints, nil
}
