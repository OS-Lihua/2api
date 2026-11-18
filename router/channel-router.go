package router

import (
	"net/http"
	"path"

	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
)

type permissionRoute struct {
	method     string
	path       string
	permission authz.Permission
	handler    gin.HandlerFunc
}

// handlePermissionRoute registers a Casbin-guarded route and declares the same
// permission as the access token scope that route requires.
func handlePermissionRoute(group *gin.RouterGroup, method, relativePath string, permission authz.Permission, handlers ...gin.HandlerFunc) {
	middleware.DeclareAccessTokenPermissionRoute(method, joinPaths(group.BasePath(), relativePath), permission)
	group.Handle(method, relativePath, append([]gin.HandlerFunc{middleware.RequirePermission(permission)}, handlers...)...)
}

// joinPaths matches gin's route path joining, which keeps a trailing slash
// from the relative path, so declared keys equal c.FullPath().
func joinPaths(absolutePath, relativePath string) string {
	if relativePath == "" {
		return absolutePath
	}
	finalPath := path.Join(absolutePath, relativePath)
	if relativePath[len(relativePath)-1] == '/' && finalPath[len(finalPath)-1] != '/' {
		return finalPath + "/"
	}
	return finalPath
}

func registerChannelRoutes(apiRouter *gin.RouterGroup) {
	channelRoute := apiRouter.Group("/channel")
	channelRoute.Use(middleware.AdminAuth())

	channelRoute.POST("/:id/key",
		middleware.RootAuth(),
		middleware.CriticalRateLimit(),
		middleware.DisableCache(),
		middleware.SecureVerificationRequired(),
		controller.RetiredLegacyChannelManagement,
	)

	for _, route := range channelPermissionRoutes {
		handlePermissionRoute(channelRoute, route.method, route.path, route.permission, route.handler)
	}
}

// registerProviderRoutes registers /api/admin/v1/providers. It keeps the
// permissions of the matching /api/channel routes.
func registerProviderRoutes(apiRouter *gin.RouterGroup) {
	providerRoute := apiRouter.Group("/admin/v1/providers")
	providerRoute.Use(middleware.AdminAuth())
	for _, route := range providerPermissionRoutes {
		handlePermissionRoute(providerRoute, route.method, route.path, route.permission, route.handler)
	}
}

func registerSingleModelChannelRoutes(apiRouter *gin.RouterGroup) {
	group := apiRouter.Group("/admin/v1/channels", middleware.AdminAuth())
	for _, route := range singleModelChannelRoutes {
		handlePermissionRoute(group, route.method, route.path, route.permission, route.handler)
	}
}

var singleModelChannelRoutes = []permissionRoute{
	{http.MethodPost, "/import", authz.ChannelWrite, controller.BulkImport},
	{http.MethodPost, "/bulk-confirm", authz.ChannelOperate, controller.BulkImport},
	{http.MethodPost, "/bulk-enable", authz.ChannelOperate, controller.BulkImport},
	{http.MethodGet, "", authz.ChannelRead, controller.ListSingleModelChannels},
	{http.MethodPost, "", authz.ChannelWrite, controller.SaveSingleModelChannel},
	{http.MethodGet, "/protocols", authz.ChannelRead, controller.ChannelProtocolOptions},
	{http.MethodGet, "/suggest-models", authz.ChannelRead, controller.SuggestChannelModels},
	{http.MethodPost, "/:id/probes", authz.ChannelOperate, controller.RunChannelProbe},
	{http.MethodGet, "/:id/probes", authz.ChannelRead, controller.ListChannelProbes},
	{http.MethodGet, "/:id", authz.ChannelRead, controller.GetSingleModelChannel},
	{http.MethodPatch, "/:id", authz.ChannelWrite, controller.SaveSingleModelChannel},
	{http.MethodDelete, "/:id", authz.ChannelWrite, controller.DeleteSingleModelChannel},
	{http.MethodPost, "/:id/confirm", authz.ChannelOperate, controller.ConfirmSingleModelChannel},
	{http.MethodPost, "/:id/enable", authz.ChannelOperate, controller.EnableSingleModelChannel},
	{http.MethodPost, "/:id/disable", authz.ChannelOperate, controller.DisableSingleModelChannel},
}

