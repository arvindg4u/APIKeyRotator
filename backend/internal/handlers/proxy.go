package handlers

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
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

	// 4. 校验密钥池 (没有可用的 Key 时没必要继续往下走)
	if len(services.ActiveAPIKeys(&proxyConfig)) == 0 {
		return nil, nil, fmt.Errorf("no active API keys for generic service '%s'", proxyConfig.Slug)
	}

	// 5. 处理请求头
	headers := utils.FilterRequestHeaders(handler.C.Request.Header, []string{"x-proxy-key"})

	// 6. 处理查询参数
	// 注意: 这里不再注入上游 Key。真实 Key 由 forwardRequest 在每次尝试前统一注入
	// (见 services.InjectUpstreamKey), 这样 429 换 Key 重试时注入位置与首次请求完全一致。
	params := make(map[string]string)
	for key, values := range handler.C.Request.URL.Query() {
		if len(values) > 0 {
			params[key] = values[0]
		}
	}

	// 7. 读取请求体
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
// 上游返回 429/401/403 时自动换下一个 Key 重试，直到所有可用 Key 都试过一遍
func (h *ProxyHandler) forwardRequest(c *gin.Context, target *services.TargetRequest, proxyConfig *models.ProxyConfig) error {
	client := &http.Client{}
	if h.cfg.ProxyTimeout > 0 {
		client.Timeout = time.Duration(h.cfg.ProxyTimeout) * time.Second
	}

	rotator := services.NewKeyRotator(h.cfg, h.cacheClient, fmt.Sprintf("Generic Proxy for '%s'", proxyConfig.Slug))

	// 没有配置注入位置时，换 Key 对上游请求没有任何影响，只尝试一次
	keyName, keyLocation := services.ResolveUpstreamKeyTarget(proxyConfig, "generic")
	injectEnabled := keyName != "" && keyLocation != ""

	// tried 记录本次请求已经用过的 Key，保证不会重复尝试同一个 Key
	tried := make(map[string]bool)
	var lastRetryable *services.BufferedResponse

	for {
		selected, err := rotator.Next(proxyConfig, tried)
		if err != nil {
			if errors.Is(err, services.ErrNoAvailableKey) {
				return h.writeExhaustedResponse(c, rotator, proxyConfig, lastRetryable)
			}
			return err
		}
		tried[selected.KeyValue] = true

		// 每次尝试都重新注入 Key：首次请求与重试走的是同一段注入逻辑，位置绝不会不一致
		services.InjectUpstreamKey(target, proxyConfig, "generic", selected.KeyValue)
		logger.Infof("Generic slug '%s': using upstream key (masked): %s",
			proxyConfig.Slug, utils.MaskAPIKeyDefault(selected.KeyValue))

		resp, err := h.doGenericUpstreamRequest(c, client, target)
		if err != nil {
			return err
		}

		if !services.IsRetryableStatus(resp.StatusCode) {
			return h.writeGenericResponse(c, resp)
		}

		// 可重试状态：先读完响应体并关闭连接（避免连接泄漏），再换下一个 Key 重试。
		// 注意此时尚未向客户端写入任何内容，重试是安全的。
		body, _ := io.ReadAll(io.LimitReader(resp.Body, services.MaxBufferedErrorBody))
		resp.Body.Close()
		lastRetryable = &services.BufferedResponse{StatusCode: resp.StatusCode, Header: resp.Header, Body: body}

		if resp.StatusCode == http.StatusTooManyRequests {
			// 429：让这个 Key 冷却一段时间，后续请求优先使用其它 Key
			rotator.MarkRateLimited(proxyConfig.ID, selected.KeyValue, services.ParseRetryAfter(resp.Header, body))
		}

		logger.Warningf("Generic slug '%s': upstream returned %d for key %s, trying next key",
			proxyConfig.Slug, resp.StatusCode, utils.MaskAPIKeyDefault(selected.KeyValue))

		if !injectEnabled {
			break
		}
	}

	return h.writeExhaustedResponse(c, rotator, proxyConfig, lastRetryable)
}

// writeExhaustedResponse 在所有 Key 都试过之后写回响应：
// 优先回写最后一次上游响应（保留真实错误信息），并带上 Retry-After 提示客户端何时重试
func (h *ProxyHandler) writeExhaustedResponse(c *gin.Context, rotator *services.KeyRotator,
	proxyConfig *models.ProxyConfig, lastRetryable *services.BufferedResponse) error {

	if retryAfter := rotator.ShortestCooldown(proxyConfig); retryAfter > 0 {
		c.Header("Retry-After", strconv.Itoa(retryAfterSeconds(retryAfter)))
	}

	if lastRetryable != nil {
		return h.writeGenericResponse(c, lastRetryable.HTTPResponse())
	}

	logger.Errorf("Generic slug '%s': no API key available to serve the request", proxyConfig.Slug)
	c.JSON(http.StatusTooManyRequests, gin.H{"detail": "All API keys are rate limited, please try again later"})
	return nil
}

// retryAfterSeconds 把等待时长换算成 Retry-After 响应头需要的秒数 (向上取整，至少 1 秒)
func retryAfterSeconds(duration time.Duration) int {
	seconds := int(duration / time.Second)
	if duration%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}
	return seconds
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
