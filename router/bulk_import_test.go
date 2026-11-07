package router

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newBulkImportServer(t *testing.T) (*gin.Engine, *model.User, string, model.Model, model.Provider) {
	t.Helper()
	engine, actor, token := newAdminTestServer(t)
	require.NoError(t, model.DB.Model(actor).Update("role", common.RoleRootUser).Error)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.ModelOffering{}, &model.ModelOfferingMember{}, &model.ModelOfferingName{}, &model.TaskPlugin{}))
	require.NoError(t, model.MigratePricing(model.DB))
	t.Cleanup(model.InstallPriceVersionCheckers)
	clock.UseFake(t, time.Unix(1800000000, 0))
	publisher := model.Vendor{Name: "Bulk publisher", Status: 1}
	require.NoError(t, publisher.Insert())
	unified := model.Model{ModelName: "bulk-model", VendorID: publisher.Id, Status: 1}
	require.NoError(t, unified.Insert())
	provider := model.Provider{Name: "Existing account", Type: 1, Status: common.ChannelStatusEnabled, Key: "fixture-credential"}
	require.NoError(t, model.DB.Create(&provider).Error)
	return engine, actor, token, unified, provider
}

func decodeBulkResponse(t *testing.T, response *httptest.ResponseRecorder, status int) model.BulkImportResult {
	t.Helper()
	require.Equal(t, status, response.Code, response.Body.String())
	var reply struct{ Data model.BulkImportResult }
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &reply), response.Body.String())
	return reply.Data
}

// Business rows and sequence values are part of the preview's no-write contract.
// The operation audit is deliberately verified separately.
func bulkBusinessState(t *testing.T) string {
	t.Helper()
	var state struct {
		Providers, Channels, Prices int64
		Sequences                   []struct {
			Name string
			Last *int64
		}
	}
	require.NoError(t, model.DB.Model(&model.Provider{}).Count(&state.Providers).Error)
	require.NoError(t, model.DB.Model(&model.Channel{}).Count(&state.Channels).Error)
	require.NoError(t, model.DB.Model(&model.PriceVersion{}).Count(&state.Prices).Error)
	require.NoError(t, model.DB.Raw(`SELECT "sequencename" AS "name", "last_value" AS "last" FROM "pg_sequences" WHERE "schemaname" = current_schema() AND ("sequencename" LIKE 'providers%' OR "sequencename" LIKE 'channels%' OR "sequencename" LIKE 'price_versions%') ORDER BY "sequencename"`).Scan(&state.Sequences).Error)
	encoded, err := common.Marshal(state)
	require.NoError(t, err)
	return string(encoded)
}

func TestBulkImportHTTP(t *testing.T) {
	engine, actor, token, unified, _ := newBulkImportServer(t)
	secret := "credential-preview-sentinel"
	body := fmt.Sprintf(`{"rows":[
 {"resource":"providers","action":"create","ref":"p","data":{"name":"Imported account","type":1,"key":%q,"base_url":"https://user:url-sentinel@example.com/?token=query-sentinel","header_override":"{\"Authorization\":\"header-sentinel\"}"}},
 {"resource":"channels","action":"create","ref":"c","data":{"provider_ref":"p","model_id":%d,"protocol":"openai_chat","upstream_model_name":"imported-model"}},
 {"resource":"channels","action":"confirm","data":{"channel_ref":"c"}},
 {"resource":"price-versions","action":"create","data":{"owner_type":"channel","owner_ref":"c","currency":"USD","expression":"tier(\"free\", fixed(0))","effective_at":%d}},
 {"resource":"channels","action":"enable","data":{"channel_ref":"c"}}
 ]}`, secret, unified.Id, clock.Now().Unix()-1)
	before := bulkBusinessState(t)
	preview := adminSecurityRequest(engine, token, "POST", "/api/admin/v1/bulk-import?dry_run=true", body)
	result := decodeBulkResponse(t, preview, 200)
	require.True(t, result.Valid)
	assert.False(t, result.Committed)
	require.Len(t, result.Rows, 5)
	assert.Equal(t, 5, result.Rows[4].Row)
	assert.Equal(t, model.ChannelEnabled, result.Rows[4].Status)
	assert.Equal(t, before, bulkBusinessState(t))
	for _, sentinel := range []string{secret, "url-sentinel", "query-sentinel", "header-sentinel"} {
		assert.NotContains(t, preview.Body.String(), sentinel)
	}
	commit := adminSecurityRequest(engine, token, "POST", "/api/admin/v1/bulk-import", body)
	result = decodeBulkResponse(t, commit, 200)
	require.True(t, result.Committed)
	channelPath := fmt.Sprintf("/api/admin/v1/channels/%d", result.Rows[4].ID)
	channel := adminSecurityRequest(engine, token, "GET", channelPath, "")
	require.Equal(t, 200, channel.Code)
	assert.Contains(t, channel.Body.String(), `"status":"enabled"`)
	price := adminSecurityRequest(engine, token, "GET", fmt.Sprintf("/api/admin/v1/price-versions/%d", result.Rows[3].ID), "")
	require.Equal(t, 200, price.Code)
	var priced struct{ Data model.PriceVersion }
	require.NoError(t, common.Unmarshal(price.Body.Bytes(), &priced))
	assert.Equal(t, actor.Id, priced.Data.CreatedBy)
	assert.Equal(t, `tier("free", fixed(0))`, priced.Data.Expression)
	for _, sentinel := range []string{secret, "url-sentinel", "query-sentinel", "header-sentinel"} {
		assert.NotContains(t, commit.Body.String(), sentinel)
	}
	audits, _, err := model.GetAuditLogs(model.AuditLogFilter{Category: model.AuditCategoryOperation}, 0, 100, common.RoleRootUser)
	require.NoError(t, err)
	encoded, err := common.Marshal(audits)
	require.NoError(t, err)
	for _, sentinel := range []string{secret, "url-sentinel", "query-sentinel", "header-sentinel", token} {
		assert.NotContains(t, string(encoded), sentinel)
	}
	assert.Contains(t, string(encoded), "bulk_import.execute")
	assert.Contains(t, string(encoded), `"dry_run":true`)
	// A later validation failure cannot leave the preceding Provider or Channel.
	bad := strings.Replace(body, `"currency":"USD"`, `"currency":"invalid"`, 1)
	before = bulkBusinessState(t)
	result = decodeBulkResponse(t, adminSecurityRequest(engine, token, "POST", "/api/admin/v1/bulk-import", bad), 400)
	assert.False(t, result.Valid)
	assert.NotEmpty(t, result.Rows[3].Error)
	assert.Equal(t, before, bulkBusinessState(t))
}

