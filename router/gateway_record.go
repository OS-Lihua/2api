package router

import (
	"net/http"

	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
)

func registerGatewayRecordRoutes(api *gin.RouterGroup) {
	admin := api.Group("/admin/v1", middleware.DisableCache(), middleware.AdminAuth())
	handlePermissionRoute(admin, http.MethodGet, "/requests", authz.RequestRead, controller.ListGatewayRequests)
	handlePermissionRoute(admin, http.MethodGet, "/requests/:id/attempts", authz.RequestRead, controller.ListGatewayAttempts)
	self := api.Group("/requests/self", middleware.DisableCache(), middleware.UserAuth())
	self.GET("", controller.ListSelfGatewayRequests)
	self.GET("/:id/attempts", controller.ListSelfGatewayAttempts)
}
