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
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/googleapis/mcp-toolbox/internal/resources"
	"github.com/googleapis/mcp-toolbox/internal/tools/looker/lookercommon"
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
