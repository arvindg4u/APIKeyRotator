package adapters

import (
	"fmt"
	"io"
	"strings"

	"api-key-rotator/backend/internal/config"
	"api-key-rotator/backend/internal/infrastructure/cache"
	"api-key-rotator/backend/internal/logger"
	"api-key-rotator/backend/internal/models"
	"api-key-rotator/backend/internal/services"
	"api-key-rotator/backend/internal/utils"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// GeminiAdapter 适配器，用于处理Google Gemini原生API格式
type GeminiAdapter struct {
	*BaseLLMAdapter
}

// NewGeminiAdapter 创建Gemini适配器实例
func NewGeminiAdapter(cfg *config.Config, db *gorm.DB, cacheClient cache.CacheInterface,
	c *gin.Context, proxyConfig *models.ProxyConfig, action string) *GeminiAdapter {
	return &GeminiAdapter{
		BaseLLMAdapter: NewBaseLLMAdapter(cfg, db, cacheClient, c, proxyConfig, action),
	}
}

// ProcessRequest 处理Gemini格式的请求
func (a *GeminiAdapter) ProcessRequest() (*services.TargetRequest, error) {
	// 1. 代理访问认证 (与客户端SDK格式无关, 支持 Bearer/x-api-key/x-goog-api-key/key 等)
	proxyKey := a.GetProxyKey()

	if !a.ValidateProxyKey(proxyKey) {
		return nil, fmt.Errorf("invalid Proxy Key. Provide it via 'Authorization: Bearer <key>', 'x-goog-api-key' header, or the 'key' URL query parameter")
	}

	// 2. 构建目标请求 (偷梁换柱)
	// 移除客户端用于代理认证的Header, 避免代理密钥泄漏到上游
	headers := utils.FilterRequestHeaders(a.c.Request.Header, []string{
		"x-goog-api-key", "authorization", "x-api-key", "x-anthropic-api-key", "accept-encoding",
	})

	// 处理查询参数 (剔除客户端用于代理认证的 'key' 参数, 避免泄漏代理密钥)
	// 注意: 这里不再注入上游 Key。真实 Key 由 forwardLLMRequest 在每次尝试前统一注入
	// (见 services.InjectUpstreamKey), 这样 429 换 Key 重试时注入位置与首次请求完全一致。
	params := make(map[string]string)
	for key, values := range a.c.Request.URL.Query() {
		if len(values) == 0 {
			continue
		}
		// 客户端传入的 'key' 是代理密钥, 不应转发给上游
		if strings.EqualFold(key, "key") {
			continue
		}
		params[key] = values[0]
	}

	// URL拼接方式也不同
	baseURL := ""
	if a.proxyConfig.TargetBaseURL != nil {
		baseURL = strings.TrimSuffix(*a.proxyConfig.TargetBaseURL, "/")
	}
	finalURL := fmt.Sprintf("%s/%s", baseURL, a.action)

	// 读取请求体
	body, err := io.ReadAll(a.c.Request.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read request body: %w", err)
	}

	logger.Infof("%s: Prepared upstream request %s %s", a.logPrefix, a.c.Request.Method, finalURL)

	return &services.TargetRequest{
		Method:  a.c.Request.Method,
		URL:     finalURL,
		Headers: headers,
		Params:  params,
		Body:    body,
	}, nil
}
