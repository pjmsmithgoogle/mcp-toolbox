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

package lookerrunquery_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/googleapis/mcp-toolbox/internal/server"
	"github.com/googleapis/mcp-toolbox/internal/testutils"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	lkr "github.com/googleapis/mcp-toolbox/internal/tools/looker/lookerrunquery"
	v4 "github.com/looker-open-source/sdk-codegen/go/sdk/v4"
)

func TestParseFromYamlLookerRunQuery(t *testing.T) {
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
            type: looker-run-query
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
					Type:   "looker-run-query",
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
				t.Errorf("unexpected diff (-want +got):\n%s", diff)
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
		"customer.last_name": "Simpson",
		"customer.city":      "", // Empty string override clears default city filter
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

