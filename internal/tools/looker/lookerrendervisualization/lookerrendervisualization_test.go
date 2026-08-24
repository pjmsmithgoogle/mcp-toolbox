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

func TestParseFromYamlLookerRenderVisualization(t *testing.T) {
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
			  resource_uri: ui://custom/vis.html
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
						UI: &tools.ToolUIMeta{
							ResourceURI: "ui://custom/vis.html",
							Visibility:  []string{"model", "app"},
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
			_, _, _, got, _, _, err := server.UnmarshalPrimitiveConfig(ctx, testutils.FormatYaml(tc.in))
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

	cfg := lkr.Config{
		ConfigBase: tools.ConfigBase{
			Name:        "render_visualization",
			Description: "Render visualization tool",
			UI: &tools.ToolUIMeta{
				RemoteURL: ts.URL,
			},
		},
		Type:   "looker-render-visualization",
		Source: "my-instance",
	}

	toolInstance, err := cfg.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	if toolInstance.GetUIMeta() == nil {
		t.Fatalf("expected UI meta to be populated, got nil")
	}
	if toolInstance.GetUIMeta().ResourceURI != "ui://looker/render_visualization.html" {
		t.Errorf("expected default resource URI ui://looker/render_visualization.html, got %s", toolInstance.GetUIMeta().ResourceURI)
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
	if resList[0].GetMIMEType() != "text/html;profile=mcp-app" {
		t.Errorf("expected MIMEType text/html;profile=mcp-app, got %s", resList[0].GetMIMEType())
	}

	content, err := resList[0].Read(ctx)
	if err != nil {
		t.Fatalf("error reading resource: %v", err)
	}
	if !strings.Contains(content.Text, "Looker Visualization") {
		t.Errorf("expected HTML content to contain Looker Visualization, got %s", content.Text)
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

	cfg := lkr.Config{
		ConfigBase: tools.ConfigBase{
			Name:        "render_visualization",
			Description: "Render visualization tool",
			UI: &tools.ToolUIMeta{
				RemoteURL: ts.URL,
			},
		},
		Type:   "looker-render-visualization",
		Source: "my-instance",
	}

	toolInstance, err := cfg.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	rp := toolInstance.(resources.ResourceProvider)
	content, err := rp.GetResources()[0].Read(ctx)
	if err != nil {
		t.Fatalf("failed to read resource: %v", err)
	}
	if !strings.Contains(content.Text, "Looker Visualization Scene") {
		t.Errorf("expected fetched content, got %s", content.Text)
	}

	// 2. Admin disabled (403 Forbidden)
	tsForbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer tsForbidden.Close()

	cfgForbidden := lkr.Config{
		ConfigBase: tools.ConfigBase{
			Name:        "render_visualization",
			Description: "Render visualization tool",
			UI: &tools.ToolUIMeta{
				RemoteURL: tsForbidden.URL,
			},
		},
		Type:   "looker-render-visualization",
		Source: "my-instance",
	}

	toolForbidden, err := cfgForbidden.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	rpForbidden := toolForbidden.(resources.ResourceProvider)
	_, err = rpForbidden.GetResources()[0].Read(ctx)
	if err == nil || !strings.Contains(err.Error(), "disabled by administrator") {
		t.Errorf("expected disabled by administrator error, got %v", err)
	}

	// 3. Error on 404
	tsNotFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer tsNotFound.Close()

	cfgNotFound := lkr.Config{
		ConfigBase: tools.ConfigBase{
			Name:        "render_visualization",
			Description: "Render visualization tool",
			UI: &tools.ToolUIMeta{
				RemoteURL: tsNotFound.URL,
			},
		},
		Type:   "looker-render-visualization",
		Source: "my-instance",
	}

	toolNotFound, err := cfgNotFound.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	rpNotFound := toolNotFound.(resources.ResourceProvider)
	_, err = rpNotFound.GetResources()[0].Read(ctx)
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("expected HTTP 404 error, got %v", err)
	}

	// 4. Error when no URL configured
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
	_, err = rpNoURL.GetResources()[0].Read(ctx)
	if err == nil || !strings.Contains(err.Error(), "no Looker instance URL configured") {
		t.Errorf("expected no Looker instance URL configured error, got %v", err)
	}
}
