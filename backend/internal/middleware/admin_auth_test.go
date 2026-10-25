package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"api-key-rotator/backend/internal/auth"

	"github.com/gin-gonic/gin"
)

func TestRequireAdmin(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const secret = "test-secret"
	token, err := auth.GenerateToken(secret, "admin", time.Hour)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"valid bearer token", "Bearer " + token, http.StatusOK},
		{"valid X-Admin-Token", token, http.StatusOK},
		{"garbage bearer token", "Bearer nonsense", http.StatusUnauthorized},
		{"token signed with another secret", "Bearer " + mustGenerate(t, "other-secret"), http.StatusUnauthorized},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router := gin.New()
			router.GET("/admin/proxy-configs", RequireAdmin(secret), func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"ok": true})
			})

			req := httptest.NewRequest(http.MethodGet, "/admin/proxy-configs", nil)
			if tc.header != "" {
				if tc.name == "valid X-Admin-Token" {
					req.Header.Set("X-Admin-Token", tc.header)
				} else {
					req.Header.Set("Authorization", tc.header)
				}
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)

			if recorder.Code != tc.want {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.want)
			}
		})
	}
}

func mustGenerate(t *testing.T, secret string) string {
	t.Helper()
	token, err := auth.GenerateToken(secret, "admin", time.Hour)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	return token
}
