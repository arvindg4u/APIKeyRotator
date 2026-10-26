package services

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"api-key-rotator/backend/internal/config"
	"api-key-rotator/backend/internal/infrastructure/cache"
	"api-key-rotator/backend/internal/models"
)

// ---------------------------------------------------------------------------
// 测试用的假缓存
// ---------------------------------------------------------------------------

type fakeCacheItem struct {
	value      interface{}
	expiration time.Time
}

// fakeCache 是 cache.CacheInterface 的内存实现，额外支持注入错误以模拟缓存不可用
type fakeCache struct {
	mu    sync.Mutex
	items map[string]fakeCacheItem
	err   error // 非 nil 时所有操作返回该错误
}

var _ cache.CacheInterface = (*fakeCache)(nil)

func newFakeCache() *fakeCache {
	return &fakeCache{items: make(map[string]fakeCacheItem)}
}

func (c *fakeCache) Set(ctx context.Context, key string, value interface{}, expiration time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	exp := time.Time{}
	if expiration > 0 {
		exp = time.Now().Add(expiration)
	}
	c.items[key] = fakeCacheItem{value: value, expiration: exp}
	return nil
}

func (c *fakeCache) Get(ctx context.Context, key string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return "", c.err
	}
	item, ok := c.items[key]
	if !ok {
		return "", cache.ErrNotFound
	}
	if !item.expiration.IsZero() && time.Now().After(item.expiration) {
		delete(c.items, key)
		return "", cache.ErrNotFound
	}
	return fmt.Sprintf("%v", item.value), nil
}

func (c *fakeCache) Del(ctx context.Context, keys ...string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return 0, c.err
	}
	deleted := int64(0)
	for _, key := range keys {
		if _, ok := c.items[key]; ok {
			delete(c.items, key)
			deleted++
		}
	}
	return deleted, nil
}

func (c *fakeCache) Exists(ctx context.Context, key string) (bool, error) {
	_, err := c.Get(ctx, key)
	return err == nil, nil
}

func (c *fakeCache) Incr(ctx context.Context, key string) (int64, error) {
	return c.IncrBy(ctx, key, 1)
}

func (c *fakeCache) IncrBy(ctx context.Context, key string, value int64) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return 0, c.err
	}
	next := value
	if item, ok := c.items[key]; ok {
		expired := !item.expiration.IsZero() && time.Now().After(item.expiration)
		if expired {
			delete(c.items, key)
		} else if current, isInt := item.value.(int64); isInt {
			next = current + value
		}
	}
	c.items[key] = fakeCacheItem{value: next}
	return next, nil
}

func (c *fakeCache) Decr(ctx context.Context, key string) (int64, error) {
	return c.IncrBy(ctx, key, -1)
}

func (c *fakeCache) Ping(ctx context.Context) error { return c.err }

func (c *fakeCache) Info(ctx context.Context) (map[string]interface{}, error) {
	return map[string]interface{}{"type": "fake"}, nil
}

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

func testProxyConfig() *models.ProxyConfig {
	return &models.ProxyConfig{
		ID:   7,
		Name: "test-service",
		Slug: "test-service",
		APIKeys: []models.APIKey{
			{ID: 1, KeyValue: "key-1", IsActive: true},
			{ID: 2, KeyValue: "key-2", IsActive: true},
			{ID: 3, KeyValue: "key-3", IsActive: true},
			{ID: 4, KeyValue: "key-4", IsActive: false}, // 已禁用，永远不该被选中
		},
	}
}

func testRotator(t *testing.T) (*KeyRotator, *fakeCache, *models.ProxyConfig) {
	t.Helper()
	t.Cleanup(resetFallbackCooldowns)
	cacheClient := newFakeCache()
	cfg := &config.Config{KeyCooldownSeconds: 60, KeyMaxCooldownSeconds: 1800}
	return NewKeyRotator(cfg, cacheClient, "Test Rotator"), cacheClient, testProxyConfig()
}

