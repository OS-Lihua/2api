package router

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/pluginhost"
	"github.com/gin-gonic/gin"
)

type pluginDispatchStateKey struct{}

type pluginDispatchState struct {
	generation *jsplugin.RoutingGeneration
	hit        atomic.Bool
	writer     *gatedResponseWriter
	requestID  string
	language   string
}

func (s *pluginDispatchState) markHit() {
	s.hit.Store(true)
	s.writer.activate()
}

type pluginRouteHandlers func(*jsplugin.RoutingGeneration, jsplugin.RouteGroup) []gin.HandlerFunc

type pluginGenerationBuilder struct {
	policy         *pluginhost.Policy
	trustedProxies []string
	routeHandlers  pluginRouteHandlers
	registerRoute  func(*gin.Engine, jsplugin.RouteGroup, []gin.HandlerFunc)
	configure      func(*gin.Engine) error
}

type pluginRouteDispatcher struct {
	registry *jsplugin.Registry
}

// defaultPluginDispatcher serves the default registry's plugin routes from the
// outer engine's NoRoute and from the host routes that hand requests to plugin
// routes first.
var defaultPluginDispatcher = &pluginRouteDispatcher{registry: jsplugin.DefaultRegistry}

func SetPluginRouter(outer *gin.Engine) gin.HandlerFunc {
	trustedProxies, _, err := common.ResolveTrustedProxies(os.Getenv("TRUSTED_PROXIES"))
	dispatcher := defaultPluginDispatcher
	if err != nil {
		common.SysError(common.LogText("configure plugin router trusted proxies: %s", err.Error()))
		return dispatcher.dispatch
	}
	builder := newPluginGenerationBuilder(outer.Routes(), trustedProxies, productionPluginRouteHandlers)
	if err = jsplugin.DefaultRegistry.SetGenerationPreparer(builder.prepare); err != nil {
		common.SysError(common.LogText("build initial plugin router: %s", err.Error()))
	}
	return dispatcher.dispatch
}

func newPluginGenerationBuilder(staticRoutes []gin.RouteInfo, trustedProxies []string, handlers pluginRouteHandlers) *pluginGenerationBuilder {
	builder := &pluginGenerationBuilder{
		policy:         pluginhost.New(staticRoutes),
		trustedProxies: append([]string(nil), trustedProxies...),
		routeHandlers:  handlers,
	}
	builder.registerRoute = func(engine *gin.Engine, group jsplugin.RouteGroup, routeHandlers []gin.HandlerFunc) {
		engine.Handle(group.Method, group.GinPath, routeHandlers...)
	}
	builder.configure = func(engine *gin.Engine) error {
		return common.ConfigureTrustedProxies(engine, builder.trustedProxies)
	}
	return builder
}

func productionPluginRouteHandlers(generation *jsplugin.RoutingGeneration, group jsplugin.RouteGroup) []gin.HandlerFunc {
	binding := group.Bindings[0]
	keys := make([]string, 0, len(group.Bindings))
	for _, candidate := range group.Bindings {
		keys = append(keys, candidate.Plugin.Meta.Key)
	}
	pinRoute := func(c *gin.Context) {
		pinnedGeneration := generation
		if state, _ := c.Request.Context().Value(pluginDispatchStateKey{}).(*pluginDispatchState); state != nil && state.generation != nil {
			pinnedGeneration = state.generation
		}
		c.Set(jsplugin.ContextKeyPinnedPlugin, jsplugin.PinnedPlugin{
			Generation: pinnedGeneration,
			Plugin:     binding.Plugin,
		})
		c.Set(jsplugin.ContextKeyPinnedRoute, jsplugin.PinnedRoute{
			Generation: pinnedGeneration,
			Plugin:     binding.Plugin,
			Route:      binding.Route,
			Candidates: group.Bindings,
		})
		logger.LogDebug(
			c,
			"task_plugin subsystem=router event=route_matched generation=%d plugin=%q version=%q method=%q route_type=%q",
			pinnedGeneration.Number,
			binding.Plugin.Meta.Key,
			binding.Plugin.Meta.Version,
			binding.Route.Method,
			binding.Route.Type,
		)
		c.Next()
		logger.LogDebug(
			c,
			"task_plugin subsystem=router event=route_complete generation=%d plugin=%q method=%q status=%d",
			pinnedGeneration.Number,
			binding.Plugin.Meta.Key,
			binding.Route.Method,
			c.Writer.Status(),
		)
	}
	auth := middleware.TaskOriginAuth()
	if group.Method == http.MethodGet || group.Method == http.MethodHead {
		auth = middleware.TaskReadAuth()
	}
	return []gin.HandlerFunc{
		pinRoute,
		middleware.NativeRouteCredential(keys...),
		auth,
		middleware.SystemPerformanceCheck(),
		middleware.ResolveTaskOffering(),
		middleware.ResolveTaskPluginOffering(),
		controller.AuthorizeTaskGateway,
		middleware.ModelRequestRateLimit(),
		middleware.PrepareTaskPluginRoute(),
		controller.TaskGateway,
		controller.RelayNotImplemented,
	}
}

