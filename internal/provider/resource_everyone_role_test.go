package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEveryoneRoleResource_Metadata(t *testing.T) {
	r := NewEveryoneRoleResource()
	req := resource.MetadataRequest{
		ProviderTypeName: "discord",
	}
	resp := &resource.MetadataResponse{}

	r.Metadata(t.Context(), req, resp)

	assert.Equal(t, "discord_everyone_role", resp.TypeName)
}

func TestEveryoneRoleResource_Schema(t *testing.T) {
	r := NewEveryoneRoleResource()
	req := resource.SchemaRequest{}
	resp := &resource.SchemaResponse{}

	r.Schema(t.Context(), req, resp)

	assert.NotNil(t, resp.Schema)
	assert.Contains(t, resp.Schema.Description, "@everyone role")

	// Check required attribute
	guildIDAttr, ok := resp.Schema.Attributes["guild_id"]
	assert.True(t, ok)
	assert.True(t, guildIDAttr.IsRequired())

	// Check optional attributes
	colorAttr, ok := resp.Schema.Attributes["color"]
	assert.True(t, ok)
	assert.True(t, colorAttr.IsOptional())

	// Check computed attributes
	idAttr, ok := resp.Schema.Attributes["id"]
	assert.True(t, ok)
	assert.True(t, idAttr.IsComputed())
}

func TestEveryoneRoleResource_Configure(t *testing.T) {
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
			r := &everyoneRoleResource{}
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

func importEveryoneRoleForTest(t *testing.T, r resource.Resource, id string) (tfsdk.State, diag.Diagnostics) {
	t.Helper()
	importer, ok := r.(resource.ResourceWithImportState)
	require.True(t, ok, "@everyone must support importing")
	var schemaResp resource.SchemaResponse
	r.Schema(t.Context(), resource.SchemaRequest{}, &schemaResp)
	resp := resource.ImportStateResponse{State: tfsdk.State{
		Schema: schemaResp.Schema,
		Raw:    tftypes.NewValue(schemaResp.Schema.Type().TerraformType(t.Context()), nil),
	}}
	importer.ImportState(t.Context(), resource.ImportStateRequest{ID: id}, &resp)
	return resp.State, resp.Diagnostics
}

func TestEveryoneRoleResource_ImportState(t *testing.T) {
	for _, id := range []string{"1", "12345678901234567", "1452601985235816601", "18446744073709551615"} {
		t.Run(id, func(t *testing.T) {
			// Import only seeds identity. It must work without a configured client;
			// Terraform's subsequent Read obtains the live attributes.
			state, diags := importEveryoneRoleForTest(t, NewEveryoneRoleResource(), id)
			require.False(t, diags.HasError(), "%v", diags)
			var actual everyoneRoleResourceModel
			require.False(t, state.Get(t.Context(), &actual).HasError())
			assert.Equal(t, everyoneRoleResourceModel{
				ID: types.StringValue(id), GuildID: types.StringValue(id),
				Color: types.Int64Null(), Hoist: types.BoolNull(), Mentionable: types.BoolNull(),
				Permissions: types.Int64Null(), Position: types.Int64Null(), Managed: types.BoolNull(),
			}, actual)
		})
	}

	for _, id := range []string{"", "0", "001", "+1", "-1", " 1", "1 ", "guild-id", "1:2", "18446744073709551616"} {
		t.Run("invalid/"+id, func(t *testing.T) {
			state, diags := importEveryoneRoleForTest(t, NewEveryoneRoleResource(), id)
			require.True(t, diags.HasError(), "%v", diags)
			assert.Equal(t, "Invalid Import ID", diags.Errors()[0].Summary())
			assert.True(t, state.Raw.IsNull(), "invalid IDs must not seed partial state")
		})
	}
}

type everyoneRoleTestTransport struct {
	guildID  string
	roles    []*discordgo.Role
	status   int
	err      error
	requests []string
}

func (a *everyoneRoleTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	a.requests = append(a.requests, req.Method+" "+req.URL.Path)
	if req.Method != http.MethodGet || req.URL.Path != "/api/v9/guilds/"+a.guildID+"/roles" {
		return nil, fmt.Errorf("unexpected request during import: %s %s", req.Method, req.URL.Path)
	}
	if a.err != nil {
		return nil, a.err
	}
	status := a.status
	if status == 0 {
		status = http.StatusOK
	}
	body, err := json.Marshal(a.roles)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusForbidden:
		body = []byte(`{"code":50013,"message":"Missing Permissions"}`)
	case http.StatusNotFound:
		body = []byte(`{"code":10004,"message":"Unknown Guild"}`)
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(body))),
		Request:    req,
	}, nil
}

