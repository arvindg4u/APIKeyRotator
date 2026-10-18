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

// AnthropicAdapter is an adapter for the Anthropic API.
type AnthropicAdapter struct {
	*BaseLLMAdapter
}

// NewAnthropicAdapter creates a new AnthropicAdapter instance.
func NewAnthropicAdapter(cfg *config.Config, db *gorm.DB, cacheClient cache.CacheInterface,
	c *gin.Context, proxyConfig *models.ProxyConfig, action string) *AnthropicAdapter {
	return &AnthropicAdapter{
		BaseLLMAdapter: NewBaseLLMAdapter(cfg, db, cacheClient, c, proxyConfig, action),
	}
}

// ProcessRequest handles the request for the Anthropic API.
func (a *AnthropicAdapter) ProcessRequest() (*services.TargetRequest, error) {
	// 1. Authenticate the proxy request. Try 'x-api-key', 'x-anthropic-api-key', then 'Authorization' header.
	proxyKey := a.c.GetHeader("x-api-key")
	if proxyKey == "" {
		proxyKey = a.c.GetHeader("x-anthropic-api-key")
	}
	if proxyKey == "" {
		authHeader := a.c.GetHeader("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			proxyKey = strings.TrimPrefix(authHeader, "Bearer ")
		}
	}

	validKeys := a.cfg.GetGlobalProxyKeys()
	isValidKey := false
	for _, key := range validKeys {
		if proxyKey == key {
			isValidKey = true
			break
		}
	}
	if !isValidKey {
		return nil, fmt.Errorf("invalid Proxy Key. Provide it via the 'x-api-key' header")
	}

	// 2. Build the target request.
	// Note: the real upstream key is NOT injected here. It is injected by forwardLLMRequest
	// right before every attempt (see services.InjectUpstreamKey), so that switching to the
	// next key after a 429 lands in exactly the same place as the first attempt.
	headers := utils.FilterRequestHeaders(a.c.Request.Header, []string{"x-api-key", "Authorization", "x-anthropic-api-key", "accept-encoding"})
	headers["anthropic-version"] = "2023-06-01"

	baseURL := ""
	if a.proxyConfig.TargetBaseURL != nil {
		baseURL = strings.TrimSuffix(*a.proxyConfig.TargetBaseURL, "/")
	}
	finalURL := fmt.Sprintf("%s/%s", baseURL, a.action)

	params := make(map[string]string)
	for key, values := range a.c.Request.URL.Query() {
		if len(values) > 0 {
			params[key] = values[0]
		}
	}

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
