package handlers

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"api-key-rotator/backend/internal/adapters"
	"api-key-rotator/backend/internal/config"
	"api-key-rotator/backend/internal/converters"
	"api-key-rotator/backend/internal/infrastructure/cache"
	"api-key-rotator/backend/internal/logger"
	"api-key-rotator/backend/internal/models"
	"api-key-rotator/backend/internal/services"
	"api-key-rotator/backend/internal/utils"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// LLMProxyHandler LLM代理处理器
type LLMProxyHandler struct {
	cfg         *config.Config
	db          *gorm.DB
	cacheClient cache.CacheInterface
}

// NewLLMProxyHandler 创建LLM代理处理器实例
func NewLLMProxyHandler(cfg *config.Config, db *gorm.DB, cacheClient cache.CacheInterface) *LLMProxyHandler {
	return &LLMProxyHandler{
		cfg:         cfg,
		db:          db,
		cacheClient: cacheClient,
	}
}

// HandleLLMProxy 处理LLM代理请求
func (h *LLMProxyHandler) HandleLLMProxy(c *gin.Context) {
	slug := c.Param("slug")
	action := strings.TrimPrefix(c.Param("action"), "/")

	if err := services.ValidateSlug(slug); err != nil {
		logger.Warningf("Bad Request for LLM slug '%s': %v", slug, err)
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}

	targetRequest, proxyConfig, err := h.prepareLLMRequest(c, slug, action)
	if err != nil {
		logger.Warningf("Bad Request for LLM slug '%s': %v", slug, err)
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}

	// 转发请求，传入proxyConfig以支持响应格式转换
	if err := h.forwardLLMRequest(c, targetRequest, proxyConfig); err != nil {
		logger.Errorf("An unexpected error occurred in LlmApiProxyHandler for slug '%s': %v", slug, err)
		// 流式响应一旦开始写，就不能再追加一个 JSON 错误体，
		// 否则客户端会收到 "半截 SSE + JSON" 的损坏响应。
		if c.Writer.Written() {
			c.Abort()
			return
		}
		// Headers copied from the upstream response may still be buffered (for
		// example Content-Encoding: gzip). They do not describe our JSON error.
		c.Writer.Header().Del("Content-Encoding")
		c.Writer.Header().Del("Content-Length")
		if errors.Is(err, ErrUpstreamBodyTimeout) {
			c.JSON(http.StatusGatewayTimeout, gin.H{"detail": "Gateway Timeout"})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"detail": "Bad Gateway"})
		return
	}
}

