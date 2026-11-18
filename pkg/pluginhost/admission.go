// Package pluginhost applies the host's public-route admission rules without
// importing controllers, models, request middleware, or a serving registry.
package pluginhost

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/gin-gonic/gin"
)

// Policy owns a copy of the complete host Method/Path declarations.
type Policy struct {
	staticRoutes []gin.RouteInfo
	hostSegments map[string]struct{}
}

type EngineBuilder func(*jsplugin.RoutingGeneration) (http.Handler, string, error)

func New(staticRoutes []gin.RouteInfo) *Policy {
	policy := &Policy{staticRoutes: append([]gin.RouteInfo(nil), staticRoutes...), hostSegments: map[string]struct{}{"api": {}}}
	for _, route := range staticRoutes {
		first, _, _ := strings.Cut(strings.TrimPrefix(route.Path, "/"), "/")
		if first != "" && !strings.HasPrefix(first, ":") && !strings.HasPrefix(first, "*") {
			policy.hostSegments[first] = struct{}{}
		}
	}
	return policy
}

// PrepareOffline uses exactly the same admission and Gin registration checks,
// but never carries a runtime incumbent or installs request handlers. The
// source deployment must stand on its own declarations, not process history.
func (b *Policy) PrepareOffline(candidate, _ *jsplugin.RoutingGeneration) (jsplugin.PreparedRoutingGeneration, error) {
	return b.Prepare(candidate, nil, validateRouteTree)
}

func validateRouteTree(generation *jsplugin.RoutingGeneration) (_ http.Handler, offender string, err error) {
	currentPlugin := ""
	defer func() {
		if recovered := recover(); recovered != nil {
			offender = currentPlugin
			err = fmt.Errorf("inner Gin registration panic: %v", recovered)
		}
	}()
	tree := gin.New()
	tree.RedirectTrailingSlash = false
	tree.HandleMethodNotAllowed = true
	for _, group := range generation.RouteGroups() {
		currentPlugin = group.Bindings[0].Plugin.Meta.Key
		tree.Handle(group.Method, group.GinPath, func(*gin.Context) {})
	}
	return nil, "", nil
}

// pluginHandoffRoutes are the host routes that offer matching requests to the
// plugin routes before their own handlers run, so a plugin route may overlap
// them: GET /v1/videos/:task_id offers a task to its own plugin's query route
// (handOffNativeTaskQuery), Gemini discovery offers native operation queries,
// and the Gemini relay offers :predictLongRunning.
var pluginHandoffRoutes = func() map[string]struct{} {
	routes := map[string]struct{}{
		http.MethodGet + " /v1/videos/:task_id":   {},
		http.MethodGet + " /v1beta/models/*model": {},
		http.MethodPost + " /v1beta/models/*path": {},
	}
	for _, prefix := range []string{"/mj", "/:mode/mj"} {
		for _, operation := range []string{"action", "shorten", "modal", "imagine", "change", "simple-change", "describe", "blend", "edits", "video", "upload-discord-images"} {
			routes[http.MethodPost+" "+prefix+"/submit/"+operation] = struct{}{}
		}
		for _, path := range []string{"/insight-face/swap", "/task/list-by-condition"} {
			routes[http.MethodPost+" "+prefix+path] = struct{}{}
		}
		for _, path := range []string{"/task/:id/fetch", "/task/:id/image-seed"} {
			routes[http.MethodGet+" "+prefix+path] = struct{}{}
		}
	}
	return routes
}()

func (b *Policy) Prepare(candidate, current *jsplugin.RoutingGeneration, build EngineBuilder) (jsplugin.PreparedRoutingGeneration, error) {
	blocked := make(map[*jsplugin.LoadedPlugin]string)
	for {
		accepted, skipped, routingErrors := b.admitPlugins(candidate, current, blocked)
		plugins := sortedPlugins(accepted)
		filtered, err := candidate.RebuildWithPlugins(plugins)
		if err != nil {
			return jsplugin.PreparedRoutingGeneration{}, err
		}
		filtered = filtered.WithSkippedRoutes(skipped)
		engine, offender, buildErr := build(filtered)
		if buildErr == nil {
			return jsplugin.PreparedRoutingGeneration{
				Generation: filtered.WithRuntime(engine),
				Errors:     routingErrors,
			}, nil
		}
		if offender == "" {
			return jsplugin.PreparedRoutingGeneration{}, buildErr
		}

		failedPlugin := accepted[offender]
		if failedPlugin == nil {
			return jsplugin.PreparedRoutingGeneration{}, fmt.Errorf("public route rebuild attributed failure to absent plugin %q: %w", offender, buildErr)
		}
		blocked[failedPlugin] = fmt.Sprintf("plugin %s rejected while rebuilding public routes: %v", offender, buildErr)
	}
}