var providerPermissionRoutes = []permissionRoute{
	{method: http.MethodPost, path: "/import", permission: authz.ChannelSensitiveWrite, handler: controller.BulkImport},
	{method: http.MethodGet, path: "", permission: authz.ChannelRead, handler: controller.ListProviders},
	{method: http.MethodPost, path: "", permission: authz.ChannelSensitiveWrite, handler: controller.CreateProvider},
	{method: http.MethodGet, path: "/:id", permission: authz.ChannelRead, handler: controller.GetProvider},
	{method: http.MethodPatch, path: "/:id", permission: authz.ChannelWrite, handler: controller.UpdateProvider},
	{method: http.MethodDelete, path: "/:id", permission: authz.ChannelSensitiveWrite, handler: controller.DeleteProvider},
	{method: http.MethodPost, path: "/:id/enable", permission: authz.ChannelOperate, handler: controller.EnableProvider},
	{method: http.MethodPost, path: "/:id/disable", permission: authz.ChannelOperate, handler: controller.DisableProvider},
	{method: http.MethodPost, path: "/:id/balance", permission: authz.ChannelOperate, handler: controller.RefreshProviderBalance},
	{method: http.MethodGet, path: "/:id/keys", permission: authz.ChannelOperate, handler: controller.ListProviderKeys},
	{method: http.MethodPost, path: "/:id/keys/enable", permission: authz.ChannelOperate, handler: controller.EnableAllProviderKeys},
	{method: http.MethodPost, path: "/:id/keys/disable", permission: authz.ChannelOperate, handler: controller.DisableAllProviderKeys},
	{method: http.MethodDelete, path: "/:id/keys/disabled", permission: authz.ChannelOperate, handler: controller.DeleteDisabledProviderKeys},
	{method: http.MethodPost, path: "/:id/keys/:index/enable", permission: authz.ChannelOperate, handler: controller.EnableProviderKey},
	{method: http.MethodPost, path: "/:id/keys/:index/disable", permission: authz.ChannelOperate, handler: controller.DisableProviderKey},
	{method: http.MethodDelete, path: "/:id/keys/:index", permission: authz.ChannelOperate, handler: controller.DeleteProviderKey},
}

// Retired routes retain their original permissions, but never enter legacy handlers.
var channelPermissionRoutes = []permissionRoute{
	{method: http.MethodGet, path: "/priority/order", permission: authz.ChannelRead, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodPost, path: "/priority/reorder", permission: authz.ChannelWrite, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodGet, path: "/", permission: authz.ChannelRead, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodGet, path: "/search", permission: authz.ChannelRead, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodGet, path: "/models", permission: authz.ChannelRead, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodGet, path: "/default_base_urls", permission: authz.ChannelRead, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodGet, path: "/models_enabled", permission: authz.ChannelRead, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodGet, path: "/ops", permission: authz.ChannelRead, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodGet, path: "/:id", permission: authz.ChannelRead, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodGet, path: "/test", permission: authz.ChannelOperate, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodGet, path: "/test/:id", permission: authz.ChannelOperate, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodGet, path: "/update_balance", permission: authz.ChannelOperate, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodGet, path: "/update_balance/:id", permission: authz.ChannelOperate, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodPost, path: "/", permission: authz.ChannelSensitiveWrite, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodPut, path: "/", permission: authz.ChannelWrite, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodPost, path: "/status/batch", permission: authz.ChannelOperate, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodPost, path: "/:id/status", permission: authz.ChannelOperate, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodDelete, path: "/disabled", permission: authz.ChannelSensitiveWrite, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodPost, path: "/tag/disabled", permission: authz.ChannelOperate, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodPost, path: "/tag/enabled", permission: authz.ChannelOperate, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodPut, path: "/tag", permission: authz.ChannelWrite, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodDelete, path: "/:id", permission: authz.ChannelSensitiveWrite, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodPost, path: "/batch", permission: authz.ChannelSensitiveWrite, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodPost, path: "/fix", permission: authz.ChannelOperate, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodGet, path: "/fetch_models/:id", permission: authz.ChannelOperate, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodGet, path: "/:id/vllm/status", permission: authz.ChannelRead, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodGet, path: "/:id/sglang/status", permission: authz.ChannelRead, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodPost, path: "/fetch_models", permission: authz.ChannelSensitiveWrite, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodPost, path: "/:id/codex/refresh", permission: authz.ChannelSensitiveWrite, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodGet, path: "/:id/codex/usage", permission: authz.ChannelRead, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodGet, path: "/:id/codex/usage/reset-credits", permission: authz.ChannelRead, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodPost, path: "/:id/codex/usage/reset", permission: authz.ChannelOperate, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodPost, path: "/ollama/pull", permission: authz.ChannelSensitiveWrite, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodPost, path: "/ollama/pull/stream", permission: authz.ChannelSensitiveWrite, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodDelete, path: "/ollama/delete", permission: authz.ChannelSensitiveWrite, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodGet, path: "/ollama/version/:id", permission: authz.ChannelSensitiveWrite, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodPost, path: "/batch/tag", permission: authz.ChannelWrite, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodGet, path: "/tag/models", permission: authz.ChannelRead, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodPost, path: "/copy/:id", permission: authz.ChannelSensitiveWrite, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodPost, path: "/multi_key/manage", permission: authz.ChannelOperate, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodPost, path: "/upstream_updates/apply", permission: authz.ChannelWrite, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodPost, path: "/upstream_updates/apply_all", permission: authz.ChannelWrite, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodPost, path: "/upstream_updates/detect", permission: authz.ChannelOperate, handler: controller.RetiredLegacyChannelManagement},
	{method: http.MethodPost, path: "/upstream_updates/detect_all", permission: authz.ChannelOperate, handler: controller.RetiredLegacyChannelManagement},
}
