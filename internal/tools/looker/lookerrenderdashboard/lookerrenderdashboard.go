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
package lookerrenderdashboard

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	yaml "github.com/goccy/go-yaml"
	"github.com/googleapis/mcp-toolbox/internal/resources"
	"github.com/googleapis/mcp-toolbox/internal/sources"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/tools/looker/lookercommon"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"

	"github.com/looker-open-source/sdk-codegen/go/rtl"
	v4 "github.com/looker-open-source/sdk-codegen/go/sdk/v4"
)

const (
	resourceType        string = "looker-render-dashboard"
	defaultResourceName string = "looker_render_dashboard_ui"
	defaultResourceURI  string = "ui://looker/render_dashboard.html"
)

func init() {
	if !tools.Register(resourceType, newConfig) {
		panic(fmt.Sprintf("tool type %q already registered", resourceType))
	}
}

func newConfig(ctx context.Context, name string, decoder *yaml.Decoder) (tools.ToolConfig, error) {
	actual := Config{ConfigBase: tools.ConfigBase{Name: name}}
	if err := decoder.DecodeContext(ctx, &actual); err != nil {
		return nil, err
	}
	return actual, nil
}

type compatibleSource interface {
	UseClientAuthorization() bool
	GetAuthTokenHeaderName() string
	LookerApiSettings() *rtl.ApiSettings
	GetLookerSDK(context.Context, string) (*v4.LookerSDK, error)
}

type Config struct {
	tools.ConfigBase `yaml:",inline"`
	Type             string                 `yaml:"type" validate:"required"`
	Source           string                 `yaml:"source" validate:"required"`
	Annotations      *tools.ToolAnnotations `yaml:"annotations,omitempty"`
}

// validate interface
var _ tools.ToolConfig = Config{}

func (cfg Config) ToolConfigType() string {
	return resourceType
}

type uiState struct {
	mu         sync.RWMutex
	src        compatibleSource
	cachedHTML string
	cacheTime  time.Time
}

func getDashboardParameters() parameters.Parameters {
	dashboardIdParameter := parameters.NewStringParameter(
		"dashboard_id",
		"The unique identifier (or slug) of the dashboard to retrieve and render.",
	)
	filtersParameter := parameters.NewMapParameter(
		"filters",
		"Optional dashboard filter overrides as key-value pairs (e.g. {\"State\": \"California\"}).",
		"",
		parameters.WithMapDefault(map[string]any{}),
	)

	return parameters.Parameters{
		dashboardIdParameter,
		filtersParameter,
	}
}

