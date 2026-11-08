package router

import (
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"

	"github.com/gin-gonic/gin"
)

func SetVideoRouter(router *gin.Engine) {
	videoSharedRouter := router.Group("/v1")
	videoSharedRouter.Use(middleware.RouteTag("relay"))
	videoSharedRouter.Use(middleware.ResolveTaskOffering(), middleware.TokenAuth())
	videoSharedRouter.Use(middleware.SystemPerformanceCheck())
	videoSharedRouter.POST(
		"/video/generations",
		middleware.RouteTaskPluginEndpoint(),
		middleware.TaskPluginEndpointOnly(middleware.ModelRequestRateLimit()),
		middleware.PrepareTaskPluginEndpoint(),
		controller.TaskGateway,
		middleware.Distribute(),
		func(c *gin.Context) {
			controller.RelayTaskPluginEndpoint(c, controller.RelayTask)
		},
	)

	videoV1Router := router.Group("/v1")
	videoV1Router.Use(middleware.RouteTag("relay"))
	{
		videoV1Router.GET("/video/generations/:task_id", middleware.TaskReadAuth(), middleware.Distribute(), controller.RelayTaskFetch)
		videoV1Router.POST("/videos/:video_id/remix", middleware.TaskOriginAuth(), middleware.PrepareTaskVideoRemix(), controller.TaskGateway, middleware.Distribute(), controller.RelayTask)
	}
}
