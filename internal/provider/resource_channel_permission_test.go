package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelPermissionResource_Metadata(t *testing.T) {
	r := NewChannelPermissionResource()
	req := resource.MetadataRequest{
		ProviderTypeName: "discord",
	}
	resp := &resource.MetadataResponse{}

	r.Metadata(t.Context(), req, resp)

	assert.Equal(t, "discord_channel_permission", resp.TypeName)
}

func TestChannelPermissionResource_Schema(t *testing.T) {
	r := NewChannelPermissionResource()
	req := resource.SchemaRequest{}
	resp := &resource.SchemaResponse{}

	r.Schema(t.Context(), req, resp)

	assert.NotNil(t, resp.Schema)
	assert.Contains(t, resp.Schema.Description, "Creates and manages Discord channel permission overwrites")

	// Check required attributes
	requiredAttrs := []string{"channel_id", "type", "overwrite_id", "allow"}
	for _, attrName := range requiredAttrs {
		attr, ok := resp.Schema.Attributes[attrName]
		assert.True(t, ok, "Attribute %s should exist", attrName)
		assert.True(t, attr.IsRequired(), "Attribute %s should be required", attrName)
	}

	// Check optional attributes
	denyAttr, ok := resp.Schema.Attributes["deny"]
	assert.True(t, ok)
	assert.True(t, denyAttr.IsOptional())

	// Check computed attributes
	idAttr, ok := resp.Schema.Attributes["id"]
	assert.True(t, ok)
	assert.True(t, idAttr.IsComputed())
}

func TestChannelPermissionResource_Configure(t *testing.T) {
	tests := []struct {
		name          string
		providerData  interface{}
		expectError   bool
		errorContains string
	}{
		{
			name:         "valid discordgo.Session",
			providerData: &discordgo.Session{},
			expectError:  false,
		},
		{
			name:          "invalid provider data type",
			providerData:  "invalid",
			expectError:   true,
			errorContains: "Unexpected Resource Configure Type",
		},
		{
			name:         "nil provider data",
			providerData: nil,
			expectError:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &channelPermissionResource{}
			req := resource.ConfigureRequest{
				ProviderData: tt.providerData,
			}
			resp := &resource.ConfigureResponse{}

			r.Configure(t.Context(), req, resp)

			if tt.expectError {
				assert.True(t, resp.Diagnostics.HasError())
				if tt.errorContains != "" {
					assert.Contains(t, resp.Diagnostics.Errors()[0].Summary(), tt.errorContains)
				}
			} else {
				assert.False(t, resp.Diagnostics.HasError())
			}
		})
	}
}

const permissionTestChannelID = "1452601985235816601"

type permissionTestAPI struct {
	mu           sync.Mutex
	overwrites   map[string]discordgo.PermissionOverwrite
	requests     []string
	status       int
	err          error
	readBarrier  chan struct{}
	barrierReads int
}

