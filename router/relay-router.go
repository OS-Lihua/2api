package router

import (
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"

	"github.com/gin-gonic/gin"
)

func redirectModelDiscoveryTrailingSlash(c *gin.Context) {
	path := c.Request.URL.EscapedPath()
	if c.Request.Method == http.MethodGet && strings.HasSuffix(path, "/") {
		target := strings.TrimSuffix(path, "/")
		if query := c.Request.URL.RawQuery; query != "" {
			target += "?" + query
		}
		c.Redirect(http.StatusMovedPermanently, target)
		c.Abort()
		return
	}
	c.Next()
}

func SetRelayRouter(router *gin.Engine) {
	router.Use(middleware.CORS())
	router.Use(middleware.DecompressRequestMiddleware())
	router.Use(middleware.BodyStorageCleanup()) // 清理请求体存储
	router.Use(middleware.StatsMiddleware())
	// https://platform.openai.com/docs/api-reference/introduction
	modelsRouter := router.Group("/v1/models")
	modelsRouter.Use(middleware.RouteTag("relay"))
	modelsRouter.Use(redirectModelDiscoveryTrailingSlash)
	modelsRouter.Use(middleware.DisableCache(), middleware.GatewayTokenAuth())
	{
		modelsRouter.GET("", func(c *gin.Context) {
			switch {
			case c.GetHeader("x-api-key") != "" && c.GetHeader("anthropic-version") != "":
				controller.ListOfferingModels(c, constant.ChannelTypeAnthropic)
			case c.GetHeader("x-goog-api-key") != "" || c.Query("key") != "": // 单独的适配
				controller.ListOfferingModels(c, constant.ChannelTypeGemini)
			default:
				controller.ListOfferingModels(c, constant.ChannelTypeOpenAI)
			}
		})

		modelsRouter.GET("/*model", func(c *gin.Context) {
			switch {
			case c.GetHeader("x-api-key") != "" && c.GetHeader("anthropic-version") != "":
				controller.RetrieveOfferingModel(c, constant.ChannelTypeAnthropic)
			case c.GetHeader("x-goog-api-key") != "" || c.Query("key") != "":
				controller.RetrieveOfferingModel(c, constant.ChannelTypeGemini)
			default:
				controller.RetrieveOfferingModel(c, constant.ChannelTypeOpenAI)
			}
		})
	}

	geminiRouter := router.Group("/v1beta/models")
	geminiRouter.Use(func(c *gin.Context) {
		generation := defaultPluginDispatcher.registry.Generation()
		if generation != nil {
			for _, group := range generation.RouteGroups() {
				if group.Method == c.Request.Method && routeMatchesPath(group.GinPath, c.Request.URL.EscapedPath()) {
					defaultPluginDispatcher.dispatch(c)
					return
				}
			}
		}
		c.Next()
	})
	geminiRouter.Use(middleware.RouteTag("relay"))
	geminiRouter.Use(redirectModelDiscoveryTrailingSlash)
	geminiRouter.Use(middleware.DisableCache(), middleware.GatewayTokenAuth())
	{
		geminiRouter.GET("", func(c *gin.Context) {
			controller.ListOfferingModels(c, constant.ChannelTypeGemini)
		})
		geminiRouter.GET("/*model", func(c *gin.Context) { controller.RetrieveOfferingModel(c, constant.ChannelTypeGemini) })
	}

	geminiCompatibleRouter := router.Group("/v1beta/openai/models")
	geminiCompatibleRouter.Use(middleware.RouteTag("relay"))
	geminiCompatibleRouter.Use(middleware.DisableCache(), middleware.GatewayTokenAuth())
	{
		geminiCompatibleRouter.GET("", func(c *gin.Context) {
			controller.ListOfferingModels(c, constant.ChannelTypeOpenAI)
		})
	}

	playgroundRouter := router.Group("/pg")
	playgroundRouter.Use(middleware.RouteTag("relay"))
	playgroundRouter.Use(middleware.SystemPerformanceCheck())
	playgroundRouter.Use(middleware.UserAuth(), middleware.ResolveTextOffering(), middleware.ModelRequestRateLimit())
	{
		playgroundRouter.POST("/chat/completions", controller.Playground)
	}
	relayV1Router := router.Group("/v1")
	relayV1Router.Use(middleware.RouteTag("relay"))
	relayV1Router.Use(middleware.SystemPerformanceCheck())
	relayV1Router.Use(middleware.GatewayTokenAuth())
	relayV1Router.Use(middleware.ResolveTextOffering())
	{
		// Responses WebSocket route. Channel selection happens after the first
		// response.create event; each event runs the ordinary request limiter.
		relayV1Router.GET("/responses", controller.ResponsesWebSocket)
		relayV1Router.GET("/realtime", controller.RealtimeGateway)
	}
	relayV1Router.Use(middleware.ModelRequestRateLimit())
	{
		//http router
		httpRouter := relayV1Router.Group("")
		httpRouter.Use(controller.MediaGateway)
		httpRouter.Use(controller.TextGateway)

		// The gateways above own these requests, including missing models and
		// invalid bodies. No legacy dispatch is registered behind them.
		httpRouter.POST("/messages", controller.RelayNotImplemented)

		// chat related routes
		httpRouter.POST("/completions", controller.RelayNotImplemented)
		httpRouter.POST("/chat/completions", controller.RelayNotImplemented)

		// response related routes
		httpRouter.POST("/responses/compact", controller.RelayNotImplemented)

		// alpha search related routes (Codex standalone web search)
		httpRouter.POST("/alpha/search", controller.RelayNotImplemented)

		// DeepSeek Search and Fetch API routes
		httpRouter.POST("/tools/search", controller.RelayNotImplemented)
		httpRouter.POST("/tools/fetch", controller.RelayNotImplemented)

		// Image generation/edit endpoints are registered by the host protocol
		// router and use the task or synchronous media gateway.
		httpRouter.POST("/edits", controller.RelayNotImplemented)

		// embedding related routes
		httpRouter.POST("/embeddings", controller.RelayNotImplemented)

		// audio related routes
		httpRouter.POST("/audio/transcriptions", controller.RelayNotImplemented)
		httpRouter.POST("/audio/translations", controller.RelayNotImplemented)
		httpRouter.POST("/audio/speech", controller.RelayNotImplemented)

		// rerank related routes
		httpRouter.POST("/rerank", controller.RelayNotImplemented)

		// gemini relay routes
		httpRouter.POST("/engines/:model/embeddings", controller.RelayNotImplemented)
		httpRouter.POST("/models/*path", controller.RelayNotImplemented)

		// other relay routes
		httpRouter.POST("/moderations", controller.RelayNotImplemented)

		// not implemented
		httpRouter.POST("/images/variations", controller.RelayNotImplemented)
		httpRouter.GET("/files", controller.RelayNotImplemented)
		httpRouter.POST("/files", controller.RelayNotImplemented)
		httpRouter.DELETE("/files/:id", controller.RelayNotImplemented)
		httpRouter.GET("/files/:id/content", controller.RelayNotImplemented)
		httpRouter.POST("/fine-tunes", controller.RelayNotImplemented)
		httpRouter.GET("/fine-tunes", controller.RelayNotImplemented)
		httpRouter.GET("/fine-tunes/:id", controller.RelayNotImplemented)
		httpRouter.POST("/fine-tunes/:id/cancel", controller.RelayNotImplemented)
		httpRouter.GET("/fine-tunes/:id/events", controller.RelayNotImplemented)
		httpRouter.DELETE("/models/:model", controller.RelayNotImplemented)
	}

	relayMjRouter := router.Group("/mj")
	relayMjRouter.Use(middleware.RouteTag("relay"))
	relayMjRouter.Use(middleware.SystemPerformanceCheck())
	registerMjRouterGroup(relayMjRouter)

	relayMjModeRouter := router.Group("/:mode/mj")
	relayMjModeRouter.Use(middleware.RouteTag("relay"))
	relayMjModeRouter.Use(middleware.SystemPerformanceCheck())
	registerMjRouterGroup(relayMjModeRouter)
	//relayMjRouter.Use()

	relayGeminiRouter := router.Group("/v1beta")
	// :countTokens is not implemented. Answer it like an unregistered route
	// before auth/channel selection instead of silently relaying it as
	// generateContent (#7283).
	relayGeminiRouter.Use(func(c *gin.Context) {
		if strings.HasSuffix(c.Request.URL.Path, ":countTokens") {
			controller.RelayNotFound(c)
			c.Abort()
		}
	})
	// Veo's :predictLongRunning goes to its declared plugin route before this
	// group's auth. A dispatcher miss reaches credential validation and an
	// explicit unsupported response, never legacy generation or quota billing.
	relayGeminiRouter.Use(func(c *gin.Context) {
		if strings.HasSuffix(c.Request.URL.Path, ":predictLongRunning") {
			defaultPluginDispatcher.dispatch(c)
		}
	})
	relayGeminiRouter.Use(middleware.RouteTag("relay"))
	relayGeminiRouter.Use(middleware.SystemPerformanceCheck())
	relayGeminiRouter.Use(middleware.GatewayTokenAuth())
	relayGeminiRouter.Use(middleware.ResolveTextOffering())
	relayGeminiRouter.Use(middleware.ModelRequestRateLimit())
	relayGeminiRouter.Use(controller.TextGateway)
	{
		// The four supported text actions are consumed by TextGateway. Other
		// actions require an explicit plugin Offering contract.
		relayGeminiRouter.POST("/models/*path", controller.RelayNotImplemented)
	}
}