// ---------------------------------------------------------------------------
// 轮换行为
// ---------------------------------------------------------------------------

func TestKeyRotatorSkipsCoolingKeys(t *testing.T) {
	rotator, _, proxyConfig := testRotator(t)

	// key-1 被限流后应当退出轮换，后续请求都只落在健康的 Key 上
	rotator.MarkRateLimited(proxyConfig.ID, "key-1", 30*time.Second)

	for i := 0; i < 20; i++ {
		selected, err := rotator.Next(proxyConfig, nil)
		if err != nil {
			t.Fatalf("Next() error: %v", err)
		}
		if selected.KeyValue == "key-1" {
			t.Fatalf("iteration %d: key 'key-1' is cooling down but was selected again", i)
		}
		if !selected.IsActive {
			t.Fatalf("iteration %d: selected an inactive key %q", i, selected.KeyValue)
		}
	}
}

func TestKeyRotatorFallsBackToSoonestRecoveringKey(t *testing.T) {
	rotator, _, proxyConfig := testRotator(t)

	rotator.MarkRateLimited(proxyConfig.ID, "key-1", 30*time.Second)
	rotator.MarkRateLimited(proxyConfig.ID, "key-2", 5*time.Second)
	rotator.MarkRateLimited(proxyConfig.ID, "key-3", 120*time.Second)

	// 全部冷却时不能放弃，而是选最快恢复的那个
	selected, err := rotator.Next(proxyConfig, nil)
	if err != nil {
		t.Fatalf("Next() error: %v", err)
	}
	if selected.KeyValue != "key-2" {
		t.Fatalf("Next() = %q, want %q (the key that recovers soonest)", selected.KeyValue, "key-2")
	}
}

func TestKeyRotatorNeverRepeatsTriedKey(t *testing.T) {
	rotator, _, proxyConfig := testRotator(t)

	tried := make(map[string]bool)
	count := 0
	for {
		selected, err := rotator.Next(proxyConfig, tried)
		if errors.Is(err, ErrNoAvailableKey) {
			break
		}
		if err != nil {
			t.Fatalf("Next() error: %v", err)
		}
		if tried[selected.KeyValue] {
			t.Fatalf("key %q was handed out twice in the same request", selected.KeyValue)
		}
		tried[selected.KeyValue] = true
		count++
		if count > 10 {
			t.Fatal("Next() did not report exhaustion after all keys were tried")
		}
	}

	if count != 3 {
		t.Fatalf("tried %d keys, want 3 (only active keys, each exactly once)", count)
	}
}

func TestKeyRotatorRoundRobinDistributes(t *testing.T) {
	rotator, _, proxyConfig := testRotator(t)

	counts := make(map[string]int)
	for i := 0; i < 12; i++ {
		selected, err := rotator.Next(proxyConfig, map[string]bool{})
		if err != nil {
			t.Fatalf("Next() error: %v", err)
		}
		counts[selected.KeyValue]++
	}

	for _, key := range []string{"key-1", "key-2", "key-3"} {
		if counts[key] == 0 {
			t.Errorf("key %q was never used in 12 requests (distribution: %v)", key, counts)
		}
	}
	if counts["key-4"] != 0 {
		t.Errorf("inactive key was used: %v", counts)
	}
}

func TestKeyRotatorRequiresActiveKeys(t *testing.T) {
	rotator, _, _ := testRotator(t)

	if _, err := rotator.Next(&models.ProxyConfig{ID: 1}, nil); err == nil {
		t.Fatal("Next() with an empty key pool should fail")
	}
}

func TestKeyRotatorSurvivesCacheOutage(t *testing.T) {
	rotator, cacheClient, proxyConfig := testRotator(t)

	// 缓存挂掉 (例如 Redis 连接中断) 时仍然要能选出 Key，不能让整个代理不可用
	cacheClient.err = errors.New("redis: connection refused")

	selected, err := rotator.Next(proxyConfig, nil)
	if err != nil {
		t.Fatalf("Next() must still work when the cache is unavailable: %v", err)
	}
	if selected.KeyValue == "" {
		t.Fatal("Next() returned an empty key")
	}
}

