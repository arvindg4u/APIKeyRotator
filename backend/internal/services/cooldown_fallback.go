package services

import (
	"sync"
	"time"

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
var cooldownFallback = struct {
	sync.Mutex
	entries map[string]time.Time
}{entries: make(map[string]time.Time)}

func setFallbackCooldown(cacheKey string, expiresAt time.Time) {
	cooldownFallback.Lock()
	defer cooldownFallback.Unlock()
	cooldownFallback.entries[cacheKey] = expiresAt
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

func clearFallbackCooldown(cacheKey string) {
	cooldownFallback.Lock()
	defer cooldownFallback.Unlock()
	delete(cooldownFallback.entries, cacheKey)
}

// resetFallbackCooldowns is used by tests to keep process-local state from
// leaking between cases.
func resetFallbackCooldowns() {
	cooldownFallback.Lock()
	defer cooldownFallback.Unlock()
	cooldownFallback.entries = make(map[string]time.Time)
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
