package controller

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRetainedGatewayTaskFacts(t *testing.T) {
	operations := map[string]any{"operations": []any{map[string]any{"path": "seconds", "mode": "set", "value": "8"}}}
	t.Run("top-level override retains the changed fact", func(t *testing.T) {
		assert.Equal(t, map[string]any{"seconds": 8.0}, retainedGatewayTaskFacts(
			map[string]any{"seconds": 4.0}, map[string]any{"seconds": 8.0}, nil, map[string]any{"seconds": "8"}))
	})
	t.Run("operations override retains the changed fact", func(t *testing.T) {
		assert.Equal(t, map[string]any{"seconds": 8.0}, retainedGatewayTaskFacts(
			map[string]any{"seconds": 4.0}, map[string]any{"seconds": 8.0}, nil, operations))
	})
	t.Run("unrelated top-level key does not retain the client budget", func(t *testing.T) {
		assert.Nil(t, retainedGatewayTaskFacts(
			map[string]any{"seconds": 10.0}, map[string]any{"seconds": 10.0}, nil, map[string]any{"seconds": "8"}))
	})
	t.Run("same-value top-level override freezes the fact", func(t *testing.T) {
		assert.Equal(t, map[string]any{"seconds": 8.0}, retainedGatewayTaskFacts(
			map[string]any{"seconds": 8.0}, map[string]any{"seconds": 8.0}, nil, map[string]any{"seconds": "8"}))
	})
	t.Run("baseline failure keeps only top-level matches", func(t *testing.T) {
		err := errors.New("baseline extraction failed")
		assert.Equal(t, map[string]any{"seconds": 8.0}, retainedGatewayTaskFacts(
			nil, map[string]any{"seconds": 8.0}, err, map[string]any{"seconds": 8}))
		assert.Nil(t, retainedGatewayTaskFacts(
			nil, map[string]any{"seconds": 10.0}, err, map[string]any{"seconds": 8}))
	})
	t.Run("non-scalar facts are ignored", func(t *testing.T) {
		effective := map[string]any{"nested": map[string]any{"value": 1}, "seconds": 8.0}
		assert.Equal(t, map[string]any{"seconds": 8.0}, retainedGatewayTaskFacts(
			nil, effective, nil, map[string]any{"nested": map[string]any{"value": 1}, "seconds": 8}))
	})
	t.Run("no overrides retain nothing", func(t *testing.T) {
		assert.Nil(t, retainedGatewayTaskFacts(
			map[string]any{"seconds": 4.0}, map[string]any{"seconds": 8.0}, nil, nil))
	})
}
