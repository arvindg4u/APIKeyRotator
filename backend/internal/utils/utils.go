package utils

import (
	"net/url"
	"strings"
)

// MaskAPIKey 对敏感的 API Key 进行脱敏处理
func MaskAPIKey(keyValue string, prefixLen, suffixLen int) string {
	if keyValue == "" {
		return "[INVALID_KEY]"
	}

	keyLen := len(keyValue)
	totalVisibleLen := prefixLen + suffixLen

	if keyLen <= totalVisibleLen {
		// 如果密钥太短，无法按规则脱敏，则只显示前缀并用星号代替其余部分
		if keyLen > prefixLen {
			return keyValue[:prefixLen] + "*****"
		}
		// 如果密钥甚至比前缀还短，则只显示前两个字符（如果可能）
		if keyLen > 2 {
			return keyValue[:2] + "*****"
		}
		return "*****" // 对于极短的密钥，完全隐藏
	}

	prefix := keyValue[:prefixLen]
	suffix := keyValue[keyLen-suffixLen:]

	// 使用固定数量的星号可以避免通过星号数量猜测密钥的原始长度
	maskedPart := "*****"

	return prefix + maskedPart + suffix
}

// MaskAPIKeyDefault 使用默认参数脱敏API Key
func MaskAPIKeyDefault(keyValue string) string {
	return MaskAPIKey(keyValue, 6, 3)
}

// FilterRequestHeaders 过滤请求头，移除不应该转发的头部
//
// 注意: User-Agent 不属于逐跳头，不会被过滤。
// 之前它被一起丢掉，导致所有上游请求都带着 Go 默认的 "Go-http-client/1.1" 发出去，
// 而很多 WAF/CDN (Cloudflare 等) 会把这个 UA 直接拦掉并返回 403，
// 看起来就像"Key 失效"，实际上请求根本没到模型。
// 客户端的 UA 现在会原样转发；需要固定 UA 时用配置里的 extra_headers 覆盖即可。
func FilterRequestHeaders(headers map[string][]string, headersToRemove []string) map[string]string {
	// 这些是不能从客户端透传给上游服务器的Header
	hopByHopHeaders := []string{
		"host", "content-length", "transfer-encoding", "connection",
		"keep-alive", "te", "upgrade",
	}

	// 合并通用的逐跳头和特定需要移除的头
	allToRemove := make(map[string]bool)
	for _, header := range hopByHopHeaders {
		allToRemove[strings.ToLower(header)] = true
	}
	for _, header := range headersToRemove {
		allToRemove[strings.ToLower(header)] = true
	}

	finalHeaders := make(map[string]string)
	for key, values := range headers {
		if !allToRemove[strings.ToLower(key)] && len(values) > 0 {
			finalHeaders[key] = values[0] // 取第一个值
		}
	}

	return finalHeaders
}

// RedactSecrets removes every configured secret from a log string.
// It also checks the URL-encoded form so a proxy key in a query string cannot
// leak through Gin's access log.
func RedactSecrets(text string, secrets []string) string {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		text = strings.ReplaceAll(text, secret, "[REDACTED]")
		if escaped := url.QueryEscape(secret); escaped != secret {
			text = strings.ReplaceAll(text, escaped, "[REDACTED]")
		}
	}
	return text
}

// FilterResponseHeaders 过滤响应头，移除不应该返回给客户端的头部
func FilterResponseHeaders(headers map[string][]string) map[string]string {
	// 这些是不能从上游服务器透传给最终客户端的Header
	hopByHopHeaders := []string{
		"connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailers", "transfer-encoding", "upgrade", "content-length",
	}

	toRemove := make(map[string]bool)
	for _, header := range hopByHopHeaders {
		toRemove[strings.ToLower(header)] = true
	}

	filtered := make(map[string]string)
	for key, values := range headers {
		if !toRemove[strings.ToLower(key)] && len(values) > 0 {
			filtered[key] = values[0] // 取第一个值
		}
	}

	return filtered
}