func (b *pluginGenerationBuilder) prepare(candidate, current *jsplugin.RoutingGeneration) (jsplugin.PreparedRoutingGeneration, error) {
	return b.policy.Prepare(candidate, current, func(generation *jsplugin.RoutingGeneration) (http.Handler, string, error) {
		return b.buildInnerEngine(generation)
	})
}

func (b *pluginGenerationBuilder) buildInnerEngine(generation *jsplugin.RoutingGeneration) (engine *gin.Engine, offender string, err error) {
	currentPlugin := ""
	defer func() {
		if recovered := recover(); recovered != nil {
			offender = currentPlugin
			err = fmt.Errorf("inner Gin registration panic: %v", recovered)
			engine = nil
		}
	}()

	engine = gin.New()
	engine.RedirectTrailingSlash = false
	engine.HandleMethodNotAllowed = true
	if err = b.configure(engine); err != nil {
		return nil, "", err
	}
	engine.Use(importPluginDispatchState())
	engine.Use(pluginRouteRecovery())
	engine.Use(middleware.BodyStorageCleanup())
	engine.NoMethod(func(c *gin.Context) {
		markPluginRouteHit(c)
		generation := uint64(0)
		if state, _ := c.Request.Context().Value(pluginDispatchStateKey{}).(*pluginDispatchState); state != nil && state.generation != nil {
			generation = state.generation.Number
		}
		logger.LogDebug(
			c,
			"task_plugin subsystem=router event=method_not_allowed generation=%d request_method=%q status=%d",
			generation,
			c.Request.Method,
			http.StatusMethodNotAllowed,
		)
		c.AbortWithStatus(http.StatusMethodNotAllowed)
	})

	for _, group := range generation.RouteGroups() {
		currentPlugin = group.Bindings[0].Plugin.Meta.Key
		b.registerRoute(engine, group, b.routeHandlers(generation, group))
	}
	currentPlugin = ""
	return engine, "", nil
}

func importPluginDispatchState() gin.HandlerFunc {
	return func(c *gin.Context) {
		state, _ := c.Request.Context().Value(pluginDispatchStateKey{}).(*pluginDispatchState)
		if state != nil {
			if c.FullPath() != "" {
				state.markHit()
			}
			if state.requestID != "" {
				c.Set(common.RequestIdKey, state.requestID)
			}
			if state.language != "" {
				c.Set(string(constant.ContextKeyLanguage), state.language)
			}
		}
		c.Set(middleware.RouteTagKey, "relay")
		c.Next()
	}
}

func markPluginRouteHit(c *gin.Context) {
	state, _ := c.Request.Context().Value(pluginDispatchStateKey{}).(*pluginDispatchState)
	if state != nil {
		state.markHit()
	}
}

func pluginRouteRecovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if recover() == nil {
				return
			}
			common.SysError(common.LogText("panic recovered in plugin route"))
			if pinnedValue, exists := c.Get(jsplugin.ContextKeyPinnedRoute); exists {
				if pinned, ok := pinnedValue.(jsplugin.PinnedRoute); ok && pinned.Plugin != nil && pinned.Generation != nil {
					logger.LogDebug(
						c,
						"task_plugin subsystem=router event=panic_recovered generation=%d plugin=%q method=%q",
						pinned.Generation.Number,
						pinned.Plugin.Meta.Key,
						pinned.Route.Method,
					)
				}
			}
			c.Abort()
			if !c.Writer.Written() {
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": gin.H{
						"message": "internal plugin route error",
						"type":    "plugin_route_error",
					},
				})
			}
		}()
		c.Next()
	}
}

func (d *pluginRouteDispatcher) dispatch(c *gin.Context) {
	generation := d.registry.Generation()
	if generation == nil || generation.RuntimeHandler() == nil {
		c.Next()
		return
	}

	previousTag, hadPreviousTag := c.Get(middleware.RouteTagKey)
	c.Set(middleware.RouteTagKey, "relay")
	originalContext := c.Request.Context()
	state := &pluginDispatchState{
		generation: generation,
		requestID:  c.GetString(common.RequestIdKey),
		language:   c.GetString(string(constant.ContextKeyLanguage)),
	}
	gatedWriter := newGatedResponseWriter(c.Writer)
	state.writer = gatedWriter
	c.Request = c.Request.WithContext(context.WithValue(originalContext, pluginDispatchStateKey{}, state))

	generation.RuntimeHandler().ServeHTTP(gatedWriter, c.Request)
	if state.hit.Load() {
		if !c.Writer.Written() {
			c.Writer.WriteHeaderNow()
		}
		c.Abort()
		return
	}

	c.Request = c.Request.WithContext(originalContext)
	if hadPreviousTag {
		c.Set(middleware.RouteTagKey, previousTag)
	} else {
		delete(c.Keys, middleware.RouteTagKey)
	}
	c.Next()
}