func TestCooldownSurvivesCacheOutage(t *testing.T) {
	rotator, cacheClient, proxyConfig := testRotator(t)

	// 缓存写失败不能静默关闭冷却: 刚 429 的 Key 必须仍然被降权。
	cacheClient.err = errors.New("redis: connection refused")
	rotator.MarkRateLimited(proxyConfig.ID, "key-1", 30*time.Second)

	if remaining := rotator.RemainingCooldown(proxyConfig.ID, "key-1"); remaining <= 0 {
		t.Fatal("cooldown was lost during a cache outage; the in-process fallback did not engage")
	}

	for i := 0; i < 20; i++ {
		selected, err := rotator.Next(proxyConfig, nil)
		if err != nil {
			t.Fatalf("Next() error during cache outage: %v", err)
		}
		if selected.KeyValue == "key-1" {
			t.Fatal("a key that returned 429 was selected again while the cache was down")
		}
	}
}

func TestClearCooldownClearsFallbackDuringCacheOutage(t *testing.T) {
	rotator, cacheClient, proxyConfig := testRotator(t)

	cacheClient.err = errors.New("redis: connection refused")
	rotator.MarkRateLimited(proxyConfig.ID, "key-1", 30*time.Second)
	if remaining := rotator.RemainingCooldown(proxyConfig.ID, "key-1"); remaining <= 0 {
		t.Fatal("expected a cooldown before clearing it")
	}

	rotator.ClearCooldown(proxyConfig.ID, "key-1")
	if remaining := rotator.RemainingCooldown(proxyConfig.ID, "key-1"); remaining != 0 {
		t.Fatalf("cooldown = %s after ClearCooldown, want 0", remaining)
	}
}

// TestClearCooldownTombstoneSurvivesCacheRecovery covers H3: Del fails while
// Redis is down, then Redis recovers with the old cooldown key still present.
// The administrative clear must keep winning until the stale value is really
// gone; otherwise re-enabling a key silently leaves it deprioritised.
func TestClearCooldownTombstoneSurvivesCacheRecovery(t *testing.T) {
	rotator, cacheClient, proxyConfig := testRotator(t)

	rotator.MarkRateLimited(proxyConfig.ID, "key-1", 30*time.Second)
	if remaining := rotator.RemainingCooldown(proxyConfig.ID, "key-1"); remaining <= 0 {
		t.Fatal("expected a cooldown before clearing it")
	}

	// ClearCooldown's Del fails; the stale cache value remains behind.
	cacheClient.err = errors.New("redis: connection refused")
	rotator.ClearCooldown(proxyConfig.ID, "key-1")

	// Cache "recovers" with the stale cooldown still present.
	cacheClient.err = nil
	if remaining := rotator.RemainingCooldown(proxyConfig.ID, "key-1"); remaining != 0 {
		t.Fatalf("stale cooldown = %s after cache recovery, want 0", remaining)
	}

	// A new 429 after the clear is not suppressed by the tombstone.
	rotator.MarkRateLimited(proxyConfig.ID, "key-1", 10*time.Second)
	if remaining := rotator.RemainingCooldown(proxyConfig.ID, "key-1"); remaining <= 0 {
		t.Fatal("a new 429 after ClearCooldown was masked by the clear tombstone")
	}
}

// ---------------------------------------------------------------------------
// 冷却行为
// ---------------------------------------------------------------------------