func (cfg Config) Initialize(context.Context) (tools.Tool, error) {
	if cfg.Description == "" {
		return nil, fmt.Errorf("description is required for tool %q", cfg.Name)
	}

	if cfg.UI == nil {
		cfg.UI = &tools.ToolUIMetadata{
			Resource:   defaultResourceName,
			Visibility: []tools.ToolVisibility{tools.VisibilityModel, tools.VisibilityApp},
		}
	} else {
		if cfg.UI.Resource == "" {
			cfg.UI.Resource = defaultResourceName
		}
		if len(cfg.UI.Visibility) == 0 {
			cfg.UI.Visibility = []tools.ToolVisibility{tools.VisibilityModel, tools.VisibilityApp}
		}
	}

	allParameters := getDashboardParameters()

	state := &uiState{}

	csp := &resources.CSPConfig{
		ResourceDomains: []string{
			"https://*.lookercdn.com",
			"https://lookercdn.com",
			"https://*.cdn.looker.app",
			"https://cdn.looker.app",
			"https://maps.googleapis.com",
			"https://maps.gstatic.com",
			"https://fonts.googleapis.com",
			"https://fonts.gstatic.com",
			"https://www.google.com",
			"https://www.gstatic.com",
			"https://*.googleapis.com",
			"https://*.gstatic.com",
			"https://*.google.com",
			"https://*.goog",
			"https://*.googleusercontent.com",
		},
		ConnectDomains: []string{
			"https://*.lookercdn.com",
			"https://lookercdn.com",
			"https://*.cdn.looker.app",
			"https://cdn.looker.app",
			"https://maps.googleapis.com",
			"https://maps.gstatic.com",
			"https://*.googleapis.com",
			"https://*.gstatic.com",
			"https://*.google.com",
			"https://*.goog",
			"https://*.googleusercontent.com",
		},
	}

	lookerBaseURL := os.Getenv("LOOKER_BASE_URL")
	lookerUIURL := os.Getenv("LOOKER_UI_URL")

	ensureDomain := func(list *[]string, domain string) {
		if domain == "" {
			return
		}
		domain = strings.TrimSuffix(domain, "/")
		for _, d := range *list {
			if strings.TrimSuffix(d, "/") == domain {
				return
			}
		}
		*list = append(*list, domain)
	}

	getOrigin := func(rawURL string) string {
		if rawURL == "" {
			return ""
		}
		rawURL = strings.TrimSpace(rawURL)
		if !strings.Contains(rawURL, "://") {
			rawURL = "http://" + rawURL
		}
		if parsed, err := url.Parse(rawURL); err == nil && parsed.Host != "" {
			return fmt.Sprintf("%s://%s", parsed.Scheme, parsed.Host)
		}
		return strings.TrimSuffix(rawURL, "/")
	}

	addOrigin := func(rawURL string) {
		origin := getOrigin(rawURL)
		if origin == "" {
			return
		}
		ensureDomain(&csp.ResourceDomains, origin)
		ensureDomain(&csp.ConnectDomains, origin)

		// If origin is HTTP, also ensure corresponding WebSocket origin for local dev hot reloading
		if strings.HasPrefix(origin, "http://") {
			wsOrigin := "ws://" + strings.TrimPrefix(origin, "http://")
			ensureDomain(&csp.ConnectDomains, wsOrigin)
		} else if strings.HasPrefix(origin, "https://") {
			wssOrigin := "wss://" + strings.TrimPrefix(origin, "https://")
			ensureDomain(&csp.ConnectDomains, wssOrigin)
		}
	}

	addOrigin(lookerBaseURL)
	addOrigin(lookerUIURL)

	// Permit official Looker Data Apps runtime and vendor bundles (lit-canvas, data-app.js, ag-grid, highcharts)
	ensureDomain(&csp.ResourceDomains, "https://www.gstatic.com")
	ensureDomain(&csp.ResourceDomains, "https://storage.googleapis.com")

	// In local development environments, permit all local dev ports for both resources and connect
	if strings.Contains(lookerBaseURL, "localhost") || strings.Contains(lookerUIURL, "localhost") ||
		strings.Contains(lookerBaseURL, "127.0.0.1") || strings.Contains(lookerUIURL, "127.0.0.1") {
		for _, host := range []string{"localhost", "127.0.0.1", "0.0.0.0"} {
			for _, port := range []string{"9998", "9999", "19999", "3035"} {
				ensureDomain(&csp.ResourceDomains, fmt.Sprintf("http://%s:%s", host, port))
				ensureDomain(&csp.ConnectDomains, fmt.Sprintf("http://%s:%s", host, port))
				ensureDomain(&csp.ConnectDomains, fmt.Sprintf("ws://%s:%s", host, port))
			}
			ensureDomain(&csp.ConnectDomains, fmt.Sprintf("ws://%s:*", host))
			ensureDomain(&csp.ConnectDomains, fmt.Sprintf("wss://%s:*", host))
		}
	}
	if strings.Contains(lookerBaseURL, "dev.looker.com") || strings.Contains(lookerUIURL, "dev.looker.com") {
		ensureDomain(&csp.ResourceDomains, "https://*.dev.looker.com")
		ensureDomain(&csp.ConnectDomains, "https://*.dev.looker.com")
		ensureDomain(&csp.ConnectDomains, "wss://*.dev.looker.com")
	}
	if strings.Contains(lookerBaseURL, "looker.com") || strings.Contains(lookerUIURL, "looker.com") {
		ensureDomain(&csp.ResourceDomains, "https://*.looker.com")
		ensureDomain(&csp.ConnectDomains, "https://*.looker.com")
		ensureDomain(&csp.ConnectDomains, "wss://*.looker.com")
	}

	toolInstance := Tool{
		BaseTool: tools.NewBaseTool(
			cfg,
			tools.GetAnnotationsOrDefault(cfg.Annotations, tools.NewReadOnlyAnnotations),
			tools.Manifest{
				Description:  cfg.Description,
				Parameters:   allParameters.Manifest(),
				AuthRequired: cfg.AuthRequired,
			},
			allParameters,
		),
		state: state,
		csp:   csp,
	}

	uiResource := resources.NewDynamicUIResource(
		cfg.UI.Resource,
		defaultResourceURI,
		"Looker dashboard rendering interface",
		csp,
		nil,
		func(ctx context.Context) (string, error) {
			return toolInstance.fetchRemoteUI(ctx)
		},
	)

	toolInstance.uiResource = uiResource
	return toolInstance, nil
}

