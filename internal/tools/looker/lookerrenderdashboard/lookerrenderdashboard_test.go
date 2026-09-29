// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package lookerrenderdashboard_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/googleapis/mcp-toolbox/internal/resources"
	"github.com/googleapis/mcp-toolbox/internal/server"
	"github.com/googleapis/mcp-toolbox/internal/testutils"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	lkr "github.com/googleapis/mcp-toolbox/internal/tools/looker/lookerrenderdashboard"
	v4 "github.com/looker-open-source/sdk-codegen/go/sdk/v4"
)

func TestParseFromYamlLookerRenderDashboard(t *testing.T) {
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
			name: render_dashboard
			type: looker-render-dashboard
			source: my-instance
			description: Render Looker dashboard
			`,
			want: server.ToolConfigs{
				"render_dashboard": lkr.Config{
					ConfigBase: tools.ConfigBase{
						Name:         "render_dashboard",
						Description:  "Render Looker dashboard",
						AuthRequired: []string{},
					},
					Type:   "looker-render-dashboard",
					Source: "my-instance",
				},
			},
		},
		{
			desc: "example with custom UI metadata",
			in: `
			kind: tool
			name: render_dashboard
			type: looker-render-dashboard
			source: my-instance
			description: Render Looker dashboard
			ui:
			  resource: custom_dash
			  visibility:
			    - model
			    - app
			`,
			want: server.ToolConfigs{
				"render_dashboard": lkr.Config{
					ConfigBase: tools.ConfigBase{
						Name:         "render_dashboard",
						Description:  "Render Looker dashboard",
						AuthRequired: []string{},
						UI: &tools.ToolUIMetadata{
							Resource:   "custom_dash",
							Visibility: []tools.ToolVisibility{tools.VisibilityModel, tools.VisibilityApp},
						},
					},
					Type:   "looker-render-dashboard",
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

func TestInitializeAndResourceProvider(t *testing.T) {
	ctx := context.Background()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("<html><body>Looker Dashboard</body></html>"))
	}))
	defer ts.Close()

	t.Setenv("LOOKER_UI_URL", ts.URL)

	cfg := lkr.Config{
		ConfigBase: tools.ConfigBase{
			Name:        "render_dashboard",
			Description: "Render dashboard tool",
		},
		Type:   "looker-render-dashboard",
		Source: "my-instance",
	}

	toolInstance, err := cfg.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	if toolInstance.GetToolUIMetadata() == nil {
		t.Fatalf("expected UI meta to be populated, got nil")
	}
	if toolInstance.GetToolUIMetadata().Resource != "looker_render_dashboard_ui" {
		t.Errorf("expected default resource name looker_render_dashboard_ui, got %s", toolInstance.GetToolUIMetadata().Resource)
	}

	rp, ok := toolInstance.(resources.ResourceProvider)
	if !ok {
		t.Fatalf("expected toolInstance to implement ResourceProvider")
	}

	resList := rp.GetResources()
	if len(resList) != 1 {
		t.Fatalf("expected 1 resource, got %d", len(resList))
	}
	if resList[0].GetURI() != "ui://looker/render_dashboard.html" {
		t.Errorf("expected URI ui://looker/render_dashboard.html, got %s", resList[0].GetURI())
	}
	if resList[0].GetMimeType() != "text/html;profile=mcp-app" {
		t.Errorf("expected MimeType text/html;profile=mcp-app, got %s", resList[0].GetMimeType())
	}

	content, err := resList[0].Read(ctx, nil)
	if err != nil {
		t.Fatalf("error reading resource: %v", err)
	}
	contentStr, ok := content.(string)
	if !ok || !strings.Contains(contentStr, "Looker Dashboard") {
		t.Errorf("expected HTML content to contain Looker Dashboard, got %v", content)
	}
}

func TestRemoteUIPublicOriginDiscovery(t *testing.T) {
	ctx := context.Background()

	// Simulates the managed MCP server: toolbox fetches the UI over an internal
	// address, but Looker declares its public Host URL in the entry script src.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<html><head>` +
			`<style>@font-face { src: url('/fonts/roboto.woff2'); }</style>` +
			`<script nonce="n" src="https://looker.example.com/webpack/en/mcp-render-dashboard.webpack.js?v=1"></script>` +
			`</head><body></body></html>`))
	}))
	defer ts.Close()

	t.Setenv("LOOKER_UI_URL", ts.URL)
	t.Setenv("LOOKER_BASE_URL", "")

	cfg := lkr.Config{
		ConfigBase: tools.ConfigBase{Name: "render_dashboard", Description: "Render dashboard tool"},
		Type:       "looker-render-dashboard",
		Source:     "my-instance",
	}
	toolInstance, err := cfg.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	res, ok := toolInstance.(resources.ResourceProvider).GetResources()[0].(*resources.DynamicUIResource)
	if !ok {
		t.Fatalf("expected *resources.DynamicUIResource")
	}

	content, err := res.Read(ctx, nil)
	if err != nil {
		t.Fatalf("failed to read resource: %v", err)
	}
	html := content.(string)
	if !strings.Contains(html, `url('https://looker.example.com/fonts/roboto.woff2')`) {
		t.Errorf("expected relative font URL to be rewritten to the public origin, got %s", html)
	}
	if strings.Contains(html, "nonce=") {
		t.Errorf("expected nonce attributes to be stripped, got %s", html)
	}

	csp := res.CSP
	for _, want := range []string{
		"https://looker.example.com",
		"https://maps.googleapis.com",
		"https://maps.gstatic.com",
		"https://*.gstatic.com",
		"https://*.googleusercontent.com",
		"https://www.google.com",
	} {
		if !contains(csp.ResourceDomains, want) {
			t.Errorf("expected ResourceDomains to contain %q, got %v", want, csp.ResourceDomains)
		}
	}
	for _, want := range []string{
		"https://looker.example.com",
		"wss://looker.example.com",
		"https://maps.googleapis.com",
		"https://maps.gstatic.com",
		"https://*.gstatic.com",
		"https://*.googleusercontent.com",
	} {
		if !contains(csp.ConnectDomains, want) {
			t.Errorf("expected ConnectDomains to contain %q, got %v", want, csp.ConnectDomains)
		}
	}
}