// prepareLLMRequest 准备LLM代理请求，返回TargetRequest和ProxyConfig
func (h *LLMProxyHandler) prepareLLMRequest(c *gin.Context, slug, action string) (*services.TargetRequest, *models.ProxyConfig, error) {
	// 1. 加载基础配置
	var proxyConfig models.ProxyConfig
	if err := h.db.Preload("APIKeys").Where("slug = ? AND is_active = ? AND config_type = ?", slug, true, "LLM").First(&proxyConfig).Error; err != nil {
		return nil, nil, fmt.Errorf("LLM service configuration with slug '%s' not found or inactive", slug)
	}

	// 2. 校验密钥池 (没有可用的 Key 时没必要继续往下走)
	if len(services.ActiveAPIKeys(&proxyConfig)) == 0 {
		return nil, nil, fmt.Errorf("no active API keys for LLM service '%s'", slug)
	}

	// 3. 获取格式配置
	apiFormat := "openai_compatible"
	if proxyConfig.APIFormat != nil {
		apiFormat = *proxyConfig.APIFormat
	}
	clientFormat := "none"
	if proxyConfig.OutputFormat != nil {
		clientFormat = *proxyConfig.OutputFormat
	}

	// 4. 检查是否需要格式转换
	needConversion := converters.NeedsConversion(clientFormat, apiFormat)

	// 5. 读取请求体
	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read request body: %w", err)
	}

	// 6. 如果需要，转换请求格式
	convertedAction := action
	if needConversion {
		logger.Infof("Request format conversion enabled: %s -> %s", clientFormat, apiFormat)

		// 在转换前从原始请求体中提取模型名与流式标志 (转换后目标格式可能不包含这些字段)
		model := extractModelFromBody(bodyBytes)
		stream := extractStreamFromBody(bodyBytes)

		converter, err := converters.NewConverter(clientFormat, apiFormat)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create converter: %w", err)
		}

		// 转换请求体
		convertedBody, err := converter.ConvertRequest(bodyBytes)
		if err != nil {
			logger.Errorf("Failed to convert request: %v", err)
			return nil, nil, fmt.Errorf("failed to convert request format: %w", err)
		}
		bodyBytes = convertedBody

		// 转换请求路径
		convertedAction = converter.GetTargetPath(action)

		// 如果路径包含 {model} 占位符，用从原始请求体中提取的模型名替换
		if strings.Contains(convertedAction, "{model}") {
			if model != "" {
				convertedAction = strings.ReplaceAll(convertedAction, "{model}", model)
			} else {
				logger.Warningf("Target path '%s' contains {model} placeholder but no model was found in request body", convertedAction)
			}
		}

		// 流式请求: Gemini 上游需使用 streamGenerateContent 端点
		if stream && strings.Contains(convertedAction, ":generateContent") {
			convertedAction = strings.ReplaceAll(convertedAction, ":generateContent", ":streamGenerateContent")
		}

		logger.Infof("Converted action path: %s -> %s", action, convertedAction)
	}

	// 7. 将转换后的body放回request
	c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	// 8. 根据 API format 选择适配器 (使用API格式，而非客户端格式)
	var adapter adapters.LLMAdapter
	switch apiFormat {
	case "openai_compatible":
		adapter = adapters.NewOpenAIAdapter(h.cfg, h.db, h.cacheClient, c, &proxyConfig, convertedAction)
	case "gemini_native":
		adapter = adapters.NewGeminiAdapter(h.cfg, h.db, h.cacheClient, c, &proxyConfig, convertedAction)
	case "anthropic_native":
		adapter = adapters.NewAnthropicAdapter(h.cfg, h.db, h.cacheClient, c, &proxyConfig, convertedAction)
	default:
		logger.Errorf("No adapter found for API format '%s'", apiFormat)
		return nil, nil, fmt.Errorf("unsupported API format '%s' for LLM service '%s'", apiFormat, slug)
	}

	// 9. 实例化适配器并处理请求
	logger.Infof("LLM Proxy Handler for '%s': Dispatching to adapter: %s", slug, apiFormat)
	targetRequest, err := adapter.ProcessRequest()
	if err != nil {
		return nil, nil, err
	}

	return targetRequest, &proxyConfig, nil
}

