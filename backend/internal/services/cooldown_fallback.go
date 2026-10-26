package services

import (
	"sync"
	"time"

	"api-key-rotator/backend/internal/config"
	"api-key-rotator/backend/internal/logger"
)

// cooldownFallback keeps rate-limit cooldowns in process memory.
//
// The configured cache (Redis) is the shared, preferred store. If it is
// unreachable, cooldown must still work locally: otherwise every 429'd key is
// immediately considered healthy again and a single exhausted key keeps being
// retried. The fallback is deliberately an overlay, not a replacement: when
// the cache recovers, its value and the local value are both honoured until
// the later one expires.
//
// cleared is a tombstone overlay used when ClearCooldown could not delete the
// shared cache entry. It must mask a stale cache value until the cache recovers
// (or until the maximum possible cooldown expires), otherwise re-enabling a
// key would leave it deprioritised by a value that was never deleted.
var cooldownFallback = struct {
	sync.Mutex
	entries map[string]time.Time
	cleared map[string]time.Time
}{
	entries: make(map[string]time.Time),
	cleared: make(map[string]time.Time),
}

// cooldownClearMarkerTTL is at least as long as any cooldown MarkRateLimited
// can write (it clamps to DefaultMaxKeyCooldownSeconds). Keeping the tombstone
// that long guarantees a stale cache entry cannot resurface after recovery.
const cooldownClearMarkerTTL = time.Duration(config.DefaultMaxKeyCooldownSeconds) * time.Second

func setFallbackCooldown(cacheKey string, expiresAt time.Time) {
	cooldownFallback.Lock()
	defer cooldownFallback.Unlock()
	cooldownFallback.entries[cacheKey] = expiresAt
	// A real 429 is newer than an earlier administrative clear. Remove the
	// tombstone so the new cooldown is visible immediately.
	delete(cooldownFallback.cleared, cacheKey)
}

func fallbackCooldownRemaining(cacheKey string) time.Duration {
	cooldownFallback.Lock()
	defer cooldownFallback.Unlock()

	expiresAt, ok := cooldownFallback.entries[cacheKey]
	if !ok {
		return 0
	}
	remaining := time.Until(expiresAt)
	if remaining <= 0 {
		delete(cooldownFallback.entries, cacheKey)
		return 0
	}
	return remaining
}

// beginClearCooldown atomically drops the in-process cooldown and installs a
// tombstone before the cache delete is attempted. If the delete fails, the
// tombstone remains and RemainingCooldown ignores the stale shared value.
func beginClearCooldown(cacheKey string) {
	cooldownFallback.Lock()
	defer cooldownFallback.Unlock()
	delete(cooldownFallback.entries, cacheKey)
	cooldownFallback.cleared[cacheKey] = time.Now().Add(cooldownClearMarkerTTL)
}

// cancelClearCooldown removes the tombstone after a successful cache delete.
// If a new 429 raced with the clear, setFallbackCooldown already removed it;
// this call is then a no-op and the new cooldown stays visible.
func cancelClearCooldown(cacheKey string) {
	cooldownFallback.Lock()
	defer cooldownFallback.Unlock()
	delete(cooldownFallback.cleared, cacheKey)
}

func cooldownCleared(cacheKey string) bool {
	cooldownFallback.Lock()
	defer cooldownFallback.Unlock()

	expiresAt, ok := cooldownFallback.cleared[cacheKey]
	if !ok {
		return false
	}
	if time.Now().Before(expiresAt) {
		return true
	}
	delete(cooldownFallback.cleared, cacheKey)
	return false
}

// resetFallbackCooldowns is used by tests to keep process-local state from
// leaking between cases.
func resetFallbackCooldowns() {
	cooldownFallback.Lock()
	defer cooldownFallback.Unlock()
	cooldownFallback.entries = make(map[string]time.Time)
	cooldownFallback.cleared = make(map[string]time.Time)
}

const cacheFailureLogInterval = 30 * time.Second

var cacheFailureLog = struct {
	sync.Mutex
	last time.Time
}{}

// warnCacheFailure surfaces a cache outage without flooding the log: cooldown
// lookups happen for every key on every request, so an unthrottled warning
// would drown out everything else while Redis is down.
func warnCacheFailure(prefix, operation string, err error) {
	cacheFailureLog.Lock()
	defer cacheFailureLog.Unlock()

	now := time.Now()
	if !cacheFailureLog.last.IsZero() && now.Sub(cacheFailureLog.last) < cacheFailureLogInterval {
		return
	}
	cacheFailureLog.last = now
	logger.Warningf("%s: cache %s failed (%v); using in-process cooldown fallback until the cache recovers",
		prefix, operation, err)
}