// validate interfaces
var (
	_ tools.Tool                 = Tool{}
	_ resources.ResourceProvider = Tool{}
	baseTagRe                    = regexp.MustCompile(`(?i)<base\b[^>]*\/?>`)
	nonceAttrRe                  = regexp.MustCompile(`(?i)\s*nonce="[^"]*"`)
)

type Tool struct {
	tools.BaseTool[Config]
	uiResource resources.Resource
	state      *uiState
	csp        *resources.CSPConfig
}

// renderHTML makes the fetched UI HTML loadable inside the MCP host's sandbox.
// publicOrigin is the browser-facing Looker origin discovered from the HTML (see
// lookercommon.DiscoverMCPUIPublicOrigin). If it is empty, relative asset URLs
// are resolved against LOOKER_UI_URL, then the source BaseUrl, then
// LOOKER_BASE_URL.
func (t Tool) renderHTML(rawHTML, publicOrigin string) string {
	assetBaseURL := publicOrigin
	if lookerUIURL := os.Getenv("LOOKER_UI_URL"); assetBaseURL == "" && lookerUIURL != "" {
		lookerUIURL = strings.TrimSpace(lookerUIURL)
		if !strings.Contains(lookerUIURL, "://") {
			lookerUIURL = "http://" + lookerUIURL
		}
		if parsed, err := url.Parse(lookerUIURL); err == nil && parsed.Host != "" {
			assetBaseURL = fmt.Sprintf("%s://%s", parsed.Scheme, parsed.Host)
		}
	}
	if assetBaseURL == "" && t.state != nil {
		t.state.mu.RLock()
		src := t.state.src
		t.state.mu.RUnlock()
		if src != nil && src.LookerApiSettings() != nil && src.LookerApiSettings().BaseUrl != "" {
			assetBaseURL = strings.TrimSuffix(src.LookerApiSettings().BaseUrl, "/")
		}
	}
	if assetBaseURL == "" {
		if lookerBaseURL := os.Getenv("LOOKER_BASE_URL"); lookerBaseURL != "" {
			lookerBaseURL = strings.TrimSpace(lookerBaseURL)
			if !strings.Contains(lookerBaseURL, "://") {
				lookerBaseURL = "http://" + lookerBaseURL
			}
			if parsed, err := url.Parse(lookerBaseURL); err == nil && parsed.Host != "" {
				assetBaseURL = fmt.Sprintf("%s://%s", parsed.Scheme, parsed.Host)
			}
		}
	}

	res := strings.ReplaceAll(rawHTML, "__MCP_ASSET_BASE_URL__", assetBaseURL)
	if assetBaseURL != "" {
		res = strings.ReplaceAll(res, `src="/webpack/`, fmt.Sprintf(`src="%s/webpack/`, assetBaseURL))
		res = strings.ReplaceAll(res, `src="/public/mcp/ui/assets/`, fmt.Sprintf(`src="%s/public/mcp/ui/render_dashboard/assets/`, assetBaseURL))
		res = strings.ReplaceAll(res, `src="/public/mcp/ui/render_dashboard/assets/`, fmt.Sprintf(`src="%s/public/mcp/ui/render_dashboard/assets/`, assetBaseURL))
		res = strings.ReplaceAll(res, `src="/mcp/ui/assets/`, fmt.Sprintf(`src="%s/public/mcp/ui/render_dashboard/assets/`, assetBaseURL))
		res = strings.ReplaceAll(res, `url('/fonts/`, fmt.Sprintf(`url('%s/fonts/`, assetBaseURL))
		res = strings.ReplaceAll(res, `url("/fonts/`, fmt.Sprintf(`url("%s/fonts/`, assetBaseURL))
		res = strings.ReplaceAll(res, `url(/fonts/`, fmt.Sprintf(`url(%s/fonts/`, assetBaseURL))
	}
	// Strip any <base ...> tag to prevent CSP base-uri 'self' violation in sandboxed iframes
	res = baseTagRe.ReplaceAllString(res, "")
	// Strip any server-generated nonce attributes to prevent CSP nonce mismatch in sandboxed iframes
	res = nonceAttrRe.ReplaceAllString(res, "")
	return res
}

