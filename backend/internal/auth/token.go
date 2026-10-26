// Package auth 实现管理员会话令牌 (HS256 JWT)。
//
// 只用标准库实现, 不引入额外依赖: header/payload 用 base64url 编码,
// 签名是 HMAC-SHA256, 密钥来自环境变量 JWT_SECRET。
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// TokenTTL 管理员令牌有效期
const TokenTTL = 24 * time.Hour

var (
	// ErrInvalidToken 令牌格式错误或签名不匹配
	ErrInvalidToken = errors.New("invalid token")
	// ErrExpiredToken 令牌已过期
	ErrExpiredToken = errors.New("token expired")
)

// claims 令牌载荷
type claims struct {
	Subject   string `json:"sub"`
	Username  string `json:"username"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

func encodeSegment(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

func sign(secret, unsigned string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(unsigned))
	return encodeSegment(mac.Sum(nil))
}

// GenerateToken 签发一个 HS256 令牌
func GenerateToken(secret, username string, ttl time.Duration) (string, error) {
	now := time.Now()
	payload, err := json.Marshal(claims{
		Subject:   "admin",
		Username:  username,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(ttl).Unix(),
	})
	if err != nil {
		return "", err
	}

	header, err := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}

	unsigned := encodeSegment(header) + "." + encodeSegment(payload)
	return unsigned + "." + sign(secret, unsigned), nil
}

// ValidateToken 校验令牌, 成功后返回令牌里的用户名
func ValidateToken(secret, token string) (string, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return "", ErrInvalidToken
	}

	unsigned := parts[0] + "." + parts[1]
	expected := sign(secret, unsigned)
	// 定长比较, 避免按字节比较带来的时序差异
	if subtle.ConstantTimeCompare([]byte(expected), []byte(parts[2])) != 1 {
		return "", ErrInvalidToken
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ErrInvalidToken
	}

	var parsed claims
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return "", ErrInvalidToken
	}
	if parsed.ExpiresAt > 0 && time.Now().Unix() >= parsed.ExpiresAt {
		return "", ErrExpiredToken
	}
	return parsed.Username, nil
}

// ConstantTimeEquals 比较两个字符串, 不泄露前缀匹配的长度信息
func ConstantTimeEquals(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
