package model

import (
	"slices"

	"github.com/QuantumNous/new-api/pkg/jsplugin"
)

// TaskAliasTarget describes an alias in a captured legacy migration source.
// Declared is empty when the source resolves one plugin alias to multiple tails.
type TaskAliasTarget struct {
	Alias     string
	Declared  string
	PluginKey string
}

// ResolveTaskPluginModel matches only a pinned generation's declarations.
// Public Offering aliases are resolved by authorized Channel facts, never by
// Provider Models/ModelMapping. A nil g matches exact names only.
func ResolveTaskPluginModel(g *jsplugin.RoutingGeneration, name string, models []string) (served, declared string, ok bool) {
	if slices.Contains(models, name) {
		return name, name, true
	}
	if g != nil {
		if canonical, found := g.CanonicalModel(name); found && slices.Contains(models, canonical) {
			return canonical, canonical, true
		}
	}
	return "", "", false
}

// followChannelModelMapping walks a captured legacy Provider mapping. A self-map
// stops at the current hop; a non-self cycle is reported to the offline caller.
func followChannelModelMapping(modelMap map[string]string, start string) (string, bool) {
	current := start
	visited := map[string]bool{current: true}
	for {
		mapped, exists := modelMap[current]
		if !exists || mapped == "" {
			return current, false
		}
		if visited[mapped] {
			if mapped == current {
				return current, false
			}
			return "", true
		}
		visited[mapped] = true
		current = mapped
	}
}