func TestBulkImportStrictParsingHTTP(t *testing.T) {
	engine, _, token, unified, provider := newBulkImportServer(t)
	input := fmt.Sprintf(`{"provider_id":%d,"model_id":%d,"protocol":"openai_chat","upstream_model_name":"csv-model"}`, provider.Id, unified.Id)
	var buffer bytes.Buffer
	writer := csv.NewWriter(&buffer)
	require.NoError(t, writer.WriteAll([][]string{{"resource", "action", "ref", "data"}, {"channels", "create", "c", input}, {"channels", "confirm", "", `{"channel_ref":"c"}`}}))
	request := httptest.NewRequest("POST", "/api/admin/v1/bulk-import?dry_run=true", strings.NewReader(buffer.String()))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "text/csv")
	recorder := httptest.NewRecorder()
	before := bulkBusinessState(t)
	engine.ServeHTTP(recorder, request)
	result := decodeBulkResponse(t, recorder, 200)
	require.True(t, result.Valid)
	require.Len(t, result.Rows, 2)
	assert.Equal(t, 2, result.Rows[0].Row)
	assert.Equal(t, 3, result.Rows[1].Row)
	assert.Equal(t, before, bulkBusinessState(t))
	request = httptest.NewRequest("POST", "/api/admin/v1/bulk-import", strings.NewReader(buffer.String()))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "text/csv")
	recorder = httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	result = decodeBulkResponse(t, recorder, 200)
	assert.True(t, result.Committed)
	for _, tc := range []struct {
		name, format, body string
		status             int
	}{
		{"unknown envelope", "application/json", `{"rows":[],"unexpected":"credential-sentinel"}`, 400},
		{"trailing JSON", "application/json", `{"rows":[]} {}`, 400},
		{"missing rows", "application/json", `{}`, 400},
		{"duplicate columns", "text/csv", "resource,resource,data\nproviders,providers,{}\n", 400},
		{"unknown column", "text/csv", "resource,action,credential-sentinel,data\nproviders,create,x,{}\n", 400},
		{"ragged CSV", "text/csv", "resource,action,data\nproviders,create\n", 400},
		{"content type", "text/plain", "credential-sentinel", 415},
		{"upload bound", "application/json", strings.Repeat(" ", 2<<20) + "{}", 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/api/admin/v1/bulk-import?dry_run=true", strings.NewReader(tc.body))
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Content-Type", tc.format)
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)
			assert.Equal(t, tc.status, response.Code, response.Body.String())
			assert.NotContains(t, response.Body.String(), "credential-sentinel")
			if tc.format == "text/csv" {
				result := decodeBulkResponse(t, response, tc.status)
				require.Len(t, result.Rows, 1)
				assert.Positive(t, result.Rows[0].Row)
			}
		})
	}
	for _, data := range []string{
		`{"name":"invalid","type":999,"key":"credential-sentinel"}`,
		`{"name":"missing credential","type":1}`,
		`{"name":"invalid","type":1,"key":"credential-sentinel","unknown":true}`,
	} {
		body := `{"rows":[{"resource":"providers","action":"create","data":` + data + `}]}`
		result := decodeBulkResponse(t, adminSecurityRequest(engine, token, "POST", "/api/admin/v1/bulk-import?dry_run=true", body), 400)
		require.Len(t, result.Rows, 1)
		assert.Equal(t, 1, result.Rows[0].Row)
		assert.NotEmpty(t, result.Rows[0].Error)
	}
	for _, data := range []string{strings.Replace(input, `"provider_id":`, `"capabilities":{"features":["reasoning"]},"provider_id":`, 1), strings.Replace(input, `"provider_id":`, `"rpm":1.5,"provider_id":`, 1), strings.Replace(input, `"provider_id":`, `"rpm":-1,"provider_id":`, 1), strings.Replace(input, `"provider_id":`, `"weight":9223372036854775808,"provider_id":`, 1), strings.Replace(input, `"openai_chat"`, `"invalid"`, 1)} {
		body := `{"rows":[{"resource":"channels","action":"create","data":` + data + `}]}`
		decodeBulkResponse(t, adminSecurityRequest(engine, token, "POST", "/api/admin/v1/bulk-import?dry_run=true", body), 400)
	}
	rows := make([]map[string]any, 501)
	for i := range rows {
		rows[i] = map[string]any{"resource": "providers", "action": "create", "data": map[string]any{"name": "too many", "type": 1, "key": "credential-sentinel"}}
	}
	encoded, err := common.Marshal(map[string]any{"rows": rows})
	require.NoError(t, err)
	response := adminSecurityRequest(engine, token, "POST", "/api/admin/v1/bulk-import", string(encoded))
	assert.Equal(t, 400, response.Code)
}

