package middleware

import (
	"net/http"
	"strings"

	"api-key-rotator/backend/internal/auth"

	"github.com/gin-gonic/gin"
)

// RequireAdmin 保护管理接口: 必须带 "Authorization: Bearer <token>" (或 X-Admin-Token)。
//
// 管理接口能读写上游 Key 池, 所以不能像代理接口那样只靠一把共享密钥:
// 这里用登录时签发的短期令牌 (HS256 JWT) 逐请求校验。
func RequireAdmin(secret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := ""
		if header := c.GetHeader("Authorization"); strings.HasPrefix(header, "Bearer ") {
			token = strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
		}
		if token == "" {
			token = strings.TrimSpace(c.GetHeader("X-Admin-Token"))
		}

		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error":   "Unauthorized",
				"success": false,
			})
			return
		}

		username, err := auth.ValidateToken(secret, token)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error":   "Invalid or expired token",
				"success": false,
			})
			return
		}

		c.Set("admin_username", username)
		c.Next()
	}
}
