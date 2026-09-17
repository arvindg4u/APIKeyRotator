package handlers

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"api-key-rotator/backend/internal/infrastructure/cache"
	"api-key-rotator/backend/internal/config"
	"api-key-rotator/backend/internal/logger"
	"api-key-rotator/backend/internal/models"
	"api-key-rotator/backend/internal/services"
	"api-key-rotator/backend/internal/utils"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// ProxyHandler 通用代理处理器
type ProxyHandler struct {
	cfg        *config.Config
	db         *gorm.DB
	cacheClient cache.CacheInterface
}

// NewProxyHandler 创建通用代理处理器实例
func NewProxyHandler(cfg *config.Config, db *gorm.DB, cacheClient cache.CacheInterface) *ProxyHandler {
	return &ProxyHandler{
		cfg:        cfg,
		db:         db,
		cacheClient: cacheClient,
	}
}

// HandleGenericProxy 处理通用代理请求
func (h *ProxyHandler) HandleGenericProxy(c *gin.Context) {
	slug := strings.TrimPrefix(c.Param("slug"), "/")

	// 提取服务标识符（第一个路径段）
	parts := strings.SplitN(slug, "/", 2)
	serviceSlug := parts[0]

	if err := services.ValidateSlug(serviceSlug); err != nil {
		logger.Warningf("Bad Request for slug '%s': %v", serviceSlug, err)
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}

	handler := services.NewBaseProxyHandler(h.cfg, h.db, h.cacheClient, c, serviceSlug, "")

	targetRequest, proxyConfig, err := h.prepareGenericRequest(handler)
	if err != nil {
		logger.Warningf("Bad Request for slug '%s': %v", serviceSlug, err)
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}

	// 将完整的路径传递给转发函数
	c.Set("fullPath", slug)

	// 转发请求，传入proxyConfig以支持 429 时换 Key 重试
	if err := h.forwardRequest(c, targetRequest, proxyConfig); err != nil {
		logger.Errorf("An unexpected error occurred in GenericApiProxyHandler for slug '%s': %v", serviceSlug, err)
		c.JSON(http.StatusBadGateway, gin.H{"detail": "Bad Gateway"})
		return
	}
}

// prepareGenericRequest 准备通用代理请求，返回TargetRequest和ProxyConfig
func (h *ProxyHandler) prepareGenericRequest(handler *services.BaseProxyHandler) (*services.TargetRequest, *models.ProxyConfig, error) {
	// 1. 认证 (只支持Header)
	proxyKeyHeader := handler.C.GetHeader("X-Proxy-Key")
	validKeys := h.cfg.GetGlobalProxyKeys()
	isValidKey := false
	for _, key := range validKeys {
		if proxyKeyHeader == key {
			isValidKey = true
			break
		}
	}
	if !isValidKey {
		return nil, nil, fmt.Errorf("invalid or missing X-Proxy-Key header")
	}

	// 2. 加载配置
	var proxyConfig models.ProxyConfig
	if err := h.db.Preload("APIKeys").Where("slug = ? AND is_active = ? AND config_type = ?", handler.Slug, true, "GENERIC").First(&proxyConfig).Error; err != nil {
		return nil, nil, fmt.Errorf("generic service configuration with slug '%s' not found or inactive", handler.Slug)
	}

	// 3. 方法校验
	if proxyConfig.Method == nil || strings.ToUpper(handler.C.Request.Method) != strings.ToUpper(*proxyConfig.Method) {
		return nil, nil, fmt.Errorf("method Not Allowed. This path only accepts %s, but received %s",
			strings.ToUpper(*proxyConfig.Method), strings.ToUpper(handler.C.Request.Method))
	}

	// 4. 轮询并注入密钥
	apiKey, err := handler.RotateAPIKey(&proxyConfig)
	if err != nil {
		return nil, nil, err
	}

	// 5. 处理请求头
	headers := utils.FilterRequestHeaders(handler.C.Request.Header, []string{"x-proxy-key"})

	// 6. 处理查询参数
	params := make(map[string]string)
	for key, values := range handler.C.Request.URL.Query() {
		if len(values) > 0 {
			params[key] = values[0]
		}
	}

	// 7. 根据配置注入API密钥
	if proxyConfig.APIKeyLocation != nil && proxyConfig.APIKeyName != nil {
		location := strings.ToLower(*proxyConfig.APIKeyLocation)
		keyName := *proxyConfig.APIKeyName
		if location == "header" {
			headers[keyName] = apiKey
		} else if location == "query" {
			params[keyName] = apiKey
		}
	}

	// 8. 读取请求体
	body, err := io.ReadAll(handler.C.Request.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read request body: %w", err)
	}

	return &services.TargetRequest{
		Method:  handler.C.Request.Method,
		URL:     *proxyConfig.TargetURL,
		Headers: headers,
		Params:  params,
		Body:    body,
	}, &proxyConfig, nil
}