func (b *Policy) admitPlugins(
	candidate, current *jsplugin.RoutingGeneration,
	blocked map[*jsplugin.LoadedPlugin]string,
) (map[string]*jsplugin.LoadedPlugin, []jsplugin.SkippedRoute, map[string]string) {
	accepted := make(map[string]*jsplugin.LoadedPlugin)
	var skipped []jsplugin.SkippedRoute
	currentByKey := make(map[string]*jsplugin.LoadedPlugin)
	if current != nil && current.RuntimeHandler() != nil {
		for _, plugin := range current.Plugins() {
			currentByKey[plugin.Meta.Key] = plugin
		}
	}

	unchangedKeys := make([]string, 0)
	changedKeys := make([]string, 0)
	newKeys := make([]string, 0)
	for _, plugin := range candidate.Plugins() {
		incumbent, existed := currentByKey[plugin.Meta.Key]
		switch {
		case existed && incumbent == plugin:
			unchangedKeys = append(unchangedKeys, plugin.Meta.Key)
		case existed:
			changedKeys = append(changedKeys, plugin.Meta.Key)
		default:
			newKeys = append(newKeys, plugin.Meta.Key)
		}
	}
	sort.Strings(unchangedKeys)
	sort.Strings(changedKeys)
	sort.Strings(newKeys)

	routingErrors := make(map[string]string)
	orderedKeys := append(unchangedKeys, changedKeys...)
	orderedKeys = append(orderedKeys, newKeys...)
	rejectedKeys := make([]string, 0)
	for _, key := range orderedKeys {
		plugin, _ := candidate.Get(key)
		if blockedError := blocked[plugin]; blockedError != "" {
			routingErrors[key] = blockedError
		} else if pluginSkipped, err := b.validatePlugin(plugin, candidate); err != nil {
			routingErrors[key] = fmt.Sprintf("plugin %s rejected from public routes: %v", key, err)
		} else {
			accepted[key] = plugin
			skipped = append(skipped, pluginSkipped...)
			continue
		}
		rejectedKeys = append(rejectedKeys, key)
	}

	for _, key := range rejectedKeys {
		plugin, _ := candidate.Get(key)
		incumbent := currentByKey[key]
		if incumbent == nil || incumbent == plugin || !candidate.RetainsIncumbent(key) {
			continue
		}
		if blockedError := blocked[incumbent]; blockedError != "" {
			routingErrors[key] = blockedError
			continue
		}
		if incumbentSkipped, err := b.validatePlugin(incumbent, candidate); err == nil {
			accepted[key] = incumbent
			skipped = append(skipped, incumbentSkipped...)
		}
	}
	return accepted, skipped, routingErrors
}

// validatePlugin returns the declared top-level routes the gateway does not
// register for plugin; their /{key} copies still serve them. A key that names
// a host path segment, or a built-in plugin's own prefix, rejects the plugin:
// its /{key} address would not reach it.
//
// A top-level route is skipped when its first segment is a host namespace and
// the plugin is not built in (host_namespace), when a host route of the same
// method answers it first (host_route), or when its first segment is another
// plugin's key or a built-in plugin's own prefix (key_namespace). A route
// under the plugin's own prefix meets only the first two checks.
func (b *Policy) validatePlugin(plugin *jsplugin.LoadedPlugin, candidate *jsplugin.RoutingGeneration) ([]jsplugin.SkippedRoute, error) {
	key := plugin.Meta.Key
	if _, hostSegment := b.hostSegments[key]; hostSegment {
		return nil, fmt.Errorf("plugin key %q is a host path segment", key)
	}
	// Taken from the whole candidate generation, so the outcome does not
	// depend on the admission order.
	otherKeys := make(map[string]struct{})
	builtInPrefixes := make(map[string]string)
	for _, other := range candidate.Plugins() {
		if other.Meta.Key == key {
			continue
		}
		otherKeys[other.Meta.Key] = struct{}{}
		if !candidate.FactoryKey(other.Meta.Key) {
			continue
		}
		for _, binding := range jsplugin.PluginRouteBindings(other) {
			if binding.OwnPrefix {
				first, _, _ := strings.Cut(strings.TrimPrefix(binding.Route.Path, "/"), "/")
				builtInPrefixes[first] = other.Meta.Key
			}
		}
	}
	if owner, owned := builtInPrefixes[key]; owned {
		return nil, fmt.Errorf("plugin key %q is a prefix owned by built-in plugin %s", key, owner)
	}

	var skipped []jsplugin.SkippedRoute
	for _, binding := range jsplugin.PluginRouteBindings(plugin) {
		if binding.Pinned {
			continue
		}
		route := binding.Route
		first, _, _ := strings.Cut(strings.TrimPrefix(route.Path, "/"), "/")
		_, hostSegment := b.hostSegments[first]
		_, otherKey := otherKeys[first]
		_, builtInPrefix := builtInPrefixes[first]
		reason := ""
		switch {
		case hostSegment && !candidate.FactoryKey(key):
			reason = "host_namespace"
		case b.routeAnsweredByHost(route):
			reason = "host_route"
		case (otherKey || builtInPrefix) && !binding.OwnPrefix:
			reason = "key_namespace"
		}
		if reason != "" {
			skipped = append(skipped, jsplugin.SkippedRoute{PluginKey: key, Route: route, Reason: reason})
		}
	}
	return skipped, nil
}

