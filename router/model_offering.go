package router

import (
	"net/http"

	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
)

func registerModelOfferingRoutes(api *gin.RouterGroup) {
	group := api.Group("/admin/v1/model-offerings", middleware.AdminAuth())
	for _, route := range []permissionRoute{
		{http.MethodGet, "", authz.ModelRead, controller.ListModelOfferings},
		{http.MethodPost, "", authz.ModelWrite, controller.SaveModelOffering},
		{http.MethodGet, "/:id", authz.ModelRead, controller.GetModelOffering},
		{http.MethodPut, "/:id", authz.ModelWrite, controller.SaveModelOffering},
		{http.MethodDelete, "/:id", authz.ModelWrite, controller.DeleteModelOffering},
		{http.MethodPost, "/:id/enable", authz.ModelWrite, controller.EnableModelOffering},
		{http.MethodPost, "/:id/disable", authz.ModelWrite, controller.DisableModelOffering},
	} {
		handlePermissionRoute(group, route.method, route.path, route.permission, route.handler)
	}
}
