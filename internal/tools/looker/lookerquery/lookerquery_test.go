// Copyright 2025 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package lookerquery_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/googleapis/mcp-toolbox/internal/server"
	"github.com/googleapis/mcp-toolbox/internal/sources"
	"github.com/googleapis/mcp-toolbox/internal/testutils"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	lkr "github.com/googleapis/mcp-toolbox/internal/tools/looker/lookerquery"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
	"github.com/looker-open-source/sdk-codegen/go/rtl"
	v4 "github.com/looker-open-source/sdk-codegen/go/sdk/v4"
)

type fakeLookerSource struct {
	settings *rtl.ApiSettings
	sdk      *v4.LookerSDK
}

func (f fakeLookerSource) SourceType() string                  { return "looker" }
func (f fakeLookerSource) ToConfig() sources.SourceConfig      { return nil }
func (f fakeLookerSource) IsReadOnly() bool                    { return false }
func (f fakeLookerSource) UseClientAuthorization() bool        { return false }
func (f fakeLookerSource) GetAuthTokenHeaderName() string      { return "Authorization" }
func (f fakeLookerSource) LookerApiSettings() *rtl.ApiSettings { return f.settings }
func (f fakeLookerSource) GetLookerSDK(context.Context, string) (*v4.LookerSDK, error) {
	if f.sdk != nil {
		return f.sdk, nil
	}
	return &v4.LookerSDK{}, nil
}