func TestRemainingCooldown(t *testing.T) {
	rotator, _, proxyConfig := testRotator(t)

	if got := rotator.RemainingCooldown(proxyConfig.ID, "key-1"); got != 0 {
		t.Fatalf("RemainingCooldown() = %s, want 0 for a healthy key", got)
	}

	rotator.MarkRateLimited(proxyConfig.ID, "key-1", 30*time.Second)
	remaining := rotator.RemainingCooldown(proxyConfig.ID, "key-1")
	if remaining <= 25*time.Second || remaining > 30*time.Second {
		t.Fatalf("RemainingCooldown() = %s, want ~30s", remaining)
	}

	// 冷却只针对被限流的那个 Key
	if got := rotator.RemainingCooldown(proxyConfig.ID, "key-2"); got != 0 {
		t.Fatalf("RemainingCooldown() = %s, want 0 for an untouched key", got)
	}

	rotator.ClearCooldown(proxyConfig.ID, "key-1")
	if got := rotator.RemainingCooldown(proxyConfig.ID, "key-1"); got != 0 {
		t.Fatalf("RemainingCooldown() = %s after ClearCooldown, want 0", got)
	}
}

func TestMarkRateLimitedUsesDefaultWhenNoRetryAfter(t *testing.T) {
	rotator, _, proxyConfig := testRotator(t)

	rotator.MarkRateLimited(proxyConfig.ID, "key-1", 0)

	remaining := rotator.RemainingCooldown(proxyConfig.ID, "key-1")
	if remaining <= 55*time.Second || remaining > 60*time.Second {
		t.Fatalf("cooldown = %s, want the configured default (60s)", remaining)
	}
}

func TestMarkRateLimitedCapsRetryAfter(t *testing.T) {
	t.Cleanup(resetFallbackCooldowns)
	cacheClient := newFakeCache()
	cfg := &config.Config{KeyCooldownSeconds: 60, KeyMaxCooldownSeconds: 120}
	rotator := NewKeyRotator(cfg, cacheClient, "Test Rotator")
	proxyConfig := testProxyConfig()

	// 上游给出一个夸张的 Retry-After 时按 KeyMaxCooldownSeconds 截断
	rotator.MarkRateLimited(proxyConfig.ID, "key-1", 10*time.Hour)

	if remaining := rotator.RemainingCooldown(proxyConfig.ID, "key-1"); remaining > 120*time.Second {
		t.Fatalf("cooldown = %s, want it capped at 120s", remaining)
	}
}

func TestCooldownDisabled(t *testing.T) {
	cacheClient := newFakeCache()
	cfg := &config.Config{KeyCooldownSeconds: 0}
	rotator := NewKeyRotator(cfg, cacheClient, "Test Rotator")
	proxyConfig := testProxyConfig()

	rotator.MarkRateLimited(proxyConfig.ID, "key-1", 30*time.Second)

	if got := rotator.RemainingCooldown(proxyConfig.ID, "key-1"); got != 0 {
		t.Fatalf("RemainingCooldown() = %s, want 0 when cooldown is disabled", got)
	}
	if got := rotator.ShortestCooldown(proxyConfig); got != 0 {
		t.Fatalf("ShortestCooldown() = %s, want 0 when cooldown is disabled", got)
	}
}

func TestShortestCooldown(t *testing.T) {
	rotator, _, proxyConfig := testRotator(t)

	if got := rotator.ShortestCooldown(proxyConfig); got != 0 {
		t.Fatalf("ShortestCooldown() = %s, want 0 when nothing is cooling", got)
	}

	rotator.MarkRateLimited(proxyConfig.ID, "key-1", 90*time.Second)
	rotator.MarkRateLimited(proxyConfig.ID, "key-3", 10*time.Second)

	shortest := rotator.ShortestCooldown(proxyConfig)
	if shortest <= 5*time.Second || shortest > 10*time.Second {
		t.Fatalf("ShortestCooldown() = %s, want ~10s", shortest)
	}
}