func contains(list []string, value string) bool {
	for _, v := range list {
		if v == value {
			return true
		}
	}
	return false
}

func TestRemoteUIFetching(t *testing.T) {
	ctx := context.Background()

	// 1. Success with Remote Server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html><body>Looker Dashboard Scene</body></html>"))
	}))
	defer ts.Close()

	t.Setenv("LOOKER_UI_URL", ts.URL)

	cfg := lkr.Config{
		ConfigBase: tools.ConfigBase{
			Name:        "render_dashboard",
			Description: "Render dashboard tool",
		},
		Type:   "looker-render-dashboard",
		Source: "my-instance",
	}

	toolInstance, err := cfg.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	rp := toolInstance.(resources.ResourceProvider)
	content, err := rp.GetResources()[0].Read(ctx, nil)
	if err != nil {
		t.Fatalf("failed to read resource: %v", err)
	}
	contentStr, ok := content.(string)
	if !ok || !strings.Contains(contentStr, "Looker Dashboard Scene") {
		t.Errorf("expected fetched content, got %v", content)
	}

	// 2. Admin disabled (403 Forbidden)
	tsForbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer tsForbidden.Close()

	t.Setenv("LOOKER_UI_URL", tsForbidden.URL)

	cfgForbidden := lkr.Config{
		ConfigBase: tools.ConfigBase{
			Name:        "render_dashboard",
			Description: "Render dashboard tool",
		},
		Type:   "looker-render-dashboard",
		Source: "my-instance",
	}

	toolForbidden, err := cfgForbidden.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	rpForbidden := toolForbidden.(resources.ResourceProvider)
	_, err = rpForbidden.GetResources()[0].Read(ctx, nil)
	if err == nil || !strings.Contains(err.Error(), "disabled by administrator") {
		t.Errorf("expected disabled by administrator error, got %v", err)
	}

	// 3. Error on 404
	tsNotFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer tsNotFound.Close()

	t.Setenv("LOOKER_UI_URL", tsNotFound.URL)

	cfgNotFound := lkr.Config{
		ConfigBase: tools.ConfigBase{
			Name:        "render_dashboard",
			Description: "Render dashboard tool",
		},
		Type:   "looker-render-dashboard",
		Source: "my-instance",
	}

	toolNotFound, err := cfgNotFound.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	rpNotFound := toolNotFound.(resources.ResourceProvider)
	_, err = rpNotFound.GetResources()[0].Read(ctx, nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("expected HTTP 404 error, got %v", err)
	}

	// 4. Error when no URL configured
	t.Setenv("LOOKER_UI_URL", "")
	t.Setenv("LOOKER_BASE_URL", "")

	cfgNoURL := lkr.Config{
		ConfigBase: tools.ConfigBase{
			Name:        "render_dashboard",
			Description: "Render dashboard tool",
		},
		Type:   "looker-render-dashboard",
		Source: "my-instance",
	}
	toolNoURL, err := cfgNoURL.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	rpNoURL := toolNoURL.(resources.ResourceProvider)
	_, err = rpNoURL.GetResources()[0].Read(ctx, nil)
	if err == nil || !strings.Contains(err.Error(), "no Looker instance URL configured") {
		t.Errorf("expected no Looker instance URL configured error, got %v", err)
	}
}