// forwardLLMRequest 转发LLM请求到目标服务器，并应用响应格式转换
// 上游返回 429/401/403 时自动换下一个 Key 重试，直到所有可用 Key 都试过一遍
func (h *LLMProxyHandler) forwardLLMRequest(c *gin.Context, target *services.TargetRequest, proxyConfig *models.ProxyConfig) error {
	// Final credential scrub shared by all adapters. Keep this before
	// ApplyUpstreamHeaders/InjectUpstreamKey so legitimate upstream headers
	// added by the configuration are not removed.
	if h.cfg != nil {
		services.RedactTargetCredentials(target, h.cfg.GetGlobalProxyKeys())
	}

	// 获取格式配置，创建转换器 (与 Key 无关，只创建一次)
	apiFormat := "openai_compatible"
	if proxyConfig.APIFormat != nil {
		apiFormat = *proxyConfig.APIFormat
	}
	clientFormat := "none"
	if proxyConfig.OutputFormat != nil {
		clientFormat = *proxyConfig.OutputFormat
	}

	needConversion := converters.NeedsConversion(clientFormat, apiFormat)
	var converter *converters.Converter
	if needConversion {
		var err error
		converter, err = converters.NewConverter(apiFormat, clientFormat)
		if err != nil {
			logger.Errorf("Failed to create response converter: %v", err)
			needConversion = false
		} else {
			logger.Infof("Response format conversion enabled: %s -> %s", apiFormat, clientFormat)
		}
	}

	client := newUpstreamHTTPClient(h.cfg.ProxyTimeout)

	rotator := services.NewKeyRotator(h.cfg, h.cacheClient, fmt.Sprintf("LLM Proxy for '%s'", proxyConfig.Slug))

	// 合并配置里的额外请求头 (例如自定义 User-Agent 绕过上游 WAF)，并保证带一个像样的 UA。
	// 在循环外做一次即可: 后续每次尝试复用的是同一个 Headers map
	services.ApplyUpstreamHeaders(target, proxyConfig, h.cfg)

	// 没有配置注入位置时，换 Key 对上游请求没有任何影响，只尝试一次
	keyName, keyLocation := services.ResolveUpstreamKeyTarget(proxyConfig, apiFormat)
	injectEnabled := keyName != "" && keyLocation != ""

	// tried 记录本次请求已经用过的 Key，保证不会重复尝试同一个 Key
	tried := make(map[string]bool)
	var lastRetryable *services.BufferedResponse
	var lastNetworkErr error

	// wafRetried 记录是否已经用兜底 UA 重试过。
	// WAF 拦截与 Key 无关，重试一次就够了，不应该把整个 Key 池都耗在同一个拦截上。
	wafRetried := false

	for {
		selected, err := rotator.Next(proxyConfig, tried)
		if err != nil {
			if errors.Is(err, services.ErrNoAvailableKey) {
				return h.writeExhaustedLLMResponse(c, rotator, proxyConfig, lastRetryable, lastNetworkErr, converter, needConversion)
			}
			return err
		}
		tried[selected.KeyValue] = true

		// 每次尝试都重新注入 Key：首次请求与重试走的是同一段注入逻辑，位置绝不会不一致
		services.InjectUpstreamKey(target, proxyConfig, apiFormat, selected.KeyValue)
		logger.Infof("LLM slug '%s': using upstream key (masked): %s",
			proxyConfig.Slug, utils.MaskAPIKeyDefault(selected.KeyValue))

		resp, err := h.doUpstreamLLMRequest(client, target)
		if err != nil {
			logger.Errorf("LLM slug '%s': upstream request failed for key %s: %v",
				proxyConfig.Slug, utils.MaskAPIKeyDefault(selected.KeyValue), err)
			lastNetworkErr = err
			if !injectEnabled {
				return err
			}
			continue
		}

		if !services.IsRetryableStatus(resp.StatusCode) {
			return h.writeLLMResponse(c, resp, converter, needConversion)
		}

		// 可重试状态：先读完响应体并关闭连接（避免连接泄漏），再换下一个 Key 重试。
		// 注意此时尚未向客户端写入任何内容，重试是安全的。
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, services.MaxBufferedErrorBody))
		resp.Body.Close()
		if readErr != nil {
			// A stalled/truncated error body is a gateway failure, not a valid
			// retryable response. Do not cache partial bytes as the final body.
			logger.Errorf("LLM slug '%s': failed to read upstream %d response body for key %s: %v",
				proxyConfig.Slug, resp.StatusCode, utils.MaskAPIKeyDefault(selected.KeyValue), readErr)
			lastNetworkErr = readErr
			if resp.StatusCode == http.StatusTooManyRequests {
				rotator.MarkRateLimited(proxyConfig.ID, selected.KeyValue, 0)
			}
			if !injectEnabled {
				return readErr
			}
			continue
		}
		lastRetryable = &services.BufferedResponse{StatusCode: resp.StatusCode, Header: resp.Header, Body: body}

		if resp.StatusCode == http.StatusTooManyRequests {
			// 429：让这个 Key 冷却一段时间，后续请求优先使用其它 Key。
			// 上游给出 Retry-After 时按上游建议的时间冷却。
			rotator.MarkRateLimited(proxyConfig.ID, selected.KeyValue, services.ParseRetryAfter(resp.Header, body))
		} else if services.LooksLikeBlockPage(resp.StatusCode, body) {
			// 403/401 且响应体不是 JSON: 基本可以断定是 WAF/CDN 拦截 (请求指纹被拦),
			// 不是 Key 的问题，换 Key 也解决不了 —— 用 extra_headers 设置 User-Agent
			logger.Errorf("LLM slug '%s': upstream returned %d with a non-JSON body, this looks like a WAF/CDN block rather than a bad key; set extra_headers (e.g. 'User-Agent: ...') for this config. Response: %s",
				proxyConfig.Slug, resp.StatusCode, truncateForLog(body, 256))

			// 拦截多半来自客户端的 UA 指纹: 先用兜底 UA 把同一个 Key 再试一次,
			// 成功就直接返回, 不用把整个 Key 池都试一遍
			if !wafRetried && h.cfg.UpstreamWAFRetry && services.ApplySafeUserAgent(target, h.cfg) {
				wafRetried = true
				logger.Warningf("LLM slug '%s': retrying the same upstream key with a fallback User-Agent after a WAF/CDN block",
					proxyConfig.Slug)

				resp, err = h.doUpstreamLLMRequest(client, target)
				if err != nil {
					return err
				}
				if !services.IsRetryableStatus(resp.StatusCode) {
					return h.writeLLMResponse(c, resp, converter, needConversion)
				}

				body, readErr = io.ReadAll(io.LimitReader(resp.Body, services.MaxBufferedErrorBody))
				resp.Body.Close()
				if readErr != nil {
					return fmt.Errorf("failed to read upstream %d response body: %w", resp.StatusCode, readErr)
				}
				lastRetryable = &services.BufferedResponse{StatusCode: resp.StatusCode, Header: resp.Header, Body: body}
				if resp.StatusCode == http.StatusTooManyRequests {
					rotator.MarkRateLimited(proxyConfig.ID, selected.KeyValue, services.ParseRetryAfter(resp.Header, body))
				}
			}
		}

		logger.Warningf("LLM slug '%s': upstream returned %d for key %s, trying next key",
			proxyConfig.Slug, resp.StatusCode, utils.MaskAPIKeyDefault(selected.KeyValue))

		if !injectEnabled {
			break
		}
	}

	return h.writeExhaustedLLMResponse(c, rotator, proxyConfig, lastRetryable, lastNetworkErr, converter, needConversion)
}

