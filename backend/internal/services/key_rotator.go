package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	"api-key-rotator/backend/internal/config"
	"api-key-rotator/backend/internal/infrastructure/cache"
	"api-key-rotator/backend/internal/logger"
	"api-key-rotator/backend/internal/models"
	"api-key-rotator/backend/internal/utils"
)

// ErrNoAvailableKey 表示密钥池中已经没有可以再尝试的 Key
// (本次请求已经把可用的 Key 都试过一遍)
var ErrNoAvailableKey = errors.New("no available API key")

// KeyRotator 负责从密钥池中选择/轮换上游 Key, 并在上游限流 (429) 后让该 Key 进入冷却。
//
// 设计要点:
//  1. 只靠缓存里的自增计数器做轮询是不够的 —— 并发请求之间会互相挤占计数,
//     重试时很容易又拿到刚刚 429 的那个 Key, 于是"换 Key 重试"形同虚设。
//     因此 Next 接受一个 tried 集合, 保证同一次请求内绝不重复尝试同一个 Key。
//  2. 429 说明上游已经在拒绝这个 Key, 冷却期内应当优先使用其它 Key,
//     否则每个请求都会再踩一次同一个坑 (表现为大量 429 + 无谓的延迟)。
//  3. 冷却只是"降权", 不是"禁用": 当所有 Key 都在冷却时, 仍然会挑最快恢复的那个继续尝试,
//     绝不会因为冷却而让代理直接不可用。
type KeyRotator struct {
	cfg         *config.Config
	cacheClient cache.CacheInterface
	logPrefix   string
}

// NewKeyRotator 创建一个 Key 轮换器
func NewKeyRotator(cfg *config.Config, cacheClient cache.CacheInterface, logPrefix string) *KeyRotator {
	return &KeyRotator{
		cfg:         cfg,
		cacheClient: cacheClient,
		logPrefix:   logPrefix,
	}
}

// cooldownEnabled 是否启用了冷却机制 (KEY_COOLDOWN_SECONDS <= 0 或缓存不可用时自动关闭)
func (r *KeyRotator) cooldownEnabled() bool {
	return r != nil && r.cacheClient != nil && r.cfg != nil && r.cfg.KeyCooldownSeconds > 0
}

// cooldownCacheKey 冷却状态在缓存中的键名。
// 用 Key 内容的哈希做后缀, 避免把真实的 API Key 写进缓存/Redis。
func (r *KeyRotator) cooldownCacheKey(configID int32, keyValue string) string {
	sum := sha256.Sum256([]byte(keyValue))
	return fmt.Sprintf("proxy_config:%d:key_cooldown:%s", configID, hex.EncodeToString(sum[:8]))
}

// rotationCacheKey 轮询计数器在缓存中的键名
func (r *KeyRotator) rotationCacheKey(configID int32) string {
	return fmt.Sprintf("proxy_config:%d:key_index", configID)
}

// RemainingCooldown 返回某个 Key 剩余的冷却时间, 0 表示不在冷却中
func (r *KeyRotator) RemainingCooldown(configID int32, keyValue string) time.Duration {
	if !r.cooldownEnabled() {
		return 0
	}

	raw, err := r.cacheClient.Get(context.Background(), r.cooldownCacheKey(configID, keyValue))
	if err != nil || raw == "" {
		return 0
	}

	expiresAt, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0
	}

	remaining := time.Until(time.Unix(expiresAt, 0))
	if remaining <= 0 {
		return 0
	}
	return remaining
}

// MarkRateLimited 标记某个 Key 进入冷却。
// retryAfter <= 0 时 (上游没有给出建议值) 使用配置项 KEY_COOLDOWN_SECONDS。
func (r *KeyRotator) MarkRateLimited(configID int32, keyValue string, retryAfter time.Duration) {
	if !r.cooldownEnabled() {
		return
	}

	cooldown := retryAfter
	if cooldown <= 0 {
		cooldown = time.Duration(r.cfg.KeyCooldownSeconds) * time.Second
	}
	if max := time.Duration(config.DefaultMaxKeyCooldownSeconds) * time.Second; cooldown > max {
		cooldown = max
	}
	if r.cfg.KeyMaxCooldownSeconds > 0 {
		if max := time.Duration(r.cfg.KeyMaxCooldownSeconds) * time.Second; cooldown > max {
			cooldown = max
		}
	}

	ctx := context.Background()
	expiresAt := time.Now().Add(cooldown).Unix()
	if err := r.cacheClient.Set(ctx, r.cooldownCacheKey(configID, keyValue), expiresAt, cooldown); err != nil {
		logger.Warningf("%s: failed to record rate-limit cooldown: %v", r.logPrefix, err)
		return
	}

	logger.Warningf("%s: key %s is cooling down for %s (status 429)",
		r.logPrefix, utils.MaskAPIKeyDefault(keyValue), cooldown)
}

// ClearCooldown 清除某个 Key 的冷却状态 (例如管理员在后台重新启用了这个 Key)
func (r *KeyRotator) ClearCooldown(configID int32, keyValue string) {
	if !r.cooldownEnabled() {
		return
	}
	if _, err := r.cacheClient.Del(context.Background(), r.cooldownCacheKey(configID, keyValue)); err != nil {
		logger.Warningf("%s: failed to clear rate-limit cooldown: %v", r.logPrefix, err)
	}
}

