package router

import (
	"fmt"
	"os"

	"api-key-rotator/backend/internal/config"
	"api-key-rotator/backend/internal/handlers"
	"api-key-rotator/backend/internal/infrastructure/cache"
	"api-key-rotator/backend/internal/infrastructure/database"
	"api-key-rotator/backend/internal/middleware"
	"api-key-rotator/backend/internal/utils"

	"github.com/gin-gonic/gin"
)

// Setup 设置路由
func Setup(cfg *config.Config, dbRepo database.Repository, cacheInterface cache.CacheInterface) *gin.Engine {
	// 设置Gin模式为调试模式以便看到更多日志
	gin.SetMode(gin.DebugMode)

	r := gin.New()

	// 添加中间件
	proxyKeys := cfg.GetGlobalProxyKeys()
	r.Use(gin.LoggerWithFormatter(func(param gin.LogFormatterParams) string {
		return fmt.Sprintf("%s - [%s] \"%s %s %s %d %s \"%s\" %s\"\n",
			param.ClientIP,
			param.TimeStamp.Format("02/Jan/2006:15:04:05 -0700"),
			param.Method,
			utils.RedactSecrets(param.Path, proxyKeys),
			param.Request.Proto,
			param.StatusCode,
			param.Latency,
			param.Request.UserAgent(),
			utils.RedactSecrets(param.ErrorMessage, proxyKeys),
		)
	}))
	r.Use(gin.Recovery())
	r.Use(middleware.CORS())

	// 添加全局请求调试中间件
	r.Use(func(c *gin.Context) {
		fmt.Printf("Global Request: %s %s\n", c.Request.Method, utils.RedactSecrets(c.Request.URL.Path, proxyKeys))
		c.Next()
	})

	// 健康检查 - 用于 Render / Docker / 负载均衡器探针
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	// 静态文件服务 - 为前端提供静态资源
	r.StaticFile("/", "./static/index.html")
	r.Static("/assets", "./static/assets")

	// PWA 文件 - service worker 必须从站点根路径提供.
	// 注册构建产物中实际存在的根级静态文件 (sw.js, manifest, icons 等),
	// 这样哈希化的 workbox-*.js 文件名也能被自动覆盖.
	// ./static 不存在时 (如未构建前端就 go run) 静默跳过.
	if entries, err := os.ReadDir("./static"); err == nil {
		for _, e := range entries {
			if e.IsDir() || e.Name() == "index.html" {
				continue
			}
			r.StaticFile("/"+e.Name(), "./static/"+e.Name())
		}
	}

	// 添加SPA支持 - 对于非API路径，返回index.html
	r.NoRoute(func(c *gin.Context) {
		path := c.Request.URL.Path
		// 检查是否是API请求
		if len(path) >= 6 && path[:6] == "/admin" ||
			len(path) >= 7 && path[:7] == "/proxy" ||
			len(path) >= 5 && path[:5] == "/llm" {
			// API路径返回404
			fmt.Printf("404 Not Found: %s %s\n", c.Request.Method, utils.RedactSecrets(c.Request.URL.Path, proxyKeys))
			c.JSON(404, gin.H{"error": "Route not found"})
		} else {
			// 非API路径，返回前端index.html
			c.File("./static/index.html")
		}
	})

	// 创建处理器实例，使用完整版本
	managementHandler := handlers.NewManagementHandler(cfg, dbRepo, cacheInterface)
	proxyHandler := handlers.NewProxyHandler(cfg, dbRepo.GetDB(), cacheInterface)
	llmProxyHandler := handlers.NewLLMProxyHandler(cfg, dbRepo.GetDB(), cacheInterface)

	// 管理API路由组 - 后台管理接口
	adminAPI := r.Group("/admin")
	{
		// 添加路由级别的调试中间件
		adminAPI.Use(func(c *gin.Context) {
			fmt.Printf("Admin API Request: %s %s\n", c.Request.Method, utils.RedactSecrets(c.Request.URL.Path, proxyKeys))
			c.Next()
		})

		// 公开配置 (只返回公共访问地址, 不含任何凭据)
		adminAPI.GET("/app-config", managementHandler.GetAppConfig)
		adminAPI.POST("/login", managementHandler.Login)

		// 其余管理接口必须带登录时签发的令牌:
		// 这些接口能读写上游 Key 池, 暴露在公网上时不能只靠 IP 白名单兜底
		adminProtected := adminAPI.Group("", middleware.RequireAdmin(cfg.JWTSecret))
		{
			// 代理配置管理
			adminProtected.POST("/proxy-configs", managementHandler.CreateConfig)
			adminProtected.GET("/proxy-configs", managementHandler.GetAllConfigs)
			adminProtected.GET("/proxy-configs/:id", managementHandler.GetConfigByID)
			adminProtected.PUT("/proxy-configs/:id", managementHandler.UpdateConfig)
			adminProtected.PUT("/proxy-configs/:id/status", managementHandler.UpdateConfigStatus)
			adminProtected.DELETE("/proxy-configs/:id", managementHandler.DeleteConfig)

			// API密钥管理
			adminProtected.GET("/proxy-configs/:id/keys", managementHandler.GetKeysForConfig)
			adminProtected.POST("/proxy-configs/:id/keys", managementHandler.CreateAPIKeyForConfig)
			adminProtected.POST("/proxy-configs/:id/keys/batch", managementHandler.BatchCreateAPIKeys)
			adminProtected.DELETE("/proxy-configs/:id/keys", managementHandler.ClearAllAPIKeys)
			adminProtected.PATCH("/keys/:keyID", managementHandler.UpdateAPIKeyStatus)
			adminProtected.DELETE("/keys/:keyID", managementHandler.DeleteAPIKey)
		}
	}

	// 通用代理路由组 - 公开API接口
	proxyGroup := r.Group("/proxy")
	proxyGroup.Any("/*slug", proxyHandler.HandleGenericProxy)

	// LLM代理路由组 - 公开API接口
	llmGroup := r.Group("/llm")
	llmGroup.Any("/:slug/*action", llmProxyHandler.HandleLLMProxy)

	return r
}