func registerMjRouterGroup(relayMjRouter *gin.RouterGroup) {
	relayMjRouter.GET("/image/:id", func(c *gin.Context) {
		c.Params = append(c.Params, gin.Param{Key: "artifact_key", Value: "image"})
	}, middleware.TokenOrTaskArtifactAccessAuth("id", "artifact_key"), controller.MidjourneyImage)
	// Native routes are explicitly handed to the pinned plugin pipeline. A
	// missing/disabled plugin never revives legacy quota billing.
	register := func(method, path string) {
		relayMjRouter.Handle(method, path, defaultPluginDispatcher.dispatch, middleware.TaskOriginAuth(), func(c *gin.Context) {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "task_plugin_unavailable", "message": "Task plugin is unavailable"}})
		})
	}
	for _, operation := range []string{"action", "shorten", "modal", "imagine", "change", "simple-change", "describe", "blend", "edits", "video", "upload-discord-images"} {
		register(http.MethodPost, "/submit/"+operation)
	}
	register(http.MethodPost, "/insight-face/swap")
	register(http.MethodPost, "/task/list-by-condition")
	register(http.MethodGet, "/task/:id/fetch")
	relayMjRouter.GET("/task/:id/image-seed", func(c *gin.Context) {
		c.Params = append(c.Params, gin.Param{Key: "artifact_key", Value: "seed"})
	}, middleware.TokenOrTaskArtifactAccessAuth("id", "artifact_key"), controller.MidjourneyImage)
}
