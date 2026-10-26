package router

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"api-key-rotator/backend/internal/config"
	"api-key-rotator/backend/internal/infrastructure/cache/memory"
	"api-key-rotator/backend/internal/infrastructure/database/sqlite"

	"github.com/gin-gonic/gin"
)

// TestAccessLogRedactsGlobalProxyKeys covers H1 in the access log: the raw
// query string is part of Gin's log path, so a client that authenticates with
// ?key=<proxy-key> must not have that key written to disk/stdout.
func TestAccessLogRedactsGlobalProxyKeys(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var logOutput bytes.Buffer
	originalWriter := gin.DefaultWriter
	gin.DefaultWriter = &logOutput
	defer func() { gin.DefaultWriter = originalWriter }()

	repo, err := sqlite.NewSQLiteRepository(t.TempDir() + "/router.db")
	if err != nil {
		t.Fatalf("sqlite repo: %v", err)
	}
	if err := repo.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	engine := Setup(&config.Config{
		GlobalProxyKeys: "proxy-secret",
		JWTSecret:       "test-secret",
	}, repo, memory.NewMemoryCache())

	req := httptest.NewRequest(http.MethodGet, "/health?key=proxy-secret&token=legit", nil)
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)

	logLine := logOutput.String()
	if strings.Contains(logLine, "proxy-secret") {
		t.Fatalf("access log leaked the proxy key: %q", logLine)
	}
	if !strings.Contains(logLine, "[REDACTED]") {
		t.Fatalf("access log did not redact the proxy key: %q", logLine)
	}
	if !strings.Contains(logLine, "token=legit") {
		t.Fatalf("access log lost the ordinary query parameter: %q", logLine)
	}
}
