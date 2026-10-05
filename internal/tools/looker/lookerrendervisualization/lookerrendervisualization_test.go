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
package lookerrendervisualization_test

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
	lkr "github.com/googleapis/mcp-toolbox/internal/tools/looker/lookerrendervisualization"
)

func TestParse(t *testing.T) {
	ctx := context.Background()
	tcs := []struct {
		desc string
		in   string
		want server.ToolConfigs
	}{
		{
			desc: "basic example",
			in: `
			kind: tool
			name: render_visualization
			type: looker-render-visualization
			source: my-instance
			description: Render Looker visualization
			`,
			want: server.ToolConfigs{
				"render_visualization": lkr.Config{
					ConfigBase: tools.ConfigBase{
						Name:         "render_visualization",
						Description:  "Render Looker visualization",
						AuthRequired: []string{},
					},
					Type:   "looker-render-visualization",
					Source: "my-instance",
				},
			},
		},
		{
			desc: "example with custom UI metadata",
			in: `
			kind: tool
			name: render_visualization
			type: looker-render-visualization
			source: my-instance
			description: Render Looker visualization
			ui:
			  resource: custom_vis
			  visibility:
			    - model
			    - app
			`,
			want: server.ToolConfigs{
				"render_visualization": lkr.Config{
					ConfigBase: tools.ConfigBase{
						Name:         "render_visualization",
						Description:  "Render Looker visualization",
						AuthRequired: []string{},
						UI: &tools.ToolUIMetadata{
							Resource:   "custom_vis",
							Visibility: []tools.ToolVisibility{tools.VisibilityModel, tools.VisibilityApp},
						},
					},
					Type:   "looker-render-visualization",
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
		w.Write([]byte("<html><body>Looker Visualization</body></html>"))
	}))
	defer ts.Close()

	t.Setenv("LOOKER_UI_URL", ts.URL)

	cfg := lkr.Config{
		ConfigBase: tools.ConfigBase{
			Name:        "render_visualization",
			Description: "Render visualization tool",
		},
		Type:   "looker-render-visualization",
		Source: "my-instance",
	}

	toolInstance, err := cfg.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	if toolInstance.GetToolUIMetadata() == nil {
		t.Fatalf("expected UI meta to be populated, got nil")
	}
	if toolInstance.GetToolUIMetadata().Resource != "looker_render_visualization_ui" {
		t.Errorf("expected default resource name looker_render_visualization_ui, got %s", toolInstance.GetToolUIMetadata().Resource)
	}

	rp, ok := toolInstance.(resources.ResourceProvider)
	if !ok {
		t.Fatalf("expected toolInstance to implement ResourceProvider")
	}

	resList := rp.GetResources()
	if len(resList) != 1 {
		t.Fatalf("expected 1 resource, got %d", len(resList))
	}
	if resList[0].GetURI() != "ui://looker/render_visualization.html" {
		t.Errorf("expected URI ui://looker/render_visualization.html, got %s", resList[0].GetURI())
	}
	if resList[0].GetMimeType() != "text/html;profile=mcp-app" {
		t.Errorf("expected MimeType text/html;profile=mcp-app, got %s", resList[0].GetMimeType())
	}

	content, err := resList[0].Read(ctx, nil)
	if err != nil {
		t.Fatalf("error reading resource: %v", err)
	}
	contentStr, ok := content.(string)
	if !ok || !strings.Contains(contentStr, "Looker Visualization") {
		t.Errorf("expected HTML content to contain Looker Visualization, got %v", content)
	}
}

func TestRemoteUIFetching(t *testing.T) {
	ctx := context.Background()

	// 1. Success with Remote Server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html><body>Looker Visualization Scene</body></html>"))
	}))
	defer ts.Close()

	t.Setenv("LOOKER_UI_URL", ts.URL)

	cfg := lkr.Config{
		ConfigBase: tools.ConfigBase{
			Name:        "render_visualization",
			Description: "Render visualization tool",
		},
		Type:   "looker-render-visualization",
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
	if !ok || !strings.Contains(contentStr, "Looker Visualization Scene") {
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
			Name:        "render_visualization",
			Description: "Render visualization tool",
		},
		Type:   "looker-render-visualization",
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
			Name:        "render_visualization",
			Description: "Render visualization tool",
		},
		Type:   "looker-render-visualization",
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
			Name:        "render_visualization",
			Description: "Render visualization tool",
		},
		Type:   "looker-render-visualization",
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
