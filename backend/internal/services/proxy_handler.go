package services

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"api-key-rotator/backend/internal/infrastructure/cache"
	"api-key-rotator/backend/internal/config"
	"api-key-rotator/backend/internal/logger"
	"api-key-rotator/backend/internal/models"
	"api-key-rotator/backend/internal/utils"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// TargetRequest 封装准备好的、即将被转发的请求信息
type TargetRequest struct {
	Method  string
	URL     string
	Headers map[string]string
	Params  map[string]string
	Body    []byte
}

// BaseProxyHandler 代理处理器的抽象基类
type BaseProxyHandler struct {
	cfg         *config.Config
	db          *gorm.DB
	cacheClient cache.CacheInterface
	C           *gin.Context // 导出字段
	Slug        string       // 导出字段
	action      string
	logPrefix   string
}

// NewBaseProxyHandler 创建基础代理处理器
func NewBaseProxyHandler(cfg *config.Config, db *gorm.DB, cacheClient cache.CacheInterface, c *gin.Context, slug, action string) *BaseProxyHandler {
	return &BaseProxyHandler{
		cfg:         cfg,
		db:          db,
		cacheClient: cacheClient,
		C:           c,
		Slug:        slug,
		action:      action,
		logPrefix:   fmt.Sprintf("Proxy Handler for '%s'", slug),
	}
}

// ActiveAPIKeys 返回与给定配置关联的所有处于激活状态的 API Key
func ActiveAPIKeys(serviceConfig *models.ProxyConfig) []models.APIKey {
	var activeKeys []models.APIKey
	for _, key := range serviceConfig.APIKeys {
		if key.IsActive {
			activeKeys = append(activeKeys, key)
		}
	}
	return activeKeys
}

// IsRetryableStatus 判断上游状态码是否值得换下一个 Key 重试
// 429 表示当前 Key 被限流或配额耗尽，401/403 表示当前 Key 无效或被禁用，
// 这些都是 Key 级别的问题，换 Key 可能成功；其它状态直接返回给客户端
func IsRetryableStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusTooManyRequests, http.StatusUnauthorized, http.StatusForbidden:
		return true
	default:
		return false
	}
}

// InjectUpstreamKey 将已构建好的 TargetRequest 中注入的上游 Key 替换为 newKey
// 注入规则必须与各 adapter 的初次注入保持一致 (header 还是 query、是否加 Bearer 前缀)
func InjectUpstreamKey(target *TargetRequest, proxyConfig *models.ProxyConfig, apiFormat string, newKey string) {
	// 各格式的默认注入位置/名称 (与 adapters/*.go 中的默认值保持一致)
	keyName := ""
	keyLocation := ""
	switch apiFormat {
	case "gemini_native":
		keyName = "x-goog-api-key"
		keyLocation = "header"
	case "anthropic_native":
		keyName = "x-api-key"
		keyLocation = "header"
	case "openai_compatible":
		keyName = "Authorization"
		keyLocation = "header"
	default:
		// generic: 无默认值，完全依赖数据库配置
	}

	if proxyConfig.APIKeyName != nil && *proxyConfig.APIKeyName != "" {
		keyName = *proxyConfig.APIKeyName
	}
	if proxyConfig.APIKeyLocation != nil && *proxyConfig.APIKeyLocation != "" {
		keyLocation = strings.ToLower(*proxyConfig.APIKeyLocation)
	}

	if keyName == "" || keyLocation == "" {
		return
	}

	if keyLocation == "header" {
		if target.Headers == nil {
			target.Headers = make(map[string]string)
		}
		if apiFormat == "openai_compatible" {
			target.Headers[keyName] = fmt.Sprintf("Bearer %s", newKey)
		} else {
			target.Headers[keyName] = newKey
		}
	} else if keyLocation == "query" {
		if target.Params == nil {
			target.Params = make(map[string]string)
		}
		target.Params[keyName] = newKey
	}
}

// RotateAPIKey 从与给定配置关联的密钥池中轮询一个API Key
func (h *BaseProxyHandler) RotateAPIKey(serviceConfig *models.ProxyConfig) (string, error) {
	// 获取活跃的密钥
	activeKeys := ActiveAPIKeys(serviceConfig)

	if len(activeKeys) == 0 {
		logger.Errorf("%s: Service '%s' has no active API keys.", h.logPrefix, serviceConfig.Name)
		return "", fmt.Errorf("no active API keys for this service")
	}

	// 使用缓存原子性递增来实现轮询
	ctx := context.Background()
	keyIndexKey := fmt.Sprintf("proxy_config:%d:key_index", serviceConfig.ID)
	keyIndex, err := h.cacheClient.Incr(ctx, keyIndexKey)
	if err != nil {
		logger.Errorf("%s: Failed to increment key index in cache: %v", h.logPrefix, err)
		return "", fmt.Errorf("failed to rotate API key")
	}

	// 计算实际索引
	actualIndex := int(keyIndex-1) % len(activeKeys)
	selectedKey := activeKeys[actualIndex].KeyValue

	logger.Infof("%s: Selected API key (masked): %s", h.logPrefix, utils.MaskAPIKeyDefault(selectedKey))
	return selectedKey, nil
}

// ValidateSlug 验证slug格式
func ValidateSlug(slug string) error {
	if slug == "" {
		return fmt.Errorf("slug cannot be empty")
	}
	// 可以添加更多验证规则
	return nil
}