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

package lookercommon

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/googleapis/mcp-toolbox/internal/resources"
)

// mcpUIEntryScriptRe matches the <script src="..."> tag that loads a Looker
// MCP Apps entry bundle (mcp-render-dashboard / mcp-render-visualization) and
// captures the https origin of that src.
var mcpUIEntryScriptRe = regexp.MustCompile(
	`(?i)<script\b[^>]*\bsrc\s*=\s*["'](https://[^/"'\s?#]+)/[^"']*mcp-render-(?:dashboard|visualization)[^"']*["']`,
)

// DiscoverMCPUIPublicOrigin returns the public origin that Looker declared in
// the MCP Apps UI HTML it served, or "" if none was found.
//
// Toolbox may fetch the UI over an address the end user's browser cannot
// reach. For example, the Looker-managed MCP server fetches over
// http://looker:19999 inside the cluster. Looker builds the entry script URL
// from its Host URL setting, so the origin of that script is the origin the
// browser loads assets from, and therefore the one the CSP must allow.
//
// Only https origins are returned, so a page can't downgrade the CSP to
// plain http. Local development over http keeps using the existing
// LOOKER_BASE_URL / LOOKER_UI_URL handling.
func DiscoverMCPUIPublicOrigin(html string) string {
	m := mcpUIEntryScriptRe.FindStringSubmatch(html)
	if len(m) < 2 {
		return ""
	}
	parsed, err := url.Parse(m[1])
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return ""
	}
	return fmt.Sprintf("%s://%s", parsed.Scheme, strings.ToLower(parsed.Host))
}

// AddCSPOrigin allows origin in the CSP's resource and connect domains and adds
// the matching WebSocket origin (ws:// for http, wss:// for https) to the
// connect domains. Duplicate entries are skipped. The caller must synchronize
// access to csp.
func AddCSPOrigin(csp *resources.CSPConfig, origin string) {
	origin = strings.TrimSuffix(strings.TrimSpace(origin), "/")
	if csp == nil || origin == "" {
		return
	}
	appendUnique(&csp.ResourceDomains, origin)
	appendUnique(&csp.ConnectDomains, origin)
	switch {
	case strings.HasPrefix(origin, "https://"):
		appendUnique(&csp.ConnectDomains, "wss://"+strings.TrimPrefix(origin, "https://"))
	case strings.HasPrefix(origin, "http://"):
		appendUnique(&csp.ConnectDomains, "ws://"+strings.TrimPrefix(origin, "http://"))
	}
}

func appendUnique(list *[]string, value string) {
	for _, existing := range *list {
		if strings.TrimSuffix(existing, "/") == value {
			return
		}
	}
	*list = append(*list, value)
}