// writeExhaustedLLMResponse 在所有 Key 都试过之后写回响应：
// 优先回写最后一次上游响应（保留真实错误信息），并带上 Retry-After 提示客户端何时重试。
// 如果没有任何上游响应、全部尝试都是网络错误，则返回 502（或超时 504），
// 不能把它伪装成 "所有 Key 都被限流" 的 429。
func (h *LLMProxyHandler) writeExhaustedLLMResponse(c *gin.Context, rotator *services.KeyRotator,
	proxyConfig *models.ProxyConfig, lastRetryable *services.BufferedResponse,
	lastNetworkErr error, converter *converters.Converter, needConversion bool) error {

	if lastRetryable == nil && lastNetworkErr != nil {
		return lastNetworkErr
	}

	if retryAfter := rotator.ShortestCooldown(proxyConfig); retryAfter > 0 {
		c.Header("Retry-After", strconv.Itoa(retryAfterSeconds(retryAfter)))
	}

	if lastRetryable != nil {
		return h.writeLLMResponse(c, lastRetryable.HTTPResponse(), converter, needConversion)
	}

	logger.Errorf("LLM slug '%s': no API key available to serve the request", proxyConfig.Slug)
	c.JSON(http.StatusTooManyRequests, gin.H{"detail": "All API keys are rate limited, please try again later"})
	return nil
}

// doUpstreamLLMRequest 执行单次上游 HTTP 请求，不写回客户端，供重试循环调用
func (h *LLMProxyHandler) doUpstreamLLMRequest(client *http.Client, target *services.TargetRequest) (*http.Response, error) {
	// 构建目标URL
	targetURL, err := url.Parse(target.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid target URL: %w", err)
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

	logger.Infof("Forwarding request to: %s %s (User-Agent: %q)", req.Method, req.URL.String(), req.Header.Get("User-Agent"))

	// 发送请求
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	timeoutSeconds := 0
	if h.cfg != nil {
		timeoutSeconds = h.cfg.ProxyTimeout
	}
	wrapUpstreamBody(resp, timeoutSeconds)

	logger.Infof("Received response from target with status code: %d", resp.StatusCode)
	return resp, nil
}

// writeLLMResponse 将上游响应 (成功或最终失败) 写回客户端
func (h *LLMProxyHandler) writeLLMResponse(c *gin.Context, resp *http.Response, converter *converters.Converter, needConversion bool) error {
	defer resp.Body.Close()

	// 过滤响应头
	filteredHeaders := utils.FilterResponseHeaders(resp.Header)

	contentType := resp.Header.Get("Content-Type")
	isStream := strings.Contains(contentType, "text/event-stream")
	isGzip := strings.EqualFold(strings.TrimSpace(resp.Header.Get("Content-Encoding")), "gzip")

	// We decompress non-stream gzip bodies below. Remove the encoding header
	// before it is written so clients do not try to gunzip plain text. A gzip
	// stream is passed through with its encoding header intact.
	if isGzip && !isStream {
		for key := range filteredHeaders {
			if strings.EqualFold(key, "Content-Encoding") {
				delete(filteredHeaders, key)
			}
		}
	}

	// 设置响应头
	for key, value := range filteredHeaders {
		c.Header(key, value)
	}

	// 检查是否为流式响应
	if isStream {
		// 流式响应处理
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Status(resp.StatusCode)

		if needConversion && converter != nil {
			// 带转换的流式响应
			return h.forwardStreamWithConversion(c, resp.Body, converter)
		}
		// 直接透传流式响应
		return h.forwardStreamDirect(c, resp.Body)
	}

	// 普通响应处理
	var bodyReader io.Reader = resp.Body

	// 检查是否是gzip压缩的响应
	if isGzip {
		gzReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return fmt.Errorf("failed to create gzip reader: %w", err)
		}
		defer gzReader.Close()
		bodyReader = gzReader
		logger.Infof("Response is gzip compressed, decompressing...")
	}

	body, err := io.ReadAll(bodyReader)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}

	// 如果是错误响应，打印详细日志
	if resp.StatusCode >= 400 {
		logger.Errorf("Target server returned error %d: %s", resp.StatusCode, string(body))
	}

	if needConversion && converter != nil {
		// 转换响应格式
		convertedBody, err := converter.ConvertResponse(body)
		if err != nil {
			logger.Errorf("Failed to convert response: %v", err)
			// 转换失败时返回原始响应
			c.Data(resp.StatusCode, contentType, body)
			return nil
		}
		c.Data(resp.StatusCode, "application/json", convertedBody)
	} else {
		c.Data(resp.StatusCode, contentType, body)
	}

	return nil
}

