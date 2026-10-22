package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	// DefaultKeyCooldownSeconds 上游返回 429 但没有给出 Retry-After 时使用的默认冷却时长 (秒)。
	// 设置为 0 可关闭冷却机制，退化为"纯轮询 + 失败即换下一个 Key"。
	DefaultKeyCooldownSeconds = 60
	// DefaultMaxKeyCooldownSeconds 单次冷却的上限 (秒)，避免上游给出超长的 Retry-After 把 Key 永久封存
	DefaultMaxKeyCooldownSeconds = 1800
	// DefaultUpstreamUserAgent 客户端没带 User-Agent 时使用的兜底 UA。
	// 不填的话 Go 会发出 "Go-http-client/1.1"，不少 WAF/CDN 会直接拦截并返回 403。
	DefaultUpstreamUserAgent = "Mozilla/5.0 (compatible; APIKeyRotator/1.0; +https://github.com/arvindg4u/APIKeyRotator)"
)

// Config 应用配置结构
type Config struct {
	// 数据库配置
	DBType        string // "mysql" 或 "sqlite"
	DatabaseURL   string // MySQL连接字符串
	DatabasePath  string // SQLite文件路径

	// 缓存配置
	CacheType     string // "redis" 或 "memory"
	RedisURL      string
	RedisHost     string
	RedisPort     int
	RedisPassword string

	// 服务器配置
	Port string

	// JWT配置
	JWTSecret string

	// 管理员配置
	AdminUsername string
	AdminPassword string
	AdminUser     string // 别名，兼容性

	// 代理配置
	ProxyTimeout       int
	GlobalProxyKeys    string // 逗号分隔的多个密钥，也支持单个密钥
	ProxyPublicBaseURL string

	// Key 冷却配置 (上游返回 429 之后，该 Key 被降权的时长，单位秒)
	// 设置为 0 可以关闭冷却，退化为"每次请求纯轮询 + 失败即换下一个 Key"
	KeyCooldownSeconds    int
	KeyMaxCooldownSeconds int

	// 上游请求的默认 User-Agent。
	// 客户端没带 UA 时使用，避免发出 Go 默认的 "Go-http-client/1.1" 被 WAF/CDN 拦截。
	UpstreamUserAgent string

	// 日志配置
	LogLevel string
}