func TestBulkImportActivationAndUniqueIdentityHTTP(t *testing.T) {
	engine, actor, token, unified, provider := newBulkImportServer(t)
	channelRow := fmt.Sprintf(`{"resource":"channels","action":"create","ref":"c","data":{"provider_id":%d,"model_id":%d,"protocol":"openai_chat","upstream_model_name":"atomic-model"}}`, provider.Id, unified.Id)
	confirm := `{"resource":"channels","action":"confirm","data":{"channel_ref":"c"}}`
	enable := `{"resource":"channels","action":"enable","data":{"channel_ref":"c"}}`
	futurePrice := fmt.Sprintf(`{"resource":"price-versions","action":"create","data":{"owner_type":"channel","owner_ref":"c","currency":"USD","expression":"tier(\"free\", fixed(0))","effective_at":%d}}`, clock.Now().Unix()+3600)
	for _, rows := range []string{channelRow + "," + enable, channelRow + "," + confirm + "," + enable, channelRow + "," + confirm + "," + futurePrice + "," + enable, channelRow + "," + strings.Replace(channelRow,`"ref":"c"`,`"ref":"c2"`,1)} {
		before := bulkBusinessState(t)
		for _, suffix := range []string{"?dry_run=true", ""} {
			result := decodeBulkResponse(t, adminSecurityRequest(engine, token, "POST", "/api/admin/v1/bulk-import"+suffix, `{"rows":[`+rows+`]}`), 400)
			assert.False(t, result.Committed)
		}
		assert.Equal(t, before, bulkBusinessState(t))
	}
	// Two independent requests for the same identity cannot both commit.
	var wg sync.WaitGroup
	responses := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		wg.Go(func() {
			responses <- adminSecurityRequest(engine, token, "POST", "/api/admin/v1/bulk-import", `{"rows":[`+channelRow+`]}`)
		})
	}
	wg.Wait()
	close(responses)
	successes := 0
	for response := range responses {
		if response.Code == 200 {
			successes++
		} else {
			assert.Contains(t, []int{400, 409}, response.Code, response.Body.String())
		}
	}
	assert.Equal(t, 1, successes)
	var channel model.Channel
	require.NoError(t, model.DB.Where("upstream_model_name = ?", "atomic-model").First(&channel).Error)
	// Dedicated action routes work with only the existing operate scope.
	operate := createWalletTestAccessToken(t, actor.Id, "channel:operate")
	body := fmt.Sprintf(`{"rows":[{"resource":"channels","action":"confirm","data":{"id":%d}}]}`, channel.ID)
	before:=bulkBusinessState(t)
 decodeBulkResponse(t,adminSecurityRequest(engine,operate,"POST","/api/admin/v1/channels/bulk-confirm?dry_run=true",body),200)
 response:=adminSecurityRequest(engine,token,"GET",fmt.Sprintf("/api/admin/v1/channels/%d",channel.ID),"")
 require.Equal(t,200,response.Code,response.Body.String());assert.Contains(t,response.Body.String(),`"mapping_confirmed":false`)
 assert.Equal(t,before,bulkBusinessState(t))
 result := decodeBulkResponse(t, adminSecurityRequest(engine, operate, "POST", "/api/admin/v1/channels/bulk-confirm", body), 200)
	assert.True(t, result.Committed)
	price := model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: channel.ID, Currency: "USD", Expression: `tier("free", fixed(0))`, EffectiveAt: clock.Now().Unix()}
	require.NoError(t, model.CreatePriceVersion(t.Context(), &price, actor.Id))
	require.NoError(t, model.DB.Model(&provider).Update("status", common.ChannelStatusManuallyDisabled).Error)
	enableBody := fmt.Sprintf(`{"rows":[{"resource":"channels","action":"enable","data":{"id":%d}}]}`, channel.ID)
	decodeBulkResponse(t, adminSecurityRequest(engine, operate, "POST", "/api/admin/v1/channels/bulk-enable", enableBody), 400)
	require.NoError(t, model.DB.Model(&provider).Update("status", common.ChannelStatusEnabled).Error)
	decodeBulkResponse(t, adminSecurityRequest(engine, operate, "POST", "/api/admin/v1/channels/bulk-enable", enableBody), 200)
	// Imported history remains append-only and the creator cannot be supplied.
	forged := fmt.Sprintf(`{"rows":[{"resource":"price-versions","action":"create","data":{"owner_type":"channel","owner_id":%d,"currency":"USD","expression":"p * 2","created_by":999}}]}`, channel.ID)
	decodeBulkResponse(t, adminSecurityRequest(engine, token, "POST", "/api/admin/v1/price-versions/import", forged), 400)
	duplicate := fmt.Sprintf(`{"rows":[{"resource":"price-versions","action":"create","data":{"owner_type":"channel","owner_id":%d,"currency":"USD","expression":"p * 2","effective_at":%d}}]}`, channel.ID, price.EffectiveAt)
	decodeBulkResponse(t, adminSecurityRequest(engine, token, "POST", "/api/admin/v1/price-versions/import", duplicate), 400)
	require.Error(t, model.DB.Model(&price).Update("expression", "p * 8").Error)
}

func TestBulkImportAuthorityHTTP(t *testing.T) {
	engine, actor, token, _, _ := newBulkImportServer(t)
	body := `{"rows":[{"resource":"providers","action":"create","data":{"name":"No authority bypass","type":1,"key":"authority-sentinel"}}]}`
	read := createWalletTestAccessToken(t, actor.Id, "channel:read")
	write := createWalletTestAccessToken(t, actor.Id, "channel:write")
	expired := createWalletTestAccessToken(t, actor.Id, "channel:write", "channel:sensitive_write")
	require.NoError(t, model.DB.Model(&model.UserAccessToken{}).Where("token_hash = ?", model.AccessTokenFingerprint(expired)).Update("expires_at", time.Now().Unix()-1).Error)
	bundle, err := service.CreateLoginSession(actor.Id, "password", "127.0.0.1", "bulk regression")
	require.NoError(t, err)
	_, err = model.RevokeUserSession(actor.Id, bundle.Session.SID, "test")
	require.NoError(t, err)
	for _, tc := range []struct {
		token  string
		status int
	}{{"", 401}, {"invalid", 401}, {read, 403}, {write, 403}, {expired, 401}, {bundle.AccessToken, 401}} {
		before := bulkBusinessState(t)
		response := adminSecurityRequest(engine, tc.token, "POST", "/api/admin/v1/bulk-import?dry_run=true", body)
		assert.Equal(t, tc.status, response.Code, response.Body.String())
		assert.NotContains(t, response.Body.String(), "authority-sentinel")
		assert.Equal(t, before, bulkBusinessState(t))
	}
	require.NoError(t, model.DB.Model(actor).Update("role", common.RoleAdminUser).Error)
	require.NoError(t, authz.SetUserPermissions(actor.Id, authz.PermissionsMap{"channel": {"write": false, "sensitive_write": false}}))
	response := adminSecurityRequest(engine, token, "POST", "/api/admin/v1/bulk-import", body)
	assert.Equal(t, 403, response.Code, response.Body.String())
	// A resource-specific route cannot be used to smuggle another resource.
	sensitive := createWalletTestAccessToken(t, actor.Id, "pricing:write")
	response = adminSecurityRequest(engine, sensitive, "POST", "/api/admin/v1/price-versions/import", body)
	assert.Equal(t, 400, response.Code, response.Body.String())
}
