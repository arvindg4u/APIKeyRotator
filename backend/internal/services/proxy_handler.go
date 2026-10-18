package services

import (
	"bytes"
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

// ValidateSlug 验证slug格式
func ValidateSlug(slug string) error {
	if slug == "" {
		return fmt.Errorf("slug cannot be empty")
	}
	// 可以添加更多验证规则
	return nil
}