func TestExtractAndInjectComponentGroupIDs(t *testing.T) {
	rawDashboard := map[string]any{
		"id":    "30",
		"title": "Dashboard with Tile Group",
		"dashboard_layouts": []any{
			map[string]any{
				"id": "1",
				"dashboard_layout_components": []any{
					map[string]any{
						"id":                   "100",
						"dashboard_element_id": "50",
						"row":                  float64(0),
						"column":               float64(0),
						"width":                float64(24),
						"height":               float64(9),
					},
					map[string]any{
						"id":                   "101",
						"dashboard_element_id": "51",
						"group_id":             "100",
						"row":                  float64(0),
						"column":               float64(0),
						"width":                float64(12),
						"height":               float64(8),
					},
				},
			},
		},
	}

	groupIDs := lkr.ExtractComponentGroupIDs(rawDashboard)
	if len(groupIDs) != 1 || groupIDs["101"] != "100" {
		t.Fatalf("expected groupIDs[\"101\"] == \"100\", got %v", groupIDs)
	}

	comp100ID := "100"
	comp101ID := "101"
	el50ID := "50"
	el51ID := "51"
	layoutID := "1"
	dashID := "30"
	sdkDash := v4.Dashboard{
		Id: &dashID,
		DashboardLayouts: &[]v4.DashboardLayout{
			{
				Id: &layoutID,
				DashboardLayoutComponents: &[]v4.DashboardLayoutComponent{
					{Id: &comp100ID, DashboardElementId: &el50ID},
					{Id: &comp101ID, DashboardElementId: &el51ID},
				},
			},
		},
	}

	injected := lkr.InjectComponentGroupIDs(sdkDash, groupIDs)
	dashMap, ok := injected.(map[string]any)
	if !ok {
		t.Fatalf("expected injected payload to be map[string]any, got %T", injected)
	}
	layouts := dashMap["dashboard_layouts"].([]any)
	comps := layouts[0].(map[string]any)["dashboard_layout_components"].([]any)
	comp0 := comps[0].(map[string]any)
	comp1 := comps[1].(map[string]any)
	if _, hasGroup := comp0["group_id"]; hasGroup {
		t.Errorf("expected comp 100 not to have group_id, got %v", comp0["group_id"])
	}
	if comp1["group_id"] != "100" {
		t.Errorf("expected comp 101 to have group_id \"100\", got %v", comp1["group_id"])
	}
}