// handOffNativeTaskQuery answers GET /v1/videos/:task_id in the task's own wire
// format when the task was submitted through one of its plugin's native routes
// (at the vendor path, under /{key} or under the plugin's own prefix) and that
// plugin serves a query route matching this request, as xAI does at its vendor
// path. Every other task keeps the OpenAI video response.
func (d *pluginRouteDispatcher) handOffNativeTaskQuery(c *gin.Context) {
	generation := d.registry.Generation()
	if generation == nil || generation.RuntimeHandler() == nil {
		return
	}
	task, exists, err := model.GetByTaskId(c.GetInt("id"), c.Param("task_id"))
	if err != nil || !exists || task.PrivateData.Execution == nil || task.PrivateData.Execution.TaskPlugin == nil {
		return
	}
	plugin, found := generation.Get(task.PrivateData.Execution.TaskPlugin.Key)
	if !found {
		return
	}
	submittedNatively := false
	queryMatches := false
	for _, binding := range jsplugin.PluginRouteBindings(plugin) {
		route := binding.Route
		switch {
		case route.Type != jsplugin.RouteTypeQuery:
			submittedNatively = submittedNatively || routePatternsIntersect(route.Path, task.PrivateData.Execution.RequestPath)
		case route.Method == c.Request.Method && routePatternsIntersect(route.Path, c.Request.URL.Path):
			queryMatches = true
		}
	}
	if submittedNatively && queryMatches {
		d.dispatch(c)
	}
}

type gatedResponseWriter struct {
	underlying    gin.ResponseWriter
	privateHeader http.Header
	active        atomic.Bool
	activateOnce  sync.Once
	pendingStatus int
}

func newGatedResponseWriter(underlying gin.ResponseWriter) *gatedResponseWriter {
	return &gatedResponseWriter{
		underlying:    underlying,
		privateHeader: underlying.Header().Clone(),
	}
}

func (w *gatedResponseWriter) activate() {
	w.activateOnce.Do(func() {
		target := w.underlying.Header()
		for key := range target {
			target.Del(key)
		}
		for key, values := range w.privateHeader {
			target[key] = append([]string(nil), values...)
		}
		w.active.Store(true)
		if w.pendingStatus != 0 {
			w.underlying.WriteHeader(w.pendingStatus)
		}
	})
}

func (w *gatedResponseWriter) Header() http.Header {
	if w.active.Load() {
		return w.underlying.Header()
	}
	return w.privateHeader
}

func (w *gatedResponseWriter) WriteHeader(statusCode int) {
	if w.active.Load() {
		w.underlying.WriteHeader(statusCode)
		return
	}
	if w.pendingStatus == 0 {
		w.pendingStatus = statusCode
	}
}

func (w *gatedResponseWriter) Write(data []byte) (int, error) {
	if !w.active.Load() {
		return len(data), nil
	}
	return w.underlying.Write(data)
}

func (w *gatedResponseWriter) Flush() {
	if w.active.Load() {
		w.underlying.Flush()
	}
}

func (w *gatedResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if !w.active.Load() {
		return nil, nil, errors.New("cannot hijack an unmatched plugin route")
	}
	return w.underlying.Hijack()
}

// Unwrap lets http.ResponseController reach the client connection once a
// plugin route answers, as a relayed stream that stopped early needs to cut it
// off.
func (w *gatedResponseWriter) Unwrap() http.ResponseWriter {
	if !w.active.Load() {
		return nil
	}
	return w.underlying
}

func (w *gatedResponseWriter) CloseNotify() <-chan bool {
	if w.active.Load() {
		return w.underlying.CloseNotify()
	}
	never := make(chan bool)
	return never
}

func (w *gatedResponseWriter) Push(target string, options *http.PushOptions) error {
	if !w.active.Load() {
		return http.ErrNotSupported
	}
	pusher := w.underlying.Pusher()
	if pusher == nil {
		return http.ErrNotSupported
	}
	return pusher.Push(target, options)
}

func routeMatchesPath(pattern, escapedPath string) bool {
	return pluginhost.RouteMatchesPath(pattern, escapedPath)
}

func routePatternsIntersect(leftPath, rightPath string) bool {
	return pluginhost.RoutePatternsIntersect(leftPath, rightPath)
}
