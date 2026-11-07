package router

import (
	"net/http"

	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
)

func registerPricingRoutes(api *gin.RouterGroup) {
	group := api.Group("/admin/v1", middleware.AdminAuth())
	for _, route := range []permissionRoute{
		{http.MethodPost, "/price-versions/import", authz.PricingWrite, controller.BulkImport},
		{http.MethodGet, "/price-versions", authz.PricingRead, controller.ListPriceVersions},
		{http.MethodPost, "/price-versions", authz.PricingWrite, controller.CreatePriceVersion},
		{http.MethodGet, "/price-versions/current", authz.PricingRead, controller.GetCurrentPriceVersion},
		{http.MethodGet, "/price-versions/:id", authz.PricingRead, controller.GetPriceVersion},
		{http.MethodGet, "/fx-rates", authz.PricingRead, controller.ListFXRates},
		{http.MethodPut, "/fx-rates", authz.PricingWrite, controller.SaveFXRate},
		{http.MethodGet, "/fx-rates/quote", authz.PricingRead, controller.GetFXQuote},
	} {
		handlePermissionRoute(group, route.method, route.path, route.permission, route.handler)
	}
}