// ShortestCooldown 返回密钥池中最短的重试等待时间, 用于给客户端回 Retry-After。
// 没有任何 Key 处于冷却时返回 0。
func (r *KeyRotator) ShortestCooldown(proxyConfig *models.ProxyConfig) time.Duration {
	if !r.cooldownEnabled() {
		return 0
	}

	var shortest time.Duration
	for _, key := range ActiveAPIKeys(proxyConfig) {
		remaining := r.RemainingCooldown(proxyConfig.ID, key.KeyValue)
		if remaining > 0 && (shortest == 0 || remaining < shortest) {
			shortest = remaining
		}
	}
	return shortest
}

// Next 选出下一个应当使用的 Key。
//
// tried 记录本次请求已经尝试过的 Key (用 KeyValue 去重), 这些 Key 不会再被选中;
// 处于冷却期的 Key 会被降权: 只有在没有其它选择时才会使用, 并优先挑最快恢复的那个。
// 所有 Key 都试过之后返回 ErrNoAvailableKey。
func (r *KeyRotator) Next(proxyConfig *models.ProxyConfig, tried map[string]bool) (models.APIKey, error) {
	activeKeys := ActiveAPIKeys(proxyConfig)
	if len(activeKeys) == 0 {
		return models.APIKey{}, fmt.Errorf("no active API keys for this service")
	}

	var healthy []models.APIKey
	var cooling []models.APIKey
	for _, key := range activeKeys {
		if tried[key.KeyValue] {
			continue
		}
		if r.RemainingCooldown(proxyConfig.ID, key.KeyValue) > 0 {
			cooling = append(cooling, key)
			continue
		}
		healthy = append(healthy, key)
	}

	if len(healthy) > 0 {
		return r.pickRoundRobin(proxyConfig, healthy), nil
	}

	if len(cooling) > 0 {
		// 所有 Key 都在冷却: 退而求其次, 用最快恢复的那个继续尝试,
		// 而不是直接放弃 (上游的限流窗口可能已经过去)
		best := cooling[0]
		bestRemaining := r.RemainingCooldown(proxyConfig.ID, best.KeyValue)
		for _, key := range cooling[1:] {
			if remaining := r.RemainingCooldown(proxyConfig.ID, key.KeyValue); remaining < bestRemaining {
				best, bestRemaining = key, remaining
			}
		}
		logger.Warningf("%s: all %d key(s) are cooling down, using the one that recovers in %s",
			r.logPrefix, len(cooling), bestRemaining)
		return best, nil
	}

	return models.APIKey{}, ErrNoAvailableKey
}

// pickRoundRobin 在候选 Key 中轮询选择一个, 让请求尽量均匀分布
func (r *KeyRotator) pickRoundRobin(proxyConfig *models.ProxyConfig, candidates []models.APIKey) models.APIKey {
	if len(candidates) == 0 {
		return models.APIKey{}
	}
	if len(candidates) == 1 {
		return candidates[0]
	}

	index := 0
	if r.cacheClient != nil {
		counter, err := r.cacheClient.Incr(context.Background(), r.rotationCacheKey(proxyConfig.ID))
		if err != nil {
			// 缓存不可用 (例如 Redis 连接中断) 时退化为随机选择,
			// 不能因为计数器拿不到就让整个代理不可用
			logger.Warningf("%s: rotation counter unavailable (%v), falling back to random selection", r.logPrefix, err)
			index = rand.Intn(len(candidates))
		} else {
			index = int((counter - 1) % int64(len(candidates)))
			if index < 0 {
				index += len(candidates)
			}
		}
	} else {
		index = rand.Intn(len(candidates))
	}

	return candidates[index]
}

// ParseRetryAfter 从上游响应中解析建议的重试等待时间, 解析不到时返回 0。
// 优先使用 Retry-After 响应头 (秒数或 HTTP 时间), 兼容 Gemini 在错误体中返回的 retryDelay。
func ParseRetryAfter(header http.Header, body []byte) time.Duration {
	if header != nil {
		if duration := parseRetryAfterValue(header.Get("Retry-After")); duration > 0 {
			return duration
		}
	}

	// Gemini 风格: {"error":{"code":429,"details":[{"retryDelay":"28s"}], ...}}
	if len(body) > 0 && json.Valid(body) {
		var payload struct {
			RetryDelay string `json:"retryDelay"`
			Error      *struct {
				RetryDelay string `json:"retryDelay"`
				Details    []struct {
					RetryDelay string `json:"retryDelay"`
				} `json:"details"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &payload); err == nil {
			if payload.Error != nil {
				for _, detail := range payload.Error.Details {
					if duration, err := time.ParseDuration(detail.RetryDelay); err == nil && duration > 0 {
						return duration
					}
				}
				if duration, err := time.ParseDuration(payload.Error.RetryDelay); err == nil && duration > 0 {
					return duration
				}
			}
			if duration, err := time.ParseDuration(payload.RetryDelay); err == nil && duration > 0 {
				return duration
			}
		}
	}

	return 0
}

// parseRetryAfterValue 解析 Retry-After 的取值: 秒数 (如 "30") 或 HTTP 时间 (如 "Wed, 21 Oct 2015 07:28:00 GMT")
func parseRetryAfterValue(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}

	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}

	if moment, err := http.ParseTime(value); err == nil {
		if duration := time.Until(moment); duration > 0 {
			return duration
		}
	}

	return 0
}