func (t Tool) fetchRemoteUI(ctx context.Context) (string, error) {
	if t.state == nil {
		return "", fmt.Errorf("tool state not initialized: unable to fetch dashboard UI")
	}

	// 1. Check in-memory cache (5-minute TTL)
	t.state.mu.RLock()
	if t.state.cachedHTML != "" && time.Since(t.state.cacheTime) < 5*time.Minute {
		html := t.state.cachedHTML
		t.state.mu.RUnlock()
		return html, nil
	}
	src := t.state.src
	t.state.mu.RUnlock()

	var remoteURL string
	if lookerUIURL := os.Getenv("LOOKER_UI_URL"); lookerUIURL != "" {
		lookerUIURL = strings.TrimSpace(lookerUIURL)
		if !strings.Contains(lookerUIURL, "://") {
			lookerUIURL = "http://" + lookerUIURL
		}
		lookerUIURL = strings.TrimSuffix(lookerUIURL, "/")
		if strings.HasSuffix(lookerUIURL, "/render_dashboard/assets") {
			remoteURL = lookerUIURL
		} else if strings.Contains(lookerUIURL, "/render_visualization/assets") {
			remoteURL = strings.ReplaceAll(lookerUIURL, "/render_visualization/assets", "/render_dashboard/assets")
		} else if !strings.HasSuffix(lookerUIURL, "/assets") {
			remoteURL = fmt.Sprintf("%s/public/mcp/ui/render_dashboard/assets", lookerUIURL)
		} else {
			remoteURL = lookerUIURL
		}
	} else if src != nil && src.LookerApiSettings() != nil && src.LookerApiSettings().BaseUrl != "" {
		baseURL := strings.TrimSuffix(src.LookerApiSettings().BaseUrl, "/")
		remoteURL = fmt.Sprintf("%s/public/mcp/ui/render_dashboard/assets", baseURL)
	}

	if remoteURL == "" {
		return "", fmt.Errorf("no Looker instance URL configured to fetch dashboard UI")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, remoteURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request for dashboard UI at %s: %w", remoteURL, err)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	if src != nil && src.LookerApiSettings() != nil && !src.LookerApiSettings().VerifySsl {
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to fetch dashboard UI from Looker at %s: %w", remoteURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return "", fmt.Errorf("tool UI access denied (HTTP %d): disabled by administrator", resp.StatusCode)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to fetch dashboard UI from Looker at %s: HTTP %d", remoteURL, resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil || len(bodyBytes) == 0 {
		return "", fmt.Errorf("empty dashboard UI returned from Looker at %s", remoteURL)
	}

	rawHTML := string(bodyBytes)
	publicOrigin := lookercommon.DiscoverMCPUIPublicOrigin(rawHTML)
	html := t.renderHTML(rawHTML, publicOrigin)
	t.state.mu.Lock()
	t.allowUIOrigin(remoteURL, publicOrigin)
	t.state.cachedHTML = html
	t.state.cacheTime = time.Now()
	t.state.mu.Unlock()

	return html, nil
}

// allowUIOrigin adds the origin the browser loads UI assets from to the CSP.
// That is the public origin Looker declared in the HTML. If Looker didn't
// declare one (e.g. local development over http), the origin toolbox fetched
// the UI from is allowed instead. The caller must hold t.state.mu.
func (t Tool) allowUIOrigin(remoteURL, publicOrigin string) {
	if t.csp == nil {
		return
	}
	if publicOrigin != "" {
		lookercommon.AddCSPOrigin(t.csp, publicOrigin)
		return
	}
	if parsed, err := url.Parse(remoteURL); err == nil && parsed.Host != "" {
		origin := fmt.Sprintf("%s://%s", parsed.Scheme, parsed.Host)
		for _, d := range t.csp.ResourceDomains {
			if strings.TrimSuffix(d, "/") == origin {
				return
			}
		}
		t.csp.ResourceDomains = append(t.csp.ResourceDomains, origin)
	}
}

func (t Tool) GetResources() []resources.Resource {
	if t.uiResource == nil {
		return nil
	}
	return []resources.Resource{t.uiResource}
}

func (t Tool) GetSourceName() string {
	return t.Cfg.Source
}

func (t Tool) ToConfig() tools.ToolConfig {
	return t.Cfg
}

func (t Tool) ValidateSource(source sources.Source) error {
	cs, ok := source.(compatibleSource)
	if !ok {
		return fmt.Errorf("invalid source for %q tool: source %q is not a compatible type", t.Cfg.Type, t.Cfg.Source)
	}
	if t.state != nil {
		t.state.mu.Lock()
		t.state.src = cs
		t.state.mu.Unlock()
	}
	return nil
}

func (t Tool) Invoke(ctx context.Context, s sources.Source, params parameters.ParamValues, accessToken tools.AccessToken) (any, util.ToolboxError) {
	source, ok := s.(compatibleSource)
	if !ok {
		return nil, util.NewClientServerError("source used is not compatible with the tool", http.StatusInternalServerError, nil)
	}
	logger, err := util.LoggerFromContext(ctx)
	if err != nil {
		return nil, util.NewClientServerError("unable to get logger from ctx", http.StatusInternalServerError, err)
	}

	paramMap := params.AsMap()
	dashboardId, ok := paramMap["dashboard_id"].(string)
	if !ok || dashboardId == "" {
		return nil, util.NewAgentError("dashboard_id parameter missing or invalid", nil)
	}

	filtersMap, _ := paramMap["filters"].(map[string]any)

	sdk, err := source.GetLookerSDK(ctx, string(accessToken))
	if err != nil {
		return nil, util.NewClientServerError("error getting sdk", http.StatusInternalServerError, err)
	}

	var rawDashboard map[string]any
	err = sdk.AuthSession.Do(&rawDashboard, "GET", "/4.0", fmt.Sprintf("/dashboards/%s", url.PathEscape(dashboardId)), nil, nil, source.LookerApiSettings())
	if err != nil {
		if strings.Contains(err.Error(), "status=401") {
			return nil, util.NewClientServerError("unauthorized error", http.StatusUnauthorized, err)
		}
		return nil, util.ProcessGeneralError(err)
	}

	var dashboard v4.Dashboard
	if rawBytes, marshalErr := json.Marshal(rawDashboard); marshalErr == nil {
		_ = json.Unmarshal(rawBytes, &dashboard)
	}

	inlineExtensions := make(map[string]any)
	if dashboard.DashboardElements != nil {
		for _, el := range *dashboard.DashboardElements {
			if el.ExtensionId == nil || *el.ExtensionId == "" {
				continue
			}
			extID := *el.ExtensionId
			isExtType := el.Type != nil && *el.Type == "extension"
			if strings.HasPrefix(extID, "db::") || isExtType {
				rawID := strings.TrimPrefix(extID, "db::")
				escapedID := url.PathEscape(rawID)

				var metaMap map[string]any
				if metaErr := sdk.AuthSession.Do(&metaMap, "GET", "/4.0", fmt.Sprintf("/inline_extension/%s", escapedID), nil, nil, source.LookerApiSettings()); metaErr != nil {
					logger.WarnContext(ctx, "failed to fetch inline extension metadata", "extension_id", extID, "error", metaErr)
				}

				var bundleCode string
				if bundleErr := sdk.AuthSession.Do(&bundleCode, "GET", "/4.0", fmt.Sprintf("/inline_extension/%s/bundle.js", escapedID), nil, nil, source.LookerApiSettings()); bundleErr != nil {
					logger.WarnContext(ctx, "failed to fetch inline extension bundle", "extension_id", extID, "error", bundleErr)
				} else if bundleCode != "" {
					entry := map[string]any{
						"id":   "db::" + rawID,
						"code": bundleCode,
					}
					if metaMap != nil {
						if sha, ok := metaMap["sha256_hash"]; ok {
							entry["sha256_hash"] = sha
						}
						if ent, ok := metaMap["entitlements"]; ok {
							entry["entitlements"] = ent
						}
						if label, ok := metaMap["label"]; ok {
							entry["label"] = label
						}
					}
					inlineExtensions["db::"+rawID] = entry
				}
			}
		}
	}

	dashboardPayload := PruneDashboardPayload(InjectComponentGroupIDs(dashboard, ExtractComponentGroupIDs(rawDashboard)))

	dashboardData := map[string]any{
		"dashboard": dashboardPayload,
		"filters":   filtersMap,
	}
	if len(inlineExtensions) > 0 {
		dashboardData["inline_extensions"] = inlineExtensions
	}

	logger.DebugContext(ctx, "dashboardData prepared", "dashboard_id", dashboardId, "inline_extensions_count", len(inlineExtensions))

	var dashTitle string
	if dashboard.Title != nil {
		dashTitle = *dashboard.Title
	}
	var tileCount int
	if dashboard.DashboardElements != nil {
		tileCount = len(*dashboard.DashboardElements)
	}
	var filterCount int
	if dashboard.DashboardFilters != nil {
		filterCount = len(*dashboard.DashboardFilters)
	}

	resultPayload := map[string]any{
		"dashboardData": dashboardData,
		"dashboard_id":  dashboardId,
	}
	if _, tooLarge := lookercommon.ExceedsMCPPayloadLimit(resultPayload, lookercommon.MaxMCPPayloadBytes); tooLarge {
		errMsg := "This dashboard is too large to display here. Open it in Looker to view the full dashboard."
		tooLargePayload := map[string]any{
			"dashboard_id":       dashboardId,
			"title":              dashTitle,
			"response_too_large": true,
			"error":              errMsg,
			"dashboardData": map[string]any{
				"dashboard_id":       dashboardId,
				"title":              dashTitle,
				"response_too_large": true,
				"error":              errMsg,
			},
		}
		return tools.StructuredToolResult{
			Content:           tooLargePayload,
			StructuredContent: tooLargePayload,
		}, nil
	}

	summary := map[string]any{
		"dashboard_id": dashboardId,
		"title":        dashTitle,
		"tile_count":   tileCount,
		"filter_count": filterCount,
		"status":       "rendered_in_ui",
	}
	if len(filtersMap) > 0 {
		summary["applied_filters"] = filtersMap
	}

	return tools.StructuredToolResult{
		Content:           summary,
		StructuredContent: resultPayload,
	}, nil
}

func pruneQueryMap(qMap map[string]any) {
	if qMap == nil {
		return
	}
	delete(qMap, "can")
	delete(qMap, "filter_config")
	delete(qMap, "runtime")
	delete(qMap, "expanded_share_url")
	delete(qMap, "url")
	if vcMap, ok := qMap["vis_config"].(map[string]any); ok {
		delete(vcMap, "query_fields")
	}
}

// PruneDashboardPayload strips bulky, unused Looker API metadata (such as
// permission maps, explore UI filter_config blobs, expanded_share_url/url,
// LookML SQL definitions, drill_fields, and duplicate nested query objects)
// from a dashboard payload so large multi-tile dashboards stay well within
// MCP host tool response limits.
func PruneDashboardPayload(dashboard any) any {
	var dashMap map[string]any
	if existingMap, ok := dashboard.(map[string]any); ok {
		dashMap = existingMap
	} else {
		rawBytes, err := json.Marshal(dashboard)
		if err != nil {
			return dashboard
		}
		if err := json.Unmarshal(rawBytes, &dashMap); err != nil {
			return dashboard
		}
	}

	delete(dashMap, "can")

	if elements, ok := dashMap["dashboard_elements"].([]any); ok {
		for _, rawEl := range elements {
			elMap, ok := rawEl.(map[string]any)
			if !ok {
				continue
			}
			delete(elMap, "can")

			if rmMap, ok := elMap["result_maker"].(map[string]any); ok {
				delete(rmMap, "can")
				if rmVisCfg, ok := rmMap["vis_config"].(map[string]any); ok {
					delete(rmVisCfg, "query_fields")
				}
				if rmQuery, ok := rmMap["query"].(map[string]any); ok {
					pruneQueryMap(rmQuery)
					if rmMap["vis_config"] != nil {
						delete(rmQuery, "vis_config")
					}
					delete(elMap, "query")
				}
			}
			if elQuery, ok := elMap["query"].(map[string]any); ok {
				pruneQueryMap(elQuery)
			}
			if lookMap, ok := elMap["look"].(map[string]any); ok {
				delete(lookMap, "can")
				delete(lookMap, "space")
				delete(lookMap, "folder")
				delete(lookMap, "user")
				if rmMap, ok := elMap["result_maker"].(map[string]any); ok && rmMap["query"] != nil {
					delete(lookMap, "query")
				} else if lookQuery, ok := lookMap["query"].(map[string]any); ok {
					pruneQueryMap(lookQuery)
				}
			}
		}
	}

	if filters, ok := dashMap["dashboard_filters"].([]any); ok {
		for _, rawFilter := range filters {
			fMap, ok := rawFilter.(map[string]any)
			if !ok {
				continue
			}
			delete(fMap, "can")
			if fieldMap, ok := fMap["field"].(map[string]any); ok {
				for _, fieldKey := range []string{
					"can",
					"sql",
					"sql_case",
					"drill_fields",
					"source_file",
					"source_file_path",
					"lookml_link",
					"user_attribute_filter_types",
					"available_custom_timeframes",
					"liquid_expression",
					"lookml_expression",
					"synonyms",
					"tags",
					"times_used",
					"field_group_label",
					"field_group_variant",
					"permanent",
					"scope",
				} {
					delete(fieldMap, fieldKey)
				}
			}
		}
	}

	if layouts, ok := dashMap["dashboard_layouts"].([]any); ok {
		for _, rawLayout := range layouts {
			lMap, ok := rawLayout.(map[string]any)
			if !ok {
				continue
			}
			delete(lMap, "can")
			if comps, ok := lMap["dashboard_layout_components"].([]any); ok {
				for _, rawComp := range comps {
					if cMap, ok := rawComp.(map[string]any); ok {
						delete(cMap, "can")
					}
				}
			}
		}
	}

	return dashMap
}

// ExtractComponentGroupIDs extracts a map of layout component ID to group_id
// from the raw Looker dashboard JSON payload, since v4.DashboardLayoutComponent
// in the Go SDK does not yet include the group_id field.
func ExtractComponentGroupIDs(rawDashboard map[string]any) map[string]string {
	groupIDs := make(map[string]string)
	if rawDashboard == nil {
		return groupIDs
	}
	layouts, ok := rawDashboard["dashboard_layouts"].([]any)
	if !ok {
		return groupIDs
	}
	for _, rawLayout := range layouts {
		layoutMap, ok := rawLayout.(map[string]any)
		if !ok {
			continue
		}
		components, ok := layoutMap["dashboard_layout_components"].([]any)
		if !ok {
			continue
		}
		for _, rawComp := range components {
			compMap, ok := rawComp.(map[string]any)
			if !ok {
				continue
			}
			compID := fmt.Sprintf("%v", compMap["id"])
			rawGroupID, hasGroup := compMap["group_id"]
			if !hasGroup || rawGroupID == nil || compID == "" || compID == "<nil>" {
				continue
			}
			groupIDStr := strings.TrimSpace(fmt.Sprintf("%v", rawGroupID))
			if groupIDStr != "" && groupIDStr != "<nil>" {
				groupIDs[compID] = groupIDStr
			}
		}
	}
	return groupIDs
}

// InjectComponentGroupIDs returns the dashboard payload with group_id restored
// on matching dashboard_layout_components when groupIDs is non-empty.
func InjectComponentGroupIDs(dashboard v4.Dashboard, groupIDs map[string]string) any {
	if len(groupIDs) == 0 {
		return dashboard
	}
	rawBytes, err := json.Marshal(dashboard)
	if err != nil {
		return dashboard
	}
	var dashMap map[string]any
	if err := json.Unmarshal(rawBytes, &dashMap); err != nil {
		return dashboard
	}
	layouts, ok := dashMap["dashboard_layouts"].([]any)
	if !ok {
		return dashboard
	}
	for _, rawLayout := range layouts {
		layoutMap, ok := rawLayout.(map[string]any)
		if !ok {
			continue
		}
		components, ok := layoutMap["dashboard_layout_components"].([]any)
		if !ok {
			continue
		}
		for _, rawComp := range components {
			compMap, ok := rawComp.(map[string]any)
			if !ok {
				continue
			}
			compID := fmt.Sprintf("%v", compMap["id"])
			if groupID, found := groupIDs[compID]; found {
				compMap["group_id"] = groupID
			}
		}
	}
	return dashMap
}

func (t Tool) RequiresClientAuthorization(source sources.Source) (bool, error) {
	s, ok := source.(compatibleSource)
	if !ok {
		return false, fmt.Errorf("invalid source for %q tool: source %q is not a compatible type", t.Cfg.Type, t.Cfg.Source)
	}
	return s.UseClientAuthorization(), nil
}

func (t Tool) GetAuthTokenHeaderName(source sources.Source) (string, error) {
	s, ok := source.(compatibleSource)
	if !ok {
		return "", fmt.Errorf("invalid source for %q tool: source %q is not a compatible type", t.Cfg.Type, t.Cfg.Source)
	}
	return s.GetAuthTokenHeaderName(), nil
}
