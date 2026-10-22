package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"api-key-rotator/backend/internal/infrastructure/cache"
	"api-key-rotator/backend/internal/config"
	"api-key-rotator/backend/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// MaxBufferedErrorBody 缓存可重试响应体时允许的最大长度。
// 429/401/403 的错误体通常很小, 设上限只是为了避免异常上游把内存打爆。
const MaxBufferedErrorBody = 1 << 20 // 1 MiB

// TargetRequest 封装准备好的、即将被转发的请求信息
type TargetRequest struct {
	Method  string
	URL     string
	Headers map[string]string
	Params  map[string]string
	Body    []byte
}

// BufferedResponse 缓存下来的可重试响应 (429/401/403)。
// 换 Key 重试时先把它读完并关闭连接, 这样在所有 Key 都失败时
// 还能把上游真实的错误信息回写给客户端。
type BufferedResponse struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// HTTPResponse 把缓存的响应重新包装成 *http.Response, 以复用现有的写回逻辑。
// 响应体已经被完整读出 (且 Go 的 transport 会透明解压 gzip), 因此长度/编码头不再有效。
func (b *BufferedResponse) HTTPResponse() *http.Response {
	var header http.Header
	if b.Header != nil {
		header = b.Header.Clone()
		header.Del("Content-Encoding")
		header.Del("Content-Length")
	}

	return &http.Response{
		StatusCode: b.StatusCode,
		Header:     header,
		Body:       io.NopCloser(bytes.NewReader(b.Body)),
	}
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

// ResolveUpstreamKeyTarget 计算向上游请求注入 Key 时使用的名称与位置 (header / query)。
// 这是唯一的判定入口: 首次请求和 429 换 Key 重试都必须走这里,
// 否则两者注入位置不一致就会出现"换了 Key 但请求还是用旧 Key"的情况。
func ResolveUpstreamKeyTarget(proxyConfig *models.ProxyConfig, apiFormat string) (string, string) {
	// 各格式的默认注入位置/名称
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

	// 数据库中的配置优先 (TrimSpace + 小写, 避免 "Header"/"QUERY" 之类写法导致注入位置错乱)
	if proxyConfig.APIKeyName != nil && strings.TrimSpace(*proxyConfig.APIKeyName) != "" {
		keyName = strings.TrimSpace(*proxyConfig.APIKeyName)
	}
	if proxyConfig.APIKeyLocation != nil && strings.TrimSpace(*proxyConfig.APIKeyLocation) != "" {
		keyLocation = strings.ToLower(strings.TrimSpace(*proxyConfig.APIKeyLocation))
	}

	// Gemini 用 query 传 Key 时, 参数名就是 "key"
	if apiFormat == "gemini_native" && keyLocation == "query" &&
		(proxyConfig.APIKeyName == nil || strings.TrimSpace(*proxyConfig.APIKeyName) == "") {
		keyName = "key"
	}

	return keyName, keyLocation
}

// InjectUpstreamKey 把 newKey 注入到即将转发的请求中 (替换掉上一次尝试使用的 Key)。
// 返回值表示是否真的完成了注入; 没有配置注入位置时返回 false (此时换 Key 重试没有意义)。
func InjectUpstreamKey(target *TargetRequest, proxyConfig *models.ProxyConfig, apiFormat string, newKey string) bool {
	keyName, keyLocation := ResolveUpstreamKeyTarget(proxyConfig, apiFormat)
	if keyName == "" || keyLocation == "" {
		return false
	}

	switch keyLocation {
	case "header":
		if target.Headers == nil {
			target.Headers = make(map[string]string)
		}
		if apiFormat == "openai_compatible" {
			target.Headers[keyName] = fmt.Sprintf("Bearer %s", newKey)
		} else {
			target.Headers[keyName] = newKey
		}
	case "query":
		if target.Params == nil {
			target.Params = make(map[string]string)
		}
		target.Params[keyName] = newKey
	default:
		return false
	}

	return true
}

// defaultUserAgentHeader User-Agent 响应头的规范写法
const defaultUserAgentHeader = "User-Agent"

// setHeader 写入请求头，先删掉只差大小写的同名项，
// 避免 "user-agent" 与 "User-Agent" 同时进入 http.Header 造成取值不确定
func setHeader(headers map[string]string, name, value string) {
	for key := range headers {
		if key != name && strings.EqualFold(key, name) {
			delete(headers, key)
		}
	}
	headers[name] = value
}

// hasHeader 判断请求头里是否已有某个头 (忽略大小写)
func hasHeader(headers map[string]string, name string) bool {
	for key := range headers {
		if strings.EqualFold(key, name) {
			return true
		}
	}
	return false
}

// ParseExtraHeaders 解析配置里保存的额外请求头。
// 支持两种写法:
//
//	{"User-Agent": "curl/8.4.0", "X-Foo": "bar"}   (JSON 对象)
//	User-Agent: curl/8.4.0                        (每行一个 "名字: 值"，# 开头为注释)
//
// 解析失败时返回空集合，不影响主流程。
func ParseExtraHeaders(raw *string) map[string]string {
	headers := make(map[string]string)
	if raw == nil {
		return headers
	}

	value := strings.TrimSpace(*raw)
	if value == "" {
		return headers
	}

	if strings.HasPrefix(value, "{") {
		var parsed map[string]string
		if err := json.Unmarshal([]byte(value), &parsed); err == nil {
			for name, headerValue := range parsed {
				name = strings.TrimSpace(name)
				if name != "" {
					headers[name] = headerValue
				}
			}
			return headers
		}
	}

	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, headerValue, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		headers[name] = strings.TrimSpace(headerValue)
	}

	return headers
}

// ApplyUpstreamHeaders 把配置里的额外请求头合并到即将转发的请求上，并保证带一个像样的 User-Agent。
//
// 优先级: 配置里的 extra_headers > 客户端自带的请求头 > 全局默认 UA。
// 上游返回 403 且响应体不是 JSON 时通常是 WAF/CDN 拦截，
// 用 extra_headers 设置 User-Agent (或其它指纹相关头) 往往就能解决。
func ApplyUpstreamHeaders(target *TargetRequest, proxyConfig *models.ProxyConfig, cfg *config.Config) {
	if target == nil {
		return
	}
	if target.Headers == nil {
		target.Headers = make(map[string]string)
	}

	if proxyConfig != nil {
		for name, value := range ParseExtraHeaders(proxyConfig.ExtraHeaders) {
			setHeader(target.Headers, name, value)
		}
	}

	// 客户端没带 UA 时补上默认值: Go 默认的 "Go-http-client/1.1" 会被很多 WAF 直接拦掉
	if !hasHeader(target.Headers, defaultUserAgentHeader) && cfg != nil && cfg.UpstreamUserAgent != "" {
		setHeader(target.Headers, defaultUserAgentHeader, cfg.UpstreamUserAgent)
	}
}

// LooksLikeBlockPage 判断错误响应是否像 WAF/CDN 的拦截页。
// 这类响应不是 API 返回的业务错误，换 Key 也解决不了，通常是请求头/指纹被拦。
func LooksLikeBlockPage(statusCode int, body []byte) bool {
	if statusCode != http.StatusForbidden && statusCode != http.StatusUnauthorized {
		return false
	}

	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return false
	}
	// JSON 是各家 API 正常的错误格式
	return trimmed[0] != '{' && trimmed[0] != '['
}

// ValidateSlug 验证slug格式
func ValidateSlug(slug string) error {
	if slug == "" {
		return fmt.Errorf("slug cannot be empty")
	}
	// 可以添加更多验证规则
	return nil
}