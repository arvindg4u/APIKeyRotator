package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"api-key-rotator/backend/internal/logger"
	"api-key-rotator/backend/internal/models"
	"api-key-rotator/backend/internal/services"
	"api-key-rotator/backend/internal/utils"

	"github.com/gin-gonic/gin"
)

// ModelInfo 是从上游模型列表接口规范化后的一项。
// ContextWindow 为 nil 表示上游没有返回上下文窗口等元数据, 前端显示为空。
type ModelInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	ContextWindow *int64 `json:"context_window,omitempty"`
}

// maxModelListBody 限制上游模型列表响应体的读取长度, 避免异常上游把内存打爆。
const maxModelListBody = 8 << 20 // 8 MiB

// ListConfigModels 处理 GET /admin/proxy-configs/:id/models。
// 它用该 LLM 代理配置的 Key 池, 向上游提供商的模型列表接口拉取模型清单。
func (h *ManagementHandler) ListConfigModels(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid ID"})
		return
	}

	var proxyConfig models.ProxyConfig
	if err := h.dbRepo.GetDB().Preload("APIKeys").First(&proxyConfig, int32(id)).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Config not found"})
		return
	}

	if !strings.EqualFold(proxyConfig.ConfigType, "LLM") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Model listing is only available for LLM services"})
		return
	}

	list, err := h.fetchUpstreamModels(&proxyConfig)
	if err != nil {
		logger.Errorf("Failed to list models for config %d (%s): %v", proxyConfig.ID, proxyConfig.Slug, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	if list == nil {
		list = []ModelInfo{}
	}
	c.JSON(http.StatusOK, list)
}

// fetchUpstreamModels 用配置的 Key 池请求上游模型列表接口, 失败时按 Key 轮换重试。
func (h *ManagementHandler) fetchUpstreamModels(proxyConfig *models.ProxyConfig) ([]ModelInfo, error) {
	apiFormat := "openai_compatible"
	if proxyConfig.APIFormat != nil && strings.TrimSpace(*proxyConfig.APIFormat) != "" {
		apiFormat = strings.TrimSpace(*proxyConfig.APIFormat)
	}

	if len(services.ActiveAPIKeys(proxyConfig)) == 0 {
		return nil, fmt.Errorf("no active API keys for this service")
	}

	targetURL, err := modelsURL(proxyConfig)
	if err != nil {
		return nil, err
	}

	target := &services.TargetRequest{
		Method:  http.MethodGet,
		URL:     targetURL,
		Headers: map[string]string{"Accept": "application/json"},
		Params:  map[string]string{},
	}
	// Anthropic 的接口要求带上 API 版本头, 这与具体 Key 无关。
	if apiFormat == "anthropic_native" {
		target.Headers["anthropic-version"] = "2023-06-01"
	}
	services.ApplyUpstreamHeaders(target, proxyConfig, h.cfg)

	// 没有配置 Key 注入位置时无法调用上游, 直接报错而不是发一个必然 401 的请求。
	keyName, keyLocation := services.ResolveUpstreamKeyTarget(proxyConfig, apiFormat)
	if keyName == "" || keyLocation == "" {
		return nil, fmt.Errorf("no API key injection location configured for this service")
	}

	client := newUpstreamHTTPClient(h.cfg.ProxyTimeout)
	rotator := services.NewKeyRotator(h.cfg, h.cacheClient, fmt.Sprintf("Model list for '%s'", proxyConfig.Slug))

	tried := make(map[string]bool)
	var lastErr error

	for {
		selected, err := rotator.Next(proxyConfig, tried)
		if err != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}
		tried[selected.KeyValue] = true

		services.InjectUpstreamKey(target, proxyConfig, apiFormat, selected.KeyValue)

		body, status, err := doUpstreamModelRequest(client, target, h.cfg.ProxyTimeout)
		if err != nil {
			logger.Warningf("Model list for '%s': request failed for key %s: %v",
				proxyConfig.Slug, utils.MaskAPIKeyDefault(selected.KeyValue), err)
			lastErr = err
			continue
		}

		if status >= 200 && status < 300 {
			parsed, perr := parseModelsResponse(body)
			if perr != nil {
				return nil, fmt.Errorf("failed to parse upstream model list: %w", perr)
			}
			return parsed, nil
		}

		if services.IsRetryableStatus(status) {
			if status == http.StatusTooManyRequests {
				rotator.MarkRateLimited(proxyConfig.ID, selected.KeyValue, services.ParseRetryAfter(nil, body))
			}
			lastErr = fmt.Errorf("upstream returned %d: %s", status, truncateForLog(body, 256))
			logger.Warningf("Model list for '%s': upstream returned %d for key %s, trying next key",
				proxyConfig.Slug, status, utils.MaskAPIKeyDefault(selected.KeyValue))
			continue
		}

		return nil, fmt.Errorf("upstream returned %d: %s", status, truncateForLog(body, 256))
	}
}

