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
	"context"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/googleapis/mcp-toolbox/internal/resources"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"github.com/looker-open-source/sdk-codegen/go/rtl"
	v4 "github.com/looker-open-source/sdk-codegen/go/sdk/v4"
)

const (
	colorCollectionsCacheTTL = 5 * time.Minute
	mcpUserFields            = "id,email,display_name,locale,abilities,can"
)

type cachedColorCollections struct {
	data      map[string]any
	fetchedAt time.Time
}

var (
	mcpAppStateCacheMu    sync.RWMutex
	colorCollectionsCache = make(map[string]cachedColorCollections)
	instanceLocaleCache   = make(map[string]string)
)

func resolveInstanceCacheKey(settings *rtl.ApiSettings) string {
	if settings != nil && strings.TrimSpace(settings.BaseUrl) != "" {
		return strings.TrimSuffix(strings.TrimSpace(settings.BaseUrl), "/")
	}
	if envURL := strings.TrimSpace(os.Getenv("LOOKER_BASE_URL")); envURL != "" {
		return strings.TrimSuffix(envURL, "/")
	}
	return "default"
}

// ClearMcpAppStateCacheForTesting clears the in-memory color collections and
// locale caches. Intended for unit tests.
func ClearMcpAppStateCacheForTesting() {
	mcpAppStateCacheMu.Lock()
	defer mcpAppStateCacheMu.Unlock()
	colorCollectionsCache = make(map[string]cachedColorCollections)
	instanceLocaleCache = make(map[string]string)
}

// GetLastObservedLocale returns the most recently observed user locale for the
// given Looker instance settings, or "" if none has been recorded.
func GetLastObservedLocale(settings *rtl.ApiSettings) string {
	key := resolveInstanceCacheKey(settings)
	mcpAppStateCacheMu.RLock()
	defer mcpAppStateCacheMu.RUnlock()
	return instanceLocaleCache[key]
}

// SetLastObservedLocale records the most recently observed user locale for the
// given Looker instance settings.
func SetLastObservedLocale(settings *rtl.ApiSettings, locale string) {
	locale = strings.TrimSpace(locale)
	if locale == "" {
		return
	}
	key := resolveInstanceCacheKey(settings)
	mcpAppStateCacheMu.Lock()
	instanceLocaleCache[key] = locale
	mcpAppStateCacheMu.Unlock()
}

// AppendLocaleQueryParam appends a ?locale=<locale> query parameter to
// remoteURL if locale is non-empty and remoteURL does not already specify a
// locale parameter.
func AppendLocaleQueryParam(remoteURL, locale string) string {
	locale = strings.TrimSpace(locale)
	if remoteURL == "" || locale == "" {
		return remoteURL
	}
	parsed, err := url.Parse(remoteURL)
	if err != nil {
		return remoteURL
	}
	q := parsed.Query()
	if q.Get("locale") == "" {
		q.Set("locale", locale)
		parsed.RawQuery = q.Encode()
	}
	return parsed.String()
}

func fetchActiveColorCollections(ctx context.Context, sdk *v4.LookerSDK, settings *rtl.ApiSettings) map[string]any {
	cacheKey := resolveInstanceCacheKey(settings)
	mcpAppStateCacheMu.RLock()
	if entry, ok := colorCollectionsCache[cacheKey]; ok && len(entry.data) > 0 && time.Since(entry.fetchedAt) < colorCollectionsCacheTTL {
		data := entry.data
		mcpAppStateCacheMu.RUnlock()
		return data
	}
	mcpAppStateCacheMu.RUnlock()

	var ccMap map[string]any
	if err := sdk.AuthSession.Do(&ccMap, "GET", "/4.0", "/color_collections/active", nil, nil, settings); err != nil {
		if logger, lErr := util.LoggerFromContext(ctx); lErr == nil {
			logger.DebugContext(ctx, "failed to fetch active color collections for MCP app_state", "error", err)
		}
		return nil
	}
	if len(ccMap) == 0 {
		return nil
	}

	mcpAppStateCacheMu.Lock()
	colorCollectionsCache[cacheKey] = cachedColorCollections{
		data:      ccMap,
		fetchedAt: time.Now(),
	}
	mcpAppStateCacheMu.Unlock()
	return ccMap
}

// FetchMcpAppState concurrently fetches the authenticated user's profile,
// locale, and evaluated abilities alongside the Looker instance's active color
// collections (cached per instance for 5 minutes). Errors are logged at debug
// level and omitted so older Looker instances degrade gracefully.
func FetchMcpAppState(ctx context.Context, sdk *v4.LookerSDK, settings *rtl.ApiSettings) map[string]any {
	if sdk == nil || sdk.AuthSession == nil {
		return nil
	}

	var (
		wg        sync.WaitGroup
		userMap   map[string]any
		colorsMap map[string]any
	)

	wg.Add(2)
	go func() {
		defer wg.Done()
		var u map[string]any
		reqParams := map[string]any{"fields": mcpUserFields}
		if err := sdk.AuthSession.Do(&u, "GET", "/4.0", "/user", reqParams, nil, settings); err != nil {
			if logger, lErr := util.LoggerFromContext(ctx); lErr == nil {
				logger.DebugContext(ctx, "failed to fetch user abilities for MCP app_state", "error", err)
			}
			return
		}
		if len(u) > 0 {
			userMap = u
			if loc, ok := u["locale"].(string); ok && loc != "" {
				SetLastObservedLocale(settings, loc)
			}
		}
	}()

	go func() {
		defer wg.Done()
		colorsMap = fetchActiveColorCollections(ctx, sdk, settings)
	}()

	wg.Wait()

	if len(userMap) == 0 && len(colorsMap) == 0 {
		return nil
	}

	appState := make(map[string]any)
	if len(userMap) > 0 {
		appState["user"] = userMap
	}
	if len(colorsMap) > 0 {
		appState["color_collections"] = colorsMap
	}
	return appState
}

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