func TestParseFromYamlLookerQuery(t *testing.T) {
	ctx, err := testutils.ContextWithNewLogger()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	tcs := []struct {
		desc string
		in   string
		want server.ToolConfigs
	}{
		{
			desc: "basic example",
			in: `
			kind: tool
			name: example_tool
			type: looker-query
			source: my-instance
			description: some description
				`,
			want: server.ToolConfigs{
				"example_tool": lkr.Config{
					ConfigBase: tools.ConfigBase{
						Name:         "example_tool",
						Description:  "some description",
						AuthRequired: []string{},
					},
					Type:   "looker-query",
					Source: "my-instance",
				},
			},
		},
	}
	for _, tc := range tcs {
		t.Run(tc.desc, func(t *testing.T) {
			_, _, _, got, _, _, _, _, err := server.UnmarshalPrimitiveConfig(ctx, testutils.FormatYaml(tc.in))
			if err != nil {
				t.Fatalf("unable to unmarshal: %s", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("incorrect parse: diff %v", diff)
			}
		})
	}

}

func TestFailParseFromYamlLookerQuery(t *testing.T) {
	ctx, err := testutils.ContextWithNewLogger()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	tcs := []struct {
		desc string
		in   string
		err  string
	}{
		{
			desc: "Invalid method",
			in: `
			kind: tool
			name: example_tool
			type: looker-query
			source: my-instance
			method: GOT
			description: some description
			`,
			err: "error unmarshaling tool: unable to parse tool \"example_tool\" as type \"looker-query\": [3:1] unknown field \"method\"\n   1 | authRequired: []\n   2 | description: some description\n>  3 | method: GOT\n       ^\n   4 | name: example_tool\n   5 | source: my-instance\n   6 | type: looker-query",
		},
	}
	for _, tc := range tcs {
		t.Run(tc.desc, func(t *testing.T) {
			_, _, _, _, _, _, _, _, err := server.UnmarshalPrimitiveConfig(ctx, testutils.FormatYaml(tc.in))
			if err == nil {
				t.Fatalf("expect parsing to fail")
			}
			errStr := err.Error()
			if !strings.Contains(errStr, tc.err) {
				t.Fatalf("unexpected error string: got %q, want substring %q", errStr, tc.err)
			}
		})
	}

}

func TestBuildWriteQueryWithOverrides(t *testing.T) {
	baseFilters := map[string]any{
		"orders.status": "completed",
		"customer.city": "Springfield",
	}
	baseSorts := []string{"orders.count desc"}
	filterConfig := map[string]any{"some": "config"}
	clientId := "abc1234567890123456789"
	limit := "100"

	baseQuery := v4.Query{
		Model:        "cypress_mysql",
		View:         "customer",
		Filters:      &baseFilters,
		Sorts:        &baseSorts,
		FilterConfig: &filterConfig,
		ClientId:     &clientId,
		Limit:        &limit,
	}

	filterOverrides := map[string]any{
		"customer_last_name": "Simpson",
		"customer_city":      "", // Underscored key override clears default city filter
	}
	sortOverrides := []string{"customer.last_name asc"}

	wq := lkr.BuildWriteQueryWithOverrides(baseQuery, filterOverrides, sortOverrides)

	if wq.FilterConfig != nil {
		t.Errorf("expected FilterConfig to be nil so it does not override Filters, got %v", wq.FilterConfig)
	}
	if wq.ClientId != nil {
		t.Errorf("expected ClientId to be nil, got %v", wq.ClientId)
	}
	if wq.Filters == nil {
		t.Fatalf("expected Filters to be non-nil")
	}
	wantFilters := map[string]any{
		"orders.status":      "completed",
		"customer.city":      "",
		"customer.last_name": "Simpson",
	}
	if diff := cmp.Diff(wantFilters, *wq.Filters); diff != "" {
		t.Errorf("unexpected filters diff (-want +got):\n%s", diff)
	}
	if wq.Sorts == nil || len(*wq.Sorts) != 1 || (*wq.Sorts)[0] != "customer.last_name asc" {
		t.Errorf("expected Sorts override [\"customer.last_name asc\"], got %v", wq.Sorts)
	}
}

func TestInvokeValidationErrors(t *testing.T) {
	ctx, err := testutils.ContextWithNewLogger()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	cfg := lkr.Config{
		ConfigBase: tools.ConfigBase{
			Name:        "query",
			Description: "Execute an inline or saved query",
		},
		Type:   "looker-query",
		Source: "looker-source",
	}
	tool, err := cfg.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	t.Run("invalid vis_config JSON", func(t *testing.T) {
		params := parameters.ParamValues{
			{Name: "query_id", Value: "123"},
			{Name: "vis_config", Value: "{not-valid-json"},
		}
		_, tbErr := tool.Invoke(ctx, fakeLookerSource{}, params, "")
		if tbErr == nil {
			t.Fatalf("expected error for invalid vis_config JSON, got nil")
		}
		if !strings.Contains(tbErr.Error(), "invalid vis_config JSON") {
			t.Errorf("expected error to contain %q, got %q", "invalid vis_config JSON", tbErr.Error())
		}
	})

	t.Run("invalid sorts slice elements for saved query", func(t *testing.T) {
		params := parameters.ParamValues{
			{Name: "query_id", Value: "123"},
			{Name: "sorts", Value: []any{12345}},
		}
		_, tbErr := tool.Invoke(ctx, fakeLookerSource{}, params, "")
		if tbErr == nil {
			t.Fatalf("expected error for non-string sorts element, got nil")
		}
		if !strings.Contains(tbErr.Error(), "can't convert sorts to array of strings") {
			t.Errorf("expected error to contain %q, got %q", "can't convert sorts to array of strings", tbErr.Error())
		}
	})
}

func TestInvokeInlineQueryWithVisConfigAndErrorExtraction(t *testing.T) {
	ctx, err := testutils.ContextWithNewLogger()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	failMode := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/api/4.0/queries/run/json" {
			if failMode {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`{"message":"Validation Failed","errors":[{"field":"fields","message":"Unknown field orders.bad_field"}]}`))
				return
			}
			_, _ = w.Write([]byte(`[{"orders.count":42}]`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	settings := &rtl.ApiSettings{BaseUrl: ts.URL}
	session := rtl.NewAuthSession(*settings)
	session.Client = *ts.Client()
	sdk := v4.NewLookerSDK(session)

	cfg := lkr.Config{
		ConfigBase: tools.ConfigBase{
			Name:        "query",
			Description: "Execute an inline query",
		},
		Type:   "looker-query",
		Source: "looker-source",
	}
	tool, err := cfg.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	params := parameters.ParamValues{
		{Name: "model", Value: "thelook"},
		{Name: "explore", Value: "orders"},
		{Name: "fields", Value: []any{"orders.count"}},
		{Name: "vis_config", Value: `{"type":"single_value"}`},
	}
	res, tbErr := tool.Invoke(ctx, fakeLookerSource{settings: settings, sdk: sdk}, params, "")
	if tbErr != nil {
		t.Fatalf("Invoke failed: %v", tbErr)
	}
	structured, ok := res.(tools.StructuredToolResult)
	if !ok {
		t.Fatalf("expected StructuredToolResult when vis_config is provided on inline json query, got %T", res)
	}
	visCfg, ok := structured.StructuredContent["vis_config"].(map[string]any)
	if !ok || visCfg["type"] != "single_value" {
		t.Errorf("expected vis_config.type=single_value, got %v", structured.StructuredContent["vis_config"])
	}
	contentMap, ok := structured.Content.(map[string]any)
	if !ok || contentMap["data"] == nil {
		t.Errorf("expected structured.Content to include data, got %v", structured.Content)
	}

	failMode = true
	_, tbErr = tool.Invoke(ctx, fakeLookerSource{settings: settings, sdk: sdk}, params, "")
	if tbErr == nil || tbErr.Error() != "error processing request: Validation Failed" {
		t.Errorf("expected extracted Looker error message on inline query error, got %v", tbErr)
	}
}