func TestEveryoneRoleResource_ImportAndRead(t *testing.T) {
	const guildID = "1452601985235816601"
	live := &discordgo.Role{
		ID: guildID, Name: "@everyone", Color: 0x123456,
		Mentionable: true, Permissions: (1 << 49) | 1024,
	}
	lookalike := &discordgo.Role{ID: "1452601985235816602", Name: "@everyone", Permissions: 8}
	for _, tt := range []struct {
		name        string
		roles       []*discordgo.Role
		status      int
		err         error
		wantError   string
		wantRemoved bool
	}{
		{name: "preserve all live attributes", roles: []*discordgo.Role{live}},
		{name: "identify by guild ID not role name", roles: []*discordgo.Role{lookalike, live}},
		{name: "no roles", wantError: "@everyone Role Not Found"},
		{name: "name alone is not a match", roles: []*discordgo.Role{lookalike}, wantError: "@everyone Role Not Found"},
		{name: "permission denied preserves identity", status: http.StatusForbidden, wantError: "Error Fetching Roles"},
		{name: "transport error preserves identity", err: errors.New("test connection failure"), wantError: "Error Fetching Roles"},
		{name: "missing guild", status: http.StatusNotFound, wantRemoved: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			api := &everyoneRoleTestTransport{guildID: guildID, roles: tt.roles, status: tt.status, err: tt.err}
			session, err := discordgo.New("Bot test-token")
			require.NoError(t, err)
			session.Client = &http.Client{Transport: api}
			r := &everyoneRoleResource{client: session}
			state, diags := importEveryoneRoleForTest(t, r, guildID)
			require.False(t, diags.HasError(), "%v", diags)
			assert.Empty(t, api.requests, "ImportState must not make API calls")

			resp := resource.ReadResponse{State: state}
			r.Read(t.Context(), resource.ReadRequest{State: state}, &resp)
			require.Equal(t, tt.wantError != "", resp.Diagnostics.HasError(), "%v", resp.Diagnostics)
			if tt.wantError != "" {
				assert.Equal(t, tt.wantError, resp.Diagnostics.Errors()[0].Summary())
				assert.Equal(t, state.Raw, resp.State.Raw, "failed reads must not adopt another role or discard identity")
			} else if tt.wantRemoved {
				assert.True(t, resp.State.Raw.IsNull())
			} else {
				var actual everyoneRoleResourceModel
				require.False(t, resp.State.Get(t.Context(), &actual).HasError())
				assert.Equal(t, everyoneRoleResourceModel{
					ID: types.StringValue(guildID), GuildID: types.StringValue(guildID),
					Color: types.Int64Value(int64(live.Color)), Hoist: types.BoolValue(live.Hoist),
					Mentionable: types.BoolValue(live.Mentionable), Permissions: types.Int64Value(live.Permissions),
					Position: types.Int64Value(int64(live.Position)), Managed: types.BoolValue(live.Managed),
				}, actual)
			}
			assert.Equal(t, []string{"GET /api/v9/guilds/" + guildID + "/roles"}, api.requests,
				"import refresh must not create, update, or delete roles")
		})
	}
}

func TestEveryoneRoleResource_ImportReadCancellation(t *testing.T) {
	const guildID = "1452601985235816601"
	api := &everyoneRoleTestTransport{guildID: guildID}
	session, err := discordgo.New("Bot test-token")
	require.NoError(t, err)
	session.Client = &http.Client{Transport: api}
	r := &everyoneRoleResource{client: session}
	state, diags := importEveryoneRoleForTest(t, r, guildID)
	require.False(t, diags.HasError(), "%v", diags)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	resp := resource.ReadResponse{State: state}
	r.Read(ctx, resource.ReadRequest{State: state}, &resp)
	require.True(t, resp.Diagnostics.HasError(), "%v", resp.Diagnostics)
	assert.Contains(t, resp.Diagnostics.Errors()[0].Detail(), context.Canceled.Error())
	assert.Equal(t, state.Raw, resp.State.Raw)
	assert.Empty(t, api.requests)
}