// GetGlobalProxyKeys 获取所有有效的代理密钥列表
// 支持单个密钥或逗号分隔的多个密钥
func (c *Config) GetGlobalProxyKeys() []string {
	// 分割逗号分隔的密钥，并去除空白字符
	keys := strings.Split(c.GlobalProxyKeys, ",")
	var result []string
	for _, key := range keys {
		trimmed := strings.TrimSpace(key)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// Load 加载配置
func Load() *Config {
	adminUsername := getEnv("ADMIN_USERNAME", "admin")

	// 自动检测数据库类型
	dbType := detectDatabaseType()

	// 自动检测缓存类型
	cacheType := detectCacheType()

	config := &Config{
		DBType:             dbType,
		DatabaseURL:        buildDatabaseURL(),
		DatabasePath:       getEnv("DATABASE_PATH", "/app/data/api_key_rotator.db"),
		CacheType:          cacheType,
		RedisURL:           getEnv("REDIS_URL", "redis://localhost:6379/0"),
		RedisHost:          getEnv("REDIS_HOST", "localhost"),
		RedisPort:          getEnvAsInt("REDIS_PORT", 6379),
		RedisPassword:      getEnv("REDIS_PASSWORD", ""),
		Port:               getServerPort(),
		JWTSecret:          getEnv("JWT_SECRET", "your-secret-key"),
		AdminUsername:      adminUsername,
		AdminPassword:      getEnv("ADMIN_PASSWORD", "admin123"),
		AdminUser:          adminUsername, // 别名，兼容性
		ProxyTimeout:       getEnvAsInt("PROXY_TIMEOUT", 30),
		GlobalProxyKeys:    getEnv("GLOBAL_PROXY_KEYS", "your-global-proxy-key"),
		ProxyPublicBaseURL: getEnv("PROXY_PUBLIC_BASE_URL", "http://localhost:8000"),

		// 上游限流 (429) 之后的 Key 冷却时长。上游给出 Retry-After 时以其为准。
		KeyCooldownSeconds:    getEnvAsInt("KEY_COOLDOWN_SECONDS", DefaultKeyCooldownSeconds),
		KeyMaxCooldownSeconds: getEnvAsInt("KEY_MAX_COOLDOWN_SECONDS", DefaultMaxKeyCooldownSeconds),

		// 客户端没带 User-Agent 时的兜底值
		UpstreamUserAgent: getUpstreamUserAgent(),

		LogLevel: getEnv("LOG_LEVEL", "info"),
	}

	return config
}

// getServerPort resolves the listen port.
// PORT wins when set (Render/Heroku/Fly inject it; the root Dockerfile also
// bakes BACKEND_PORT=8000, which must not override the platform port).
// Otherwise BACKEND_PORT applies, defaulting to 8000 for local runs.
func getServerPort() string {
	if value := os.Getenv("PORT"); value != "" {
		return value
	}
	return getEnv("BACKEND_PORT", "8000")
}

// getUpstreamUserAgent 解析转发给上游的默认 User-Agent。
// 未设置该环境变量时使用内置默认值; 显式设置为空字符串则退回 Go 的默认 UA。
func getUpstreamUserAgent() string {
	if value, ok := os.LookupEnv("UPSTREAM_USER_AGENT"); ok {
		return value
	}
	return DefaultUpstreamUserAgent
}

// buildDatabaseURL 构建数据库连接字符串
func buildDatabaseURL() string {
	// 优先使用完整的DATABASE_URL
	if databaseURL := os.Getenv("DATABASE_URL"); databaseURL != "" {
		return databaseURL
	}
	
	// 否则从分离的环境变量构建
	dbHost := getEnv("DB_HOST", "localhost")
	dbPort := getEnv("DB_PORT", "3306")
	dbUser := getEnv("DB_USER", "root")
	dbPassword := getEnv("DB_PASSWORD", "password")
	dbName := getEnv("DB_NAME", "api_key_rotator")
	
	return fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=True&loc=Local",
		dbUser, dbPassword, dbHost, dbPort, dbName)
}

// getEnv 获取环境变量，如果不存在则返回默认值
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// getEnvAsInt 获取环境变量并转换为整数，如果不存在或转换失败则返回默认值
func getEnvAsInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intValue, err := strconv.Atoi(value); err == nil {
			return intValue
		}
	}
	return defaultValue
}

// detectDatabaseType 自动检测数据库类型
// 检测到MySQL相关环境变量则使用MySQL，否则默认使用SQLite
func detectDatabaseType() string {
	// 检查完整的数据库URL
	if os.Getenv("DATABASE_URL") != "" {
		databaseURL := os.Getenv("DATABASE_URL")
		if strings.Contains(databaseURL, "mysql") {
			return "mysql"
		}
	}

	// 检查MySQL特定的环境变量
	if os.Getenv("DB_HOST") != "" ||
	   os.Getenv("DB_USER") != "" ||
	   os.Getenv("MYSQL_DB_HOST") != "" ||
	   os.Getenv("MYSQL_HOST") != "" {
		return "mysql"
	}

	// 默认使用SQLite
	return "sqlite"
}

// detectCacheType 自动检测缓存类型
// 检测到Redis相关环境变量则使用Redis，否则默认使用内存缓存
func detectCacheType() string {
	// 检查完整的Redis URL
	if os.Getenv("REDIS_URL") != "" {
		return "redis"
	}

	// 检查Redis特定的环境变量
	if os.Getenv("REDIS_HOST") != "" ||
	   os.Getenv("REDIS_PORT") != "" {
		return "redis"
	}

	// 默认使用内存缓存
	return "memory"
}