// RoundTrip handles requests locally, without changing DiscordGo's global endpoints.
func (a *permissionTestAPI) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}

	a.mu.Lock()
	path := strings.TrimPrefix(req.URL.Path, "/api/v9")
	a.requests = append(a.requests, req.Method+" "+path)
	resp, err := a.respond(req, path)

	// Capture both GET responses before either caller can write. This makes the
	// old read-modify-PATCH implementation lose an overwrite deterministically.
	var barrier <-chan struct{}
	if req.Method == http.MethodGet && a.readBarrier != nil && a.barrierReads < 2 {
		a.barrierReads++
		barrier = a.readBarrier
		if a.barrierReads == 2 {
			close(a.readBarrier)
		}
	}
	a.mu.Unlock()

	if barrier != nil {
		select {
		case <-barrier:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	return resp, err
}

// respond runs under the mutex and models both per-target and whole-list writes.
func (a *permissionTestAPI) respond(req *http.Request, path string) (*http.Response, error) {
	if a.err != nil {
		return nil, a.err
	}
	if a.status != 0 {
		code := 50035 // Invalid Form Body.
		switch a.status {
		case http.StatusForbidden:
			code = 50013 // Missing Permissions.
		case http.StatusNotFound:
			code = 10003 // Unknown Channel.
		}
		return permissionTestResponse(req, a.status, fmt.Sprintf(`{"code":%d,"message":"test API error"}`, code)), nil
	}

	channelPath := "/channels/" + permissionTestChannelID
	if path == channelPath {
		switch req.Method {
		case http.MethodGet:
			return a.channelResponse(req)
		case http.MethodPatch:
			var channel discordgo.Channel
			if err := json.NewDecoder(req.Body).Decode(&channel); err != nil {
				return nil, err
			}
			a.overwrites = make(map[string]discordgo.PermissionOverwrite, len(channel.PermissionOverwrites))
			for _, overwrite := range channel.PermissionOverwrites {
				a.overwrites[overwrite.ID] = *overwrite
			}
			return a.channelResponse(req)
		}
	}

	prefix := channelPath + "/permissions/"
	if strings.HasPrefix(path, prefix) {
		targetID := strings.TrimPrefix(path, prefix)
		switch req.Method {
		case http.MethodPut:
			var overwrite discordgo.PermissionOverwrite
			if err := json.NewDecoder(req.Body).Decode(&overwrite); err != nil {
				return nil, err
			}
			overwrites := a.overwrites
			if overwrites == nil {
				overwrites = make(map[string]discordgo.PermissionOverwrite)
				a.overwrites = overwrites
			}
			overwrite.ID = targetID
			overwrites[targetID] = overwrite
			return permissionTestResponse(req, http.StatusNoContent, ""), nil
		case http.MethodDelete:
			delete(a.overwrites, targetID)
			return permissionTestResponse(req, http.StatusNoContent, ""), nil
		}
	}

	return permissionTestResponse(req, http.StatusBadRequest, `{"code":50035,"message":"unexpected endpoint"}`), nil
}

func (a *permissionTestAPI) channelResponse(req *http.Request) (*http.Response, error) {
	channel := discordgo.Channel{ID: permissionTestChannelID}
	for _, overwrite := range a.overwrites {
		channel.PermissionOverwrites = append(channel.PermissionOverwrites, &overwrite)
	}
	body, err := json.Marshal(channel)
	if err != nil {
		return nil, err
	}
	return permissionTestResponse(req, http.StatusOK, string(body)), nil
}

func permissionTestResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func newPermissionTestResource(t *testing.T, api *permissionTestAPI) *channelPermissionResource {
	t.Helper()
	session, err := discordgo.New("Bot test-token")
	require.NoError(t, err)
	session.Client = &http.Client{Transport: api, Timeout: 5 * time.Second}
	return &channelPermissionResource{client: session}
}

func permissionTestModel(targetID, targetType string, allow, deny int64) channelPermissionResourceModel {
	return channelPermissionResourceModel{
		ID:          types.StringValue(permissionTestChannelID + ":" + targetID),
		ChannelID:   types.StringValue(permissionTestChannelID),
		Type:        types.StringValue(targetType),
		OverwriteID: types.StringValue(targetID),
		Allow:       types.Int64Value(allow),
		Deny:        types.Int64Value(deny),
	}
}

func runPermissionTestOperation(ctx context.Context, r *channelPermissionResource, operation string, before, planned channelPermissionResourceModel) (tfsdk.State, diag.Diagnostics) {
	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
	plan := tfsdk.Plan{Schema: schemaResp.Schema}
	planned.ID = types.StringUnknown()
	if diags := plan.Set(ctx, &planned); diags.HasError() {
		return tfsdk.State{}, diags
	}
	state := tfsdk.State{
		Schema: schemaResp.Schema,
		Raw:    tftypes.NewValue(schemaResp.Schema.Type().TerraformType(ctx), nil),
	}
	if operation != "create" {
		if diags := state.Set(ctx, &before); diags.HasError() {
			return state, diags
		}
	}

	switch operation {
	case "create":
		resp := resource.CreateResponse{State: state}
		r.Create(ctx, resource.CreateRequest{Plan: plan}, &resp)
		return resp.State, resp.Diagnostics
	case "update":
		resp := resource.UpdateResponse{State: state}
		r.Update(ctx, resource.UpdateRequest{Plan: plan, State: state}, &resp)
		return resp.State, resp.Diagnostics
	case "delete":
		var resp resource.DeleteResponse
		r.Delete(ctx, resource.DeleteRequest{State: state}, &resp)
		return state, resp.Diagnostics
	case "read":
		resp := resource.ReadResponse{State: state}
		r.Read(ctx, resource.ReadRequest{State: state}, &resp)
		return resp.State, resp.Diagnostics
	default:
		panic("unknown test operation: " + operation)
	}
}

func TestChannelPermissionResource_IndividualWrites(t *testing.T) {
	tests := []struct {
		name       string
		operation  string
		targetType string
		allow      int64
		deny       int64
		omitDeny   bool
		onlyTarget bool
	}{
		{name: "create role", operation: "create", targetType: "role", allow: 3072, deny: 8192},
		{name: "create member", operation: "create", targetType: "member", allow: 3072, deny: 8192},
		{name: "update role", operation: "update", targetType: "role", allow: 3072, deny: 8192},
		{name: "update member", operation: "update", targetType: "member", allow: 3072, deny: 8192},
		{name: "delete role", operation: "delete", targetType: "role"},
		{name: "delete member", operation: "delete", targetType: "member"},
		{name: "clear allow and deny", operation: "update", targetType: "role"},
		{name: "preserve high permission bits", operation: "update", targetType: "role", allow: 1 << 55, deny: 1 << 54},
		{name: "omitted deny defaults to zero", operation: "create", targetType: "role", allow: 1024, omitDeny: true},
		{name: "delete last overwrite", operation: "delete", targetType: "role", onlyTarget: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := permissionTestModel("1452601985235816603", tt.targetType, tt.allow, tt.deny)
			before := model
			before.Allow = types.Int64Value(1)
			before.Deny = types.Int64Value(2)
			if tt.omitDeny {
				model.Deny = types.Int64Null()
			}
			targetType := discordgo.PermissionOverwriteTypeRole
			if tt.targetType == "member" {
				targetType = discordgo.PermissionOverwriteTypeMember
			}
			api := &permissionTestAPI{overwrites: map[string]discordgo.PermissionOverwrite{}}
			expected := map[string]discordgo.PermissionOverwrite{}
			if !tt.onlyTarget {
				for _, unrelated := range []discordgo.PermissionOverwrite{
					{ID: "1452601985235816602", Type: discordgo.PermissionOverwriteTypeRole, Allow: 1024, Deny: 2048},
					{ID: "1452601985235816606", Type: discordgo.PermissionOverwriteTypeMember, Allow: 4096, Deny: 8192},
				} {
					api.overwrites[unrelated.ID] = unrelated
					expected[unrelated.ID] = unrelated
				}
			}
			if tt.operation != "create" {
				api.overwrites[before.OverwriteID.ValueString()] = discordgo.PermissionOverwrite{ID: before.OverwriteID.ValueString(), Type: targetType, Allow: 1, Deny: 2}
			}
			r := newPermissionTestResource(t, api)
			state, diags := runPermissionTestOperation(t.Context(), r, tt.operation, before, model)
			require.False(t, diags.HasError(), "%v", diags)

			method := http.MethodPut
			if tt.operation == "delete" {
				method = http.MethodDelete
			} else {
				expected[model.OverwriteID.ValueString()] = discordgo.PermissionOverwrite{ID: model.OverwriteID.ValueString(), Type: targetType, Allow: tt.allow, Deny: tt.deny}
				var actual channelPermissionResourceModel
				require.False(t, state.Get(t.Context(), &actual).HasError())
				model.Deny = types.Int64Value(tt.deny)
				assert.Equal(t, model, actual)
			}
			assert.Equal(t, expected, api.overwrites)
			assert.Equal(t, []string{method + " /channels/" + permissionTestChannelID + "/permissions/" + model.OverwriteID.ValueString()}, api.requests)
		})
	}
}

func TestChannelPermissionResource_ConcurrentCreatesPreserveAllOverwrites(t *testing.T) {
	unrelated := discordgo.PermissionOverwrite{ID: "1452601985235816602", Allow: 1024, Deny: 2048}
	api := &permissionTestAPI{
		overwrites:  map[string]discordgo.PermissionOverwrite{unrelated.ID: unrelated},
		readBarrier: make(chan struct{}),
	}
	models := []channelPermissionResourceModel{
		permissionTestModel("1452601985235816603", "role", 3072, 0),
		permissionTestModel("1452601985235816604", "member", 1024, 2048),
	}
	results := make(chan diag.Diagnostics, len(models))
	ctx := t.Context()
	for _, model := range models {
		// Independent clients ensure client-side rate-limit locks cannot hide a
		// race between separate Terraform runs or another Discord API writer.
		r := newPermissionTestResource(t, api)
		go func() {
			_, diags := runPermissionTestOperation(ctx, r, "create", channelPermissionResourceModel{}, model)
			results <- diags
		}()
	}
	for range models {
		diags := <-results
		assert.False(t, diags.HasError(), "%v", diags)
	}
	assert.Equal(t, map[string]discordgo.PermissionOverwrite{
		unrelated.ID:          unrelated,
		"1452601985235816603": {ID: "1452601985235816603", Type: discordgo.PermissionOverwriteTypeRole, Allow: 3072},
		"1452601985235816604": {ID: "1452601985235816604", Type: discordgo.PermissionOverwriteTypeMember, Allow: 1024, Deny: 2048},
	}, api.overwrites)
}

func TestChannelPermissionResource_WriteErrorsPreserveState(t *testing.T) {
	for _, operation := range []string{"create", "update", "delete"} {
		for _, failure := range []struct {
			name   string
			status int
			err    error
		}{
			{name: "invalid request", status: http.StatusBadRequest},
			{name: "permission denied", status: http.StatusForbidden},
			{name: "missing channel", status: http.StatusNotFound},
			{name: "transport error", err: errors.New("test connection failure")},
		} {
			t.Run(operation+"/"+failure.name, func(t *testing.T) {
				existing := discordgo.PermissionOverwrite{ID: "1452601985235816603", Allow: 1, Deny: 2}
				api := &permissionTestAPI{
					status: failure.status, err: failure.err,
					overwrites: map[string]discordgo.PermissionOverwrite{existing.ID: existing},
				}
				r := newPermissionTestResource(t, api)
				before := permissionTestModel(existing.ID, "role", 1, 2)
				planned := permissionTestModel(existing.ID, "role", 1024, 0)
				state, diags := runPermissionTestOperation(t.Context(), r, operation, before, planned)
				assert.Equal(t, operation != "delete" || failure.status != http.StatusNotFound, diags.HasError(), "%v", diags)
				if operation == "create" {
					assert.True(t, state.Raw.IsNull(), "failed creates must not record success")
				} else {
					var actual channelPermissionResourceModel
					require.False(t, state.Get(t.Context(), &actual).HasError())
					assert.Equal(t, before, actual, "failed writes must not record planned permissions")
				}
				assert.Equal(t, map[string]discordgo.PermissionOverwrite{existing.ID: existing}, api.overwrites)
				method := http.MethodPut
				if operation == "delete" {
					method = http.MethodDelete
				}
				assert.Equal(t, []string{method + " /channels/" + permissionTestChannelID + "/permissions/" + existing.ID}, api.requests)
			})
		}
	}
	t.Run("delete already absent overwrite", func(t *testing.T) {
		api := &permissionTestAPI{overwrites: map[string]discordgo.PermissionOverwrite{}}
		r := newPermissionTestResource(t, api)
		model := permissionTestModel("1452601985235816603", "role", 0, 0)
		_, diags := runPermissionTestOperation(t.Context(), r, "delete", model, model)
		assert.False(t, diags.HasError(), "%v", diags)
		assert.Empty(t, api.overwrites)
		assert.Equal(t, []string{"DELETE /channels/" + permissionTestChannelID + "/permissions/" + model.OverwriteID.ValueString()}, api.requests)
	})
}

func TestChannelPermissionResource_CanceledWrites(t *testing.T) {
	for _, operation := range []string{"create", "update", "delete"} {
		t.Run(operation, func(t *testing.T) {
			api := &permissionTestAPI{overwrites: map[string]discordgo.PermissionOverwrite{}}
			r := newPermissionTestResource(t, api)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			model := permissionTestModel("1452601985235816603", "role", 1024, 0)
			_, diags := runPermissionTestOperation(ctx, r, operation, model, model)
			require.True(t, diags.HasError(), "%v", diags)
			assert.Contains(t, diags.Errors()[0].Detail(), context.Canceled.Error())
			assert.Empty(t, api.overwrites)
			assert.Empty(t, api.requests)
		})
	}
}

func TestChannelPermissionResource_ReadOverwrite(t *testing.T) {
	for _, tt := range []struct {
		name      string
		missing   bool
		wrongType bool
		status    int
	}{
		{name: "refresh changed permissions"},
		{name: "missing overwrite", missing: true},
		{name: "same ID with different target type", wrongType: true},
		{name: "missing channel", status: http.StatusNotFound},
		{name: "permission denied preserves state", status: http.StatusForbidden},
	} {
		t.Run(tt.name, func(t *testing.T) {
			model := permissionTestModel("1452601985235816603", "member", 1024, 0)
			unrelated := discordgo.PermissionOverwrite{ID: "1452601985235816602", Type: discordgo.PermissionOverwriteTypeMember, Allow: 1, Deny: 2}
			api := &permissionTestAPI{
				status:     tt.status,
				overwrites: map[string]discordgo.PermissionOverwrite{unrelated.ID: unrelated},
			}
			if !tt.missing {
				targetType := discordgo.PermissionOverwriteTypeMember
				if tt.wrongType {
					targetType = discordgo.PermissionOverwriteTypeRole
				}
				api.overwrites[model.OverwriteID.ValueString()] = discordgo.PermissionOverwrite{ID: model.OverwriteID.ValueString(), Type: targetType, Allow: 3072, Deny: 2048}
			}
			r := newPermissionTestResource(t, api)
			state, diags := runPermissionTestOperation(t.Context(), r, "read", model, model)
			require.Equal(t, tt.status == http.StatusForbidden, diags.HasError(), "%v", diags)
			if tt.missing || tt.wrongType || tt.status == http.StatusNotFound {
				assert.True(t, state.Raw.IsNull())
			} else {
				var actual channelPermissionResourceModel
				require.False(t, state.Get(t.Context(), &actual).HasError())
				if tt.status == 0 {
					model.Allow = types.Int64Value(3072)
					model.Deny = types.Int64Value(2048)
				}
				assert.Equal(t, model, actual)
			}
			assert.Equal(t, []string{"GET /channels/" + permissionTestChannelID}, api.requests)
			assert.Equal(t, unrelated, api.overwrites[unrelated.ID])
		})
	}
}