// forwardRequest 转发请求到目标服务器
// 上游返回 429/401/403 时自动轮换下一个 Key 重试，直到所有可用 Key 都试过一遍
func (h *ProxyHandler) forwardRequest(c *gin.Context, target *services.TargetRequest, proxyConfig *models.ProxyConfig) error {
	// 最多尝试次数 = 可用 Key 数量，每个 Key 最多试一次
	maxAttempts := len(services.ActiveAPIKeys(proxyConfig))
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	client := &http.Client{}
	if h.cfg.ProxyTimeout > 0 {
		client.Timeout = time.Duration(h.cfg.ProxyTimeout) * time.Second
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			// 上一次尝试的 Key 被上游拒绝，轮询下一个 Key 并替换请求中的 Key 后重试
			retryHandler := services.NewBaseProxyHandler(h.cfg, h.db, h.cacheClient, c, proxyConfig.Slug, "")
			newKey, err := retryHandler.RotateAPIKey(proxyConfig)
			if err != nil {
				logger.Errorf("Generic slug '%s': failed to rotate to next API key on retry: %v", proxyConfig.Slug, err)
				break
			}
			services.InjectUpstreamKey(target, proxyConfig, "generic", newKey)
			logger.Warningf("Generic slug '%s': retrying with next key (attempt %d/%d, key masked: %s)",
				proxyConfig.Slug, attempt+1, maxAttempts, utils.MaskAPIKeyDefault(newKey))
		}

		resp, err := h.doGenericUpstreamRequest(c, client, target)
		if err != nil {
			return err
		}

		if services.IsRetryableStatus(resp.StatusCode) && attempt < maxAttempts-1 {
			// 耗尽响应体后关闭连接再重试，避免连接泄漏；
			// 注意此时尚未向客户端写入任何内容，重试是安全的
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			logger.Warningf("Generic slug '%s': upstream returned %d, will try next key (attempt %d/%d)",
				proxyConfig.Slug, resp.StatusCode, attempt+1, maxAttempts)
			continue
		}

		return h.writeGenericResponse(c, resp)
	}

	// 所有 Key 都已试过 (正常情况下最后一次尝试会直接返回上游响应，
	// 走到这里说明重试时轮询出错)，返回 429 提示客户端稍后再试
	c.JSON(http.StatusTooManyRequests, gin.H{"detail": "All API keys are rate limited, please try again later"})
	return nil
}

// doGenericUpstreamRequest 执行单次上游 HTTP 请求，不写回客户端，供重试循环调用
func (h *ProxyHandler) doGenericUpstreamRequest(c *gin.Context, client *http.Client, target *services.TargetRequest) (*http.Response, error) {
	// 构建目标URL
	targetURL, err := url.Parse(target.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid target URL: %w", err)
	}

	// 获取完整路径并处理
	fullPath, _ := c.Get("fullPath")
	requestPath := fullPath.(string)

	// 提取除了服务标识符之外的路径部分
	parts := strings.SplitN(requestPath, "/", 2)
	if len(parts) > 1 {
		requestPath = parts[1]
	} else {
		requestPath = ""
	}

	// 如果目标URL没有以"/"结尾且请求路径不为空，则添加"/"
	if requestPath != "" {
		if !strings.HasSuffix(targetURL.Path, "/") && !strings.HasPrefix(requestPath, "/") {
			targetURL.Path += "/"
		}
		targetURL.Path += requestPath
	}

	// 添加查询参数
	if len(target.Params) > 0 {
		query := targetURL.Query()
		for key, value := range target.Params {
			query.Set(key, value)
		}
		targetURL.RawQuery = query.Encode()
	}

	// 创建HTTP请求
	req, err := http.NewRequest(target.Method, targetURL.String(), bytes.NewReader(target.Body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// 设置请求头
	for key, value := range target.Headers {
		req.Header.Set(key, value)
	}

	logger.Infof("Forwarding request to: %s %s", req.Method, req.URL.String())

	// 发送请求
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	logger.Infof("Received response from target with status code: %d", resp.StatusCode)
	return resp, nil
}

// writeGenericResponse 将上游响应 (成功或最终失败) 写回客户端
func (h *ProxyHandler) writeGenericResponse(c *gin.Context, resp *http.Response) error {
	defer resp.Body.Close()

	// 过滤响应头
	filteredHeaders := utils.FilterResponseHeaders(resp.Header)

	// 设置响应头
	for key, value := range filteredHeaders {
		c.Header(key, value)
	}

	// 设置状态码
	c.Status(resp.StatusCode)

	// 检查是否为流式响应
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/event-stream") {
		// 流式响应
		c.Stream(func(w io.Writer) bool {
			buffer := make([]byte, 1024)
			n, err := resp.Body.Read(buffer)
			if err != nil {
				if err != io.EOF {
					logger.Errorf("Error reading stream: %v", err)
				}
				return false
			}
			_, err = w.Write(buffer[:n])
			return err == nil
		})
	} else {
		// 普通响应
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("failed to read response body: %w", err)
		}
		c.Data(resp.StatusCode, contentType, body)
	}

	return nil
}
