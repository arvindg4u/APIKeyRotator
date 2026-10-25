package cache

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound 表示缓存键不存在。
//
// 调用方必须能区分 "键不存在" 和 "缓存不可达"：前者是正常状态，
// 后者需要告警并启用兜底逻辑，不能静默当成 "没有冷却记录"。
var ErrNotFound = errors.New("key not found")

// CacheInterface 缓存接口
type CacheInterface interface {
	// 基础操作
	Set(ctx context.Context, key string, value interface{}, expiration time.Duration) error
	Get(ctx context.Context, key string) (string, error)
	Del(ctx context.Context, keys ...string) (int64, error)
	Exists(ctx context.Context, key string) (bool, error)

	// 计数器操作
	Incr(ctx context.Context, key string) (int64, error)
	IncrBy(ctx context.Context, key string, value int64) (int64, error)
	Decr(ctx context.Context, key string) (int64, error)

	// 连接测试
	Ping(ctx context.Context) error

	// 健康检查和统计
	Info(ctx context.Context) (map[string]interface{}, error)
}

// Manager 缓存管理器接口
type Manager interface {
	Initialize() (CacheInterface, error)
	Close() error
}
