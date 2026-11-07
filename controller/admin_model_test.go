package controller

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type adminModelEnvelope struct {
	Success    bool           `json:"success"`
	Message    string         `json:"message"`
	MessageKey string         `json:"message_key"`
	Data       map[string]any `json:"data"`
}

func adminModelServer(t *testing.T) *gin.Engine {
	t.Helper()
	modelManagementDB(t)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set("id", 1)
		c.Set("role", common.RoleAdminUser)
	})
	group := engine.Group("/api/admin/v1/models")
	group.GET("", AdminListModels)
	group.GET("/:id", AdminGetModel)
	group.POST("", AdminCreateModel)
	group.PUT("/:id", AdminUpdateModel)
	group.DELETE("/:id", AdminDeleteModel)
	return engine
}

func adminModelCall(t *testing.T, engine *gin.Engine, method, path string, body any) (int, adminModelEnvelope) {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		encoded, err := common.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(encoded)
	}
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(method, path, reader))
	var envelope adminModelEnvelope
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &envelope), recorder.Body.String())
	return recorder.Code, envelope
}

func adminModelPublisher(t *testing.T, name string) int {
	t.Helper()
	publisher := model.Vendor{Name: name, Status: 1}
	require.NoError(t, publisher.Insert())
	return publisher.Id
}