// routeAnsweredByHost reports whether a static route of the same method, or
// the trailing-slash redirect it causes, answers requests the plugin route
// could serve. The handoff routes offer matching requests to plugin routes
// first, so only their redirect counts.
func (b *Policy) routeAnsweredByHost(route jsplugin.Route) bool {
	for _, staticRoute := range b.staticRoutes {
		if route.Method != staticRoute.Method {
			continue
		}
		_, offersToPlugins := pluginHandoffRoutes[staticRoute.Method+" "+staticRoute.Path]
		if routeIntersectsStaticRoute(route, staticRoute.Path, offersToPlugins) {
			return true
		}
	}
	return false
}

func sortedPluginKeys(plugins map[string]*jsplugin.LoadedPlugin) []string {
	keys := make([]string, 0, len(plugins))
	for key := range plugins {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedPlugins(plugins map[string]*jsplugin.LoadedPlugin) []*jsplugin.LoadedPlugin {
	keys := sortedPluginKeys(plugins)
	sorted := make([]*jsplugin.LoadedPlugin, 0, len(keys))
	for _, key := range keys {
		sorted = append(sorted, plugins[key])
	}
	return sorted
}

type routePatternSegment struct {
	value    string
	dynamic  bool
	catchAll bool
	// taskID marks a query route's task-id parameter, which only ever
	// receives gateway task ids and so never equals a literal segment.
	taskID bool
}

func parseRoutePattern(routePath string) []routePatternSegment {
	parts := strings.Split(strings.TrimPrefix(routePath, "/"), "/")
	segments := make([]routePatternSegment, 0, len(parts))
	for _, part := range parts {
		segments = append(segments, routePatternSegment{
			value:    part,
			dynamic:  strings.HasPrefix(part, ":") || strings.HasPrefix(part, "*"),
			catchAll: strings.HasPrefix(part, "*"),
		})
	}
	return segments
}

func RouteMatchesPath(pattern, escapedPath string) bool {
	segments := parseRoutePattern(pattern)
	index := 0
	for encodedPart := range strings.SplitSeq(strings.TrimPrefix(escapedPath, "/"), "/") {
		part, err := url.PathUnescape(encodedPart)
		if err != nil {
			return false
		}
		if index >= len(segments) {
			return false
		}
		segment := segments[index]
		if segment.catchAll {
			return true
		}
		if segment.dynamic && part == "" || !segment.dynamic && segment.value != part {
			return false
		}
		index++
	}
	return index == len(segments)
}

func RoutePatternsIntersect(leftPath, rightPath string) bool {
	return routeSegmentsIntersect(parseRoutePattern(leftPath), parseRoutePattern(rightPath))
}

func routeSegmentsIntersect(left, right []routePatternSegment) bool {
	for index := 0; ; index++ {
		leftDone := index >= len(left)
		rightDone := index >= len(right)
		if leftDone || rightDone {
			return leftDone && rightDone
		}
		if left[index].catchAll || right[index].catchAll {
			return true
		}
		if !left[index].dynamic && !right[index].dynamic && left[index].value != right[index].value {
			return false
		}
		if (left[index].dynamic && right[index].value == "") || (right[index].dynamic && left[index].value == "") {
			return false
		}
		if (left[index].taskID && !right[index].dynamic) || (right[index].taskID && !left[index].dynamic) {
			return false
		}
	}
}

// routeIntersectsStaticRoute reports whether a request the plugin route could
// serve is answered first by the static route or by the trailing-slash
// redirect it causes. A query route's task-id parameter meets only the static
// route's wildcards. A static route that offers matching requests to the
// plugin routes first does not answer them itself, but its redirect still does.
func routeIntersectsStaticRoute(route jsplugin.Route, staticPath string, offersToPlugins bool) bool {
	pluginSegments := parseRoutePattern(route.Path)
	if route.Type == jsplugin.RouteTypeQuery {
		for index := range pluginSegments {
			pluginSegments[index].taskID = pluginSegments[index].value == ":"+route.TaskIDParam
		}
	}
	if !offersToPlugins && routeSegmentsIntersect(pluginSegments, parseRoutePattern(staticPath)) {
		return true
	}
	if catchAllIndex := strings.LastIndex(staticPath, "/*"); catchAllIndex >= 0 && catchAllIndex+2 < len(staticPath) {
		prefix := staticPath[:catchAllIndex]
		return routeSegmentsIntersect(pluginSegments, parseRoutePattern(prefix)) || routeSegmentsIntersect(pluginSegments, parseRoutePattern(prefix+"/"))
	}
	if staticPath == "/" {
		return false
	}
	alternate := strings.TrimSuffix(staticPath, "/")
	if alternate == staticPath {
		alternate += "/"
	}
	return routeSegmentsIntersect(pluginSegments, parseRoutePattern(alternate))
}