// modelsURL 根据 LLM 配置的 Base URL 推导出模型列表接口地址。
func modelsURL(proxyConfig *models.ProxyConfig) (string, error) {
	base := ""
	if proxyConfig.TargetBaseURL != nil {
		base = strings.TrimSuffix(strings.TrimSpace(*proxyConfig.TargetBaseURL), "/")
	}
	if base == "" {
		return "", fmt.Errorf("target base URL is not configured")
	}
	return base + "/models", nil
}

// doUpstreamModelRequest 执行单次上游模型列表请求, 返回响应体与状态码。
func doUpstreamModelRequest(client *http.Client, target *services.TargetRequest, timeoutSeconds int) ([]byte, int, error) {
	targetURL, err := url.Parse(target.URL)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid target URL: %w", err)
	}

	if len(target.Params) > 0 {
		query := targetURL.Query()
		for key, value := range target.Params {
			query.Set(key, value)
		}
		targetURL.RawQuery = query.Encode()
	}

	req, err := http.NewRequest(target.Method, targetURL.String(), nil)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to create request: %w", err)
	}
	for key, value := range target.Headers {
		req.Header.Set(key, value)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()
	wrapUpstreamBody(resp, timeoutSeconds)

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelListBody))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("failed to read upstream response: %w", err)
	}

	return body, resp.StatusCode, nil
}

// parseModelsResponse 把各家的模型列表响应规范化成统一的 ModelInfo 列表。
// 支持的形状:
//
//	OpenAI / OpenRouter: {"data":[{"id":"...","context_length":...}]}
//	Anthropic:           {"data":[{"id":"...","display_name":"..."}]}
//	Gemini:              {"models":[{"name":"models/...","inputTokenLimit":...}]}
func parseModelsResponse(body []byte) ([]ModelInfo, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty upstream response")
	}

	entries, err := extractModelEntries(trimmed)
	if err != nil {
		return nil, err
	}

	result := make([]ModelInfo, 0, len(entries))
	seen := make(map[string]bool)
	for _, entry := range entries {
		id := firstString(entry, "id", "model", "slug", "name")
		if id == "" {
			continue
		}
		// Gemini 返回的是 "models/gemini-2.5-flash", 调用时用去掉前缀的名字。
		id = strings.TrimPrefix(id, "models/")

		name := firstString(entry, "display_name", "displayName", "display")
		if name == "" {
			name = id
		}

		if seen[id] {
			continue
		}
		seen[id] = true

		result = append(result, ModelInfo{
			ID:            id,
			Name:          name,
			ContextWindow: firstContextWindow(entry),
		})
	}

	return result, nil
}

// extractModelEntries 从响应体中取出模型数组, 兼容顶层数组与 {"data":[...]}/{"models":[...]} 包裹。
func extractModelEntries(trimmed []byte) ([]map[string]interface{}, error) {
	var entries []map[string]interface{}
	if err := json.Unmarshal(trimmed, &entries); err == nil {
		return entries, nil
	}

	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &wrapper); err != nil {
		return nil, err
	}

	for _, key := range []string{"data", "models", "result"} {
		raw, ok := wrapper[key]
		if !ok {
			continue
		}
		if err := json.Unmarshal(raw, &entries); err == nil {
			return entries, nil
		}
	}

	return nil, fmt.Errorf("upstream response does not contain a model list")
}

// firstString 返回给定键中第一个非空字符串。
func firstString(entry map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		value, ok := entry[key]
		if !ok {
			continue
		}
		if s, ok := value.(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// firstContextWindow 从模型条目里提取上下文窗口 (token 数)。
// 各家字段名不同, 取不到时返回 nil (前端显示为空)。
func firstContextWindow(entry map[string]interface{}) *int64 {
	for _, key := range []string{
		"context_window", "context_length", "contextWindow", "contextLength",
		"max_context_length", "max_context_window", "max_input_tokens",
		"inputTokenLimit", "input_token_limit", "context_size",
	} {
		value, ok := entry[key]
		if !ok {
			continue
		}
		if n, ok := numberValue(value); ok && n > 0 {
			return &n
		}
	}
	return nil
}

// numberValue 把 JSON 里的数字 (float64) 或数字字符串转成 int64。
func numberValue(value interface{}) (int64, bool) {
	switch v := value.(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			return n, true
		}
	}
	return 0, false
}
