// Package capability defines the capabilities a Model declares. A Channel
// narrows the capabilities of its Model with the same Set type.
package capability

import (
	"database/sql/driver"
	"fmt"
	"slices"

	"github.com/QuantumNous/new-api/common"
)

// Feature is a request feature that a Model can handle.
type Feature string

const (
	Streaming        Feature = "streaming"
	ToolCalling      Feature = "tool_calling"
	Reasoning        Feature = "reasoning"
	StructuredOutput Feature = "structured_output"
	Caching          Feature = "caching"
)

// Features lists every Feature in canonical order.
var Features = []Feature{Streaming, ToolCalling, Reasoning, StructuredOutput, Caching}

// Modality is a kind of content a Model accepts or produces.
type Modality string

const (
	Text  Modality = "text"
	Image Modality = "image"
	Audio Modality = "audio"
	Video Modality = "video"
	PDF   Modality = "pdf"
)

// Modalities lists every Modality in canonical order.
var Modalities = []Modality{Text, Image, Audio, Video, PDF}

// Limits holds numeric upper bounds. A nil limit is not declared; a declared
// limit is a positive integer.
type Limits struct {
	ContextLength       *int64 `json:"context_length,omitempty"`
	MaxOutputTokens     *int64 `json:"max_output_tokens,omitempty"`
	MaxImages           *int64 `json:"max_images,omitempty"`
	MaxDurationSeconds  *int64 `json:"max_duration_seconds,omitempty"`
	EmbeddingDimensions *int64 `json:"embedding_dimensions,omitempty"`
}

// Set is a full capability declaration. It is stored as one JSON column.
type Set struct {
	Features         []Feature  `json:"features"`
	InputModalities  []Modality `json:"input_modalities"`
	OutputModalities []Modality `json:"output_modalities"`
	Limits           Limits     `json:"limits"`
}

// Normalize validates a declaration and returns it with duplicates removed
// and values in canonical order. Errors are web console messages.
func (s Set) Normalize() (Set, error) {
	features, err := canonical(s.Features, Features, "Unknown capability {{value}}")
	if err != nil {
		return Set{}, err
	}
	input, err := canonical(s.InputModalities, Modalities, "Unknown modality {{value}}")
	if err != nil {
		return Set{}, err
	}
	output, err := canonical(s.OutputModalities, Modalities, "Unknown modality {{value}}")
	if err != nil {
		return Set{}, err
	}
	limits := map[string]*int64{
		"context_length":       s.Limits.ContextLength,
		"max_output_tokens":    s.Limits.MaxOutputTokens,
		"max_images":           s.Limits.MaxImages,
		"max_duration_seconds": s.Limits.MaxDurationSeconds,
		"embedding_dimensions": s.Limits.EmbeddingDimensions,
	}
	for name, limit := range limits {
		if limit != nil && *limit <= 0 {
			return Set{}, common.NewMessage("Capability limit {{name}} must be a positive integer", map[string]any{"name": name})
		}
	}
	return Set{Features: features, InputModalities: input, OutputModalities: output, Limits: s.Limits}, nil
}

// canonical keeps the known values of all in their canonical order and
// rejects any value outside all.
func canonical[T ~string](values, all []T, unknownKey string) ([]T, error) {
	for _, value := range values {
		if !slices.Contains(all, value) {
			return nil, common.NewMessage(unknownKey, map[string]any{"value": string(value)})
		}
	}
	result := make([]T, 0, len(values))
	for _, value := range all {
		if slices.Contains(values, value) {
			result = append(result, value)
		}
	}
	return result, nil
}

// Scan reads a JSON column. NULL, written before capabilities existed, is an
// empty declaration.
func (s *Set) Scan(value any) error {
	var data []byte
	switch typed := value.(type) {
	case nil:
	case []byte:
		data = typed
	case string:
		data = []byte(typed)
	default:
		return fmt.Errorf("unsupported capability column type %T", value)
	}
	*s = Set{}
	if len(data) > 0 {
		if err := common.Unmarshal(data, s); err != nil {
			return err
		}
	}
	s.Features = nonNil(s.Features)
	s.InputModalities = nonNil(s.InputModalities)
	s.OutputModalities = nonNil(s.OutputModalities)
	return nil
}

// Value writes a JSON column. It returns a string because PostgreSQL's simple
// protocol encodes []byte as bytea.
func (s Set) Value() (driver.Value, error) {
	s.Features = nonNil(s.Features)
	s.InputModalities = nonNil(s.InputModalities)
	s.OutputModalities = nonNil(s.OutputModalities)
	encoded, err := common.Marshal(s)
	if err != nil {
		return nil, err
	}
	return string(encoded), nil
}

func nonNil[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}