// ---------------------------------------------------------------------------
// Retry-After 解析
// ---------------------------------------------------------------------------

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name    string
		header  http.Header
		body    []byte
		want    time.Duration
		wantMin time.Duration
		wantMax time.Duration
	}{
		{
			name:   "retry-after in seconds",
			header: http.Header{"Retry-After": []string{"30"}},
			want:   30 * time.Second,
		},
		{
			name:   "retry-after as http date",
			header: http.Header{"Retry-After": []string{time.Now().UTC().Add(45 * time.Second).Format(http.TimeFormat)}},
			// HTTP 时间只精确到秒，允许一秒的误差
			wantMin: 40 * time.Second,
			wantMax: 46 * time.Second,
		},
		{
			name: "gemini retryDelay in error body",
			body: []byte(`{"error":{"code":429,"message":"quota exceeded","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"28s"}]}}`),
			want: 28 * time.Second,
		},
		{
			name:    "nothing to parse",
			body:    []byte(`{"error":{"message":"something went wrong"}}`),
			want:    0,
			wantMin: 0,
			wantMax: 0,
		},
		{
			name:    "invalid body is ignored",
			body:    []byte(`not json at all`),
			want:    0,
			wantMin: 0,
			wantMax: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseRetryAfter(tt.header, tt.body)
			if tt.wantMin != 0 || tt.wantMax != 0 {
				if got < tt.wantMin || got > tt.wantMax {
					t.Fatalf("ParseRetryAfter() = %s, want between %s and %s", got, tt.wantMin, tt.wantMax)
				}
				return
			}
			if got != tt.want {
				t.Fatalf("ParseRetryAfter() = %s, want %s", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Key 注入：首次请求与重试必须落在同一个位置
// ---------------------------------------------------------------------------

func TestInjectUpstreamKeyReplacesPreviousKey(t *testing.T) {
	proxyConfig := &models.ProxyConfig{}
	target := &TargetRequest{}

	if !InjectUpstreamKey(target, proxyConfig, "openai_compatible", "key-1") {
		t.Fatal("InjectUpstreamKey() = false, want true for the default OpenAI header")
	}
	if target.Headers["Authorization"] != "Bearer key-1" {
		t.Fatalf("Authorization = %q, want %q", target.Headers["Authorization"], "Bearer key-1")
	}

	// 换 Key 重试后只应留下新的 Key，不能两个位置各留一个
	if !InjectUpstreamKey(target, proxyConfig, "openai_compatible", "key-2") {
		t.Fatal("InjectUpstreamKey() = false on retry")
	}
	if target.Headers["Authorization"] != "Bearer key-2" {
		t.Fatalf("Authorization = %q after retry, want %q", target.Headers["Authorization"], "Bearer key-2")
	}
	if len(target.Headers) != 1 {
		t.Fatalf("headers = %v, want exactly one entry", target.Headers)
	}
}

func TestInjectUpstreamKeyNormalizesLocationCasing(t *testing.T) {
	// 数据库里可能存成 "Query"/"HEADER"，大小写不该影响注入位置
	proxyConfig := &models.ProxyConfig{APIKeyLocation: strPtr("Query")}
	target := &TargetRequest{}

	if !InjectUpstreamKey(target, proxyConfig, "gemini_native", "key-1") {
		t.Fatal("InjectUpstreamKey() = false for a gemini query key")
	}
	if target.Params["key"] != "key-1" {
		t.Fatalf("query param key = %q, want %q", target.Params["key"], "key-1")
	}
	if len(target.Headers) != 0 {
		t.Fatalf("headers = %v, want none when the key goes in the query", target.Headers)
	}
}

func TestInjectUpstreamKeyNoopWithoutConfig(t *testing.T) {
	target := &TargetRequest{Headers: map[string]string{}, Params: map[string]string{}}

	if InjectUpstreamKey(target, &models.ProxyConfig{}, "generic", "key-1") {
		t.Fatal("InjectUpstreamKey() = true, want false when nothing is configured")
	}
	if len(target.Headers) != 0 || len(target.Params) != 0 {
		t.Fatalf("target = %+v, want untouched", target)
	}
}