// forwardStreamDirect 直接透传流式响应
func (h *LLMProxyHandler) forwardStreamDirect(c *gin.Context, body io.Reader) error {
	var streamErr error
	c.Stream(func(w io.Writer) bool {
		buffer := make([]byte, 1024)
		n, err := body.Read(buffer)
		if n > 0 {
			if _, werr := w.Write(buffer[:n]); werr != nil {
				streamErr = werr
				return false
			}
		}
		if err != nil {
			if err != io.EOF {
				streamErr = err
				logger.Errorf("Error reading stream: %v", err)
			}
			return false
		}
		return true
	})
	return streamErr
}

// forwardStreamWithConversion 带格式转换的流式响应
func (h *LLMProxyHandler) forwardStreamWithConversion(c *gin.Context, body io.Reader, converter *converters.Converter) error {
	scanner := bufio.NewScanner(body)
	// 增加缓冲区大小以处理大的SSE消息
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	var streamErr error
	writeAll := func(w io.Writer, chunks ...[]byte) bool {
		for _, chunk := range chunks {
			if _, err := w.Write(chunk); err != nil {
				streamErr = err
				return false
			}
		}
		return true
	}
	writeEndEvents := func(w io.Writer) bool {
		for _, event := range converter.GetStreamEndEvents() {
			if !writeAll(w, []byte("data: "), event, []byte("\n\n")) {
				return false
			}
		}
		return true
	}

	c.Stream(func(w io.Writer) bool {
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				streamErr = err
				logger.Errorf("Error scanning stream: %v", err)
				// 上游流被截断时不要再补一个 "成功结束" 事件，
				// 否则客户端会把半截响应当成完整响应。
				return false
			}
			// 流正常结束: 写入目标格式所需的结束事件 (如 OpenAI 的 [DONE])
			writeEndEvents(w)
			return false
		}

		line := scanner.Text()

		// 跳过空行
		if strings.TrimSpace(line) == "" {
			return writeAll(w, []byte("\n"))
		}

		// 处理SSE数据行
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")

			// 处理 [DONE] 信号
			if payload == "[DONE]" {
				// 上游结束信号: 写入目标格式所需的结束事件并结束流
				writeEndEvents(w)
				return false
			}

			// 转换JSON payload
			convertedPayload, err := converter.ConvertStreamChunk([]byte(payload))
			if err != nil {
				logger.Errorf("Failed to convert stream chunk: %v", err)
				// 转换失败时透传原始数据
				return writeAll(w, []byte(line+"\n"))
			}

			// 如果转换结果为nil，跳过这个chunk
			if convertedPayload == nil {
				return true
			}

			// 写入转换后的数据
			return writeAll(w, []byte("data: "), convertedPayload, []byte("\n\n"))
		}

		// 其他行（如event:, id:等）直接透传
		return writeAll(w, []byte(line+"\n"))
	})

	return streamErr
}

// extractModelFromBody extracts the model name from a request body
func extractModelFromBody(body []byte) string {
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		return ""
	}

	if model, ok := req["model"].(string); ok {
		return model
	}

	return ""
}

// extractStreamFromBody extracts the stream flag from a request body
func extractStreamFromBody(body []byte) bool {
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		return false
	}

	if stream, ok := req["stream"].(bool); ok {
		return stream
	}

	return false
}