func TestAdminModelsDatabase(t *testing.T) {
	engine := adminModelServer(t)
	openAI := adminModelPublisher(t, "OpenAI")
	alibaba := adminModelPublisher(t, "Alibaba")

	t.Run("declares capabilities in canonical form", func(t *testing.T) {
		status, created := adminModelCall(t, engine, http.MethodPost, "/api/admin/v1/models", map[string]any{
			"name":         "gpt-4o",
			"publisher_id": openAI,
			"capabilities": map[string]any{
				"features":          []string{"tool_calling", "streaming", "streaming", "caching"},
				"input_modalities":  []string{"pdf", "image", "text"},
				"output_modalities": []string{"text"},
				"limits":            map[string]any{"context_length": 128000, "max_output_tokens": 16384, "max_images": 10},
			},
		})
		require.Equal(t, http.StatusOK, status, created.Message)
		require.True(t, created.Success)
		id := int(created.Data["id"].(float64))

		status, fetched := adminModelCall(t, engine, http.MethodGet, fmt.Sprintf("/api/admin/v1/models/%d", id), nil)
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, "gpt-4o", fetched.Data["name"])
		assert.EqualValues(t, openAI, fetched.Data["publisher_id"])
		assert.EqualValues(t, 1, fetched.Data["status"])
		assert.Equal(t, map[string]any{
			"features":          []any{"streaming", "tool_calling", "caching"},
			"input_modalities":  []any{"text", "image", "pdf"},
			"output_modalities": []any{"text"},
			"limits":            map[string]any{"context_length": float64(128000), "max_output_tokens": float64(16384), "max_images": float64(10)},
		}, fetched.Data["capabilities"])
	})

	t.Run("keeps variant names as separate models", func(t *testing.T) {
		variants := map[string]int64{
			"gpt-4o-2024-08-06":          128000,
			"qwen2.5-7b-instruct":        32768,
			"qwen2.5-7b-instruct-q4_k_m": 8192,
			"qwen2.5-7b-instruct-128k":   131072,
			"Qwen2.5-7B-Instruct":        65536,
			"claude-3-5-sonnet-20241022": 200000,
			"claude-3-5-sonnet-20240620": 100000,
		}
		ids := make(map[string]int, len(variants))
		for name, contextLength := range variants {
			status, created := adminModelCall(t, engine, http.MethodPost, "/api/admin/v1/models", map[string]any{
				"name": name, "publisher_id": alibaba,
				"capabilities": map[string]any{"limits": map[string]any{"context_length": contextLength}},
			})
			require.Equal(t, http.StatusOK, status, name+": "+created.Message)
			ids[name] = int(created.Data["id"].(float64))
		}
		for name, contextLength := range variants {
			_, fetched := adminModelCall(t, engine, http.MethodGet, fmt.Sprintf("/api/admin/v1/models/%d", ids[name]), nil)
			assert.Equal(t, name, fetched.Data["name"])
			limits := fetched.Data["capabilities"].(map[string]any)["limits"].(map[string]any)
			assert.EqualValues(t, contextLength, limits["context_length"], name)
		}

		status, duplicate := adminModelCall(t, engine, http.MethodPost, "/api/admin/v1/models", map[string]any{"name": "qwen2.5-7b-instruct", "publisher_id": alibaba})
		assert.Equal(t, http.StatusConflict, status)
		assert.Equal(t, "Model name already exists", duplicate.MessageKey)
	})

	t.Run("rejects invalid declarations", func(t *testing.T) {
		cases := []struct {
			name string
			body map[string]any
			key  string
		}{
			{"missing name", map[string]any{"publisher_id": openAI}, "Model name cannot be empty"},
			{"missing publisher", map[string]any{"name": "no-publisher"}, "Publisher is required"},
			{"unknown publisher", map[string]any{"name": "ghost-publisher", "publisher_id": 999999}, "Publisher does not exist"},
			{"unknown feature", map[string]any{"name": "bad-feature", "publisher_id": openAI, "capabilities": map[string]any{"features": []string{"telepathy"}}}, "Unknown capability {{value}}"},
			{"unknown modality", map[string]any{"name": "bad-modality", "publisher_id": openAI, "capabilities": map[string]any{"output_modalities": []string{"smell"}}}, "Unknown modality {{value}}"},
			{"zero limit", map[string]any{"name": "zero-limit", "publisher_id": openAI, "capabilities": map[string]any{"limits": map[string]any{"embedding_dimensions": 0}}}, "Capability limit {{name}} must be a positive integer"},
			{"invalid status", map[string]any{"name": "bad-status", "publisher_id": openAI, "status": 2}, "Invalid catalog visibility"},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				status, response := adminModelCall(t, engine, http.MethodPost, "/api/admin/v1/models", testCase.body)
				assert.Equal(t, http.StatusBadRequest, status, response.Message)
				assert.False(t, response.Success)
				assert.Equal(t, testCase.key, response.MessageKey)
			})
		}
	})

	t.Run("updates and deletes a model", func(t *testing.T) {
		_, created := adminModelCall(t, engine, http.MethodPost, "/api/admin/v1/models", map[string]any{
			"name": "o3-mini", "publisher_id": openAI, "description": "first",
			"capabilities": map[string]any{"features": []string{"reasoning", "streaming"}},
		})
		path := fmt.Sprintf("/api/admin/v1/models/%d", int(created.Data["id"].(float64)))

		status, updated := adminModelCall(t, engine, http.MethodPut, path, map[string]any{
			"name": "o3-mini", "publisher_id": alibaba, "description": "second", "status": 0,
			"capabilities": map[string]any{"features": []string{"reasoning"}, "output_modalities": []string{"text"}},
		})
		require.Equal(t, http.StatusOK, status, updated.Message)
		_, fetched := adminModelCall(t, engine, http.MethodGet, path, nil)
		assert.EqualValues(t, alibaba, fetched.Data["publisher_id"])
		assert.Equal(t, "second", fetched.Data["description"])
		assert.EqualValues(t, 0, fetched.Data["status"])
		capabilities := fetched.Data["capabilities"].(map[string]any)
		assert.Equal(t, []any{"reasoning"}, capabilities["features"])
		assert.Equal(t, []any{"text"}, capabilities["output_modalities"])

		status, _ = adminModelCall(t, engine, http.MethodDelete, path, nil)
		require.Equal(t, http.StatusOK, status)
		status, missing := adminModelCall(t, engine, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusNotFound, status)
		assert.Equal(t, "Model not found", missing.MessageKey)
		status, _ = adminModelCall(t, engine, http.MethodPut, path, map[string]any{"name": "o3-mini", "publisher_id": openAI})
		assert.Equal(t, http.StatusNotFound, status)
	})

	t.Run("lists exact-name models only", func(t *testing.T) {
		rule := model.Model{ModelName: "gpt-4o", NameRule: model.NameRulePrefix, Status: 1}
		require.NoError(t, rule.Insert())
		status, listed := adminModelCall(t, engine, http.MethodGet, "/api/admin/v1/models?keyword=gpt-4o&page_size=100", nil)
		require.Equal(t, http.StatusOK, status)
		names := []string{}
		for _, item := range listed.Data["items"].([]any) {
			names = append(names, item.(map[string]any)["name"].(string))
		}
		assert.ElementsMatch(t, []string{"gpt-4o", "gpt-4o-2024-08-06"}, names)
		assert.EqualValues(t, 2, listed.Data["total"])

		status, _ = adminModelCall(t, engine, http.MethodGet, fmt.Sprintf("/api/admin/v1/models/%d", rule.Id), nil)
		assert.Equal(t, http.StatusNotFound, status)

		status, listed = adminModelCall(t, engine, http.MethodGet, fmt.Sprintf("/api/admin/v1/models?publisher_id=%d&page_size=100", openAI), nil)
		require.Equal(t, http.StatusOK, status)
		assert.EqualValues(t, 1, listed.Data["total"])
	})

	t.Run("legacy metadata API keeps capabilities", func(t *testing.T) {
		_, created := adminModelCall(t, engine, http.MethodPost, "/api/admin/v1/models", map[string]any{
			"name": "text-embedding-3-large", "publisher_id": openAI,
			"capabilities": map[string]any{"input_modalities": []string{"text"}, "limits": map[string]any{"embedding_dimensions": 3072}},
		})
		id := int(created.Data["id"].(float64))
		var legacy struct {
			Success bool
			Data    model.Model
		}
		modelManagementRequest(t, UpdateModelMeta, http.MethodPut, "/api/models/", map[string]any{
			"id": id, "model_name": "text-embedding-3-large", "vendor_id": openAI, "description": "legacy edit", "status": 1, "sync_official": 1,
		}, &legacy)
		require.True(t, legacy.Success)

		var stored model.Model
		require.NoError(t, model.DB.First(&stored, id).Error)
		assert.Equal(t, "legacy edit", stored.Description)
		require.NotNil(t, stored.Capabilities.Limits.EmbeddingDimensions)
		assert.EqualValues(t, 3072, *stored.Capabilities.Limits.EmbeddingDimensions)
	})
}
