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

package lookercommon_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/googleapis/mcp-toolbox/internal/resources"
	"github.com/googleapis/mcp-toolbox/internal/tools/looker/lookercommon"
	"github.com/looker-open-source/sdk-codegen/go/rtl"
	v4 "github.com/looker-open-source/sdk-codegen/go/sdk/v4"
)

func TestDiscoverMCPUIPublicOrigin(t *testing.T) {
	tcs := []struct {
		desc string
		html string
		want string
	}{
		{
			desc: "dashboard entry script",
			html: `<script type="text/javascript" nonce="abc" src="https://looker.example.com/webpack/en/mcp-render-dashboard.webpack.js?v=26.18.0"></script>`,
			want: "https://looker.example.com",
		},
		{
			desc: "visualization entry script with port and single quotes",
			html: `<script src='https://Looker.Example.com:8443/webpack/en/mcp-render-visualization.webpack.js'></script>`,
			want: "https://looker.example.com:8443",
		},
		{
			desc: "ignores unrelated scripts before the entry script",
			html: `<script src="https://cdn.example.com/other.js"></script>` +
				`<script src="https://looker.example.com/webpack/en/mcp-render-dashboard.webpack.js"></script>`,
			want: "https://looker.example.com",
		},
		{
			desc: "rejects http origins",
			html: `<script src="http://looker:19999/webpack/en/mcp-render-dashboard.webpack.js"></script>`,
			want: "",
		},
		{
			desc: "rejects relative src",
			html: `<script src="/webpack/en/mcp-render-dashboard.webpack.js"></script>`,
			want: "",
		},
		{
			desc: "no entry script",
			html: `<html><body>Looker Dashboard</body></html>`,
			want: "",
		},
	}
	for _, tc := range tcs {
		t.Run(tc.desc, func(t *testing.T) {
			if got := lookercommon.DiscoverMCPUIPublicOrigin(tc.html); got != tc.want {
				t.Errorf("DiscoverMCPUIPublicOrigin() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAddCSPOrigin(t *testing.T) {
	csp := &resources.CSPConfig{
		ResourceDomains: []string{"https://looker.example.com/"},
		ConnectDomains:  []string{"https://*.googleapis.com"},
	}

	lookercommon.AddCSPOrigin(csp, "https://looker.example.com")
	lookercommon.AddCSPOrigin(csp, "https://looker.example.com")
	lookercommon.AddCSPOrigin(csp, "http://localhost:9999/")
	lookercommon.AddCSPOrigin(csp, "")
	lookercommon.AddCSPOrigin(nil, "https://ignored.example.com")

	want := &resources.CSPConfig{
		ResourceDomains: []string{"https://looker.example.com/", "http://localhost:9999"},
		ConnectDomains: []string{
			"https://*.googleapis.com",
			"https://looker.example.com",
			"wss://looker.example.com",
			"http://localhost:9999",
			"ws://localhost:9999",
		},
	}
	if diff := cmp.Diff(want, csp); diff != "" {
		t.Errorf("AddCSPOrigin() mismatch (-want +got):\n%s", diff)
	}
}

func TestAppendLocaleQueryParam(t *testing.T) {
	if got := lookercommon.AppendLocaleQueryParam("http://localhost:9999/public/mcp/ui/render_visualization/assets", "ja_JP"); got != "http://localhost:9999/public/mcp/ui/render_visualization/assets?locale=ja_JP" {
		t.Errorf("AppendLocaleQueryParam() = %q", got)
	}
	if got := lookercommon.AppendLocaleQueryParam("http://localhost:9999/public/mcp/ui/render_visualization/assets?locale=es_ES", "ja_JP"); got != "http://localhost:9999/public/mcp/ui/render_visualization/assets?locale=es_ES" {
		t.Errorf("AppendLocaleQueryParam() should preserve existing locale, got %q", got)
	}
	if got := lookercommon.AppendLocaleQueryParam("http://localhost:9999/public/mcp/ui/render_visualization/assets", ""); got != "http://localhost:9999/public/mcp/ui/render_visualization/assets" {
		t.Errorf("AppendLocaleQueryParam() with empty locale = %q", got)
	}
}

func TestFetchMcpAppStateCachesColorCollectionsPerInstanceOnly(t *testing.T) {
	lookercommon.ClearMcpAppStateCacheForTesting()
	defer lookercommon.ClearMcpAppStateCacheForTesting()

	var userCalls int32
	var colorCalls int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/4.0/login":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "test-token",
				"token_type":   "Bearer",
				"expires_in":   3600,
			})
		case "/api/4.0/user":
			callNum := atomic.AddInt32(&userCalls, 1)
			locale := "en"
			if callNum == 2 {
				locale = "ja_JP"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     "1",
				"locale": locale,
				"abilities": map[string]any{
					"objects": map[string]any{
						"query": map[string]any{"can": map[string]any{"explore": true, "run": true}},
					},
				},
			})
		case "/api/4.0/color_collections/active":
			atomic.AddInt32(&colorCalls, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"default_collection_id": "b43731d5-dc87-4a8e-b807-635bef3948e7",
				"default_colors":        []string{"#FBBC04", "#EA4335"},
				"custom":                []any{},
				"standard":              []any{map[string]any{"id": "b43731d5-dc87-4a8e-b807-635bef3948e7", "label": "Boardwalk"}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	settings := rtl.ApiSettings{
		BaseUrl:      ts.URL,
		ApiVersion:   "4.0",
		ClientId:     "id",
		ClientSecret: "secret",
	}
	sdk := v4.NewLookerSDK(rtl.NewAuthSession(settings))

	// First call fetches both /user and /color_collections/active
	state1 := lookercommon.FetchMcpAppState(context.Background(), sdk, &settings)
	if state1 == nil {
		t.Fatalf("expected non-nil app_state on first call")
	}
	if got := atomic.LoadInt32(&userCalls); got != 1 {
		t.Errorf("userCalls after call 1 = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&colorCalls); got != 1 {
		t.Errorf("colorCalls after call 1 = %d, want 1", got)
	}
	if got := lookercommon.GetLastObservedLocale(&settings); got != "en" {
		t.Errorf("GetLastObservedLocale after call 1 = %q, want en", got)
	}

	// Second call fetches /user again (not cached) but reuses cached /color_collections/active
	state2 := lookercommon.FetchMcpAppState(context.Background(), sdk, &settings)
	if state2 == nil {
		t.Fatalf("expected non-nil app_state on second call")
	}
	if got := atomic.LoadInt32(&userCalls); got != 2 {
		t.Errorf("userCalls after call 2 = %d, want 2", got)
	}
	if got := atomic.LoadInt32(&colorCalls); got != 1 {
		t.Errorf("colorCalls after call 2 = %d, want 1 (should be cached)", got)
	}
	if got := lookercommon.GetLastObservedLocale(&settings); got != "ja_JP" {
		t.Errorf("GetLastObservedLocale after call 2 = %q, want ja_JP", got)
	}
}
