package adapters

import (
	"fmt"
	"strings"

	"api-key-rotator/backend/internal/infrastructure/cache"
	"api-key-rotator/backend/internal/config"
	"api-key-rotator/backend/internal/models"
	"api-key-rotator/backend/internal/services"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// LLMAdapter LLM适配器接口
type LLMAdapter interface {
	ProcessRequest() (*services.TargetRequest, error)
}

// BaseLLMAdapter LLM适配器的抽象基类
type BaseLLMAdapter struct {
	cfg         *config.Config
	db          *gorm.DB
	cacheClient cache.CacheInterface
	c           *gin.Context
	proxyConfig *models.ProxyConfig
	action      string
	logPrefix   string
}

// NewBaseLLMAdapter 创建基础LLM适配器
func NewBaseLLMAdapter(cfg *config.Config, db *gorm.DB, cacheClient cache.CacheInterface,
	c *gin.Context, proxyConfig *models.ProxyConfig, action string) *BaseLLMAdapter {
	apiFormat := "unknown"
	if proxyConfig.APIFormat != nil {
		apiFormat = *proxyConfig.APIFormat
	}
	return &BaseLLMAdapter{
		cfg:         cfg,
		db:          db,
		cacheClient: cacheClient,
		c:           c,
		proxyConfig: proxyConfig,
		action:      action,
		logPrefix:   "Adapter (" + apiFormat + " for ID:" + fmt.Sprintf("%d", proxyConfig.ID) + ")",
	}
}

// RotateUpstreamKey 从密钥池中轮询一个真实的上游API Key
func (a *BaseLLMAdapter) RotateUpstreamKey() (string, error) {
	// 直接使用预加载好的ProxyConfig
	handler := services.NewBaseProxyHandler(a.cfg, a.db, a.cacheClient, a.c, a.proxyConfig.Slug, a.action)
	return handler.RotateAPIKey(a.proxyConfig)
}

// GetProxyKey 从请求中提取代理密钥。
// 代理密钥认证与客户端SDK格式无关: 无论客户端使用 OpenAI/Anthropic/Gemini 哪种SDK,
// 都能从常见的认证位置提取到代理密钥。
func (a *BaseLLMAdapter) GetProxyKey() string {
	// Authorization: Bearer <key>
	if auth := a.c.GetHeader("Authorization"); auth != "" {
		if strings.HasPrefix(auth, "Bearer ") {
			return strings.TrimPrefix(auth, "Bearer ")
		}
		return auth
	}

	// x-api-key (Anthropic), x-goog-api-key (Gemini), x-anthropic-api-key
	for _, header := range []string{"x-api-key", "x-goog-api-key", "x-anthropic-api-key"} {
		if key := a.c.GetHeader(header); key != "" {
			return key
		}
	}

	// 'key' URL查询参数 (Gemini)
	return a.c.Query("key")
}

// ValidateProxyKey 校验代理密钥是否在全局代理密钥列表中
func (a *BaseLLMAdapter) ValidateProxyKey(proxyKey string) bool {
	for _, key := range a.cfg.GetGlobalProxyKeys() {
		if proxyKey == key {
			return true
		}
	}
	return false
}
