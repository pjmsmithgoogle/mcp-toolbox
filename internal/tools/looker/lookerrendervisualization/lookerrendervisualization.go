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
package lookerrendervisualization

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
	resourceType       string = "looker-render-visualization"
	defaultResourceURI string = "ui://looker/render_visualization.html"
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

func getVisualizationParameters() parameters.Parameters {
	queryIdParameter := parameters.NewStringParameter(
		"query_id",
		"Optional Looker query ID (or slug) to fetch and render the saved visualization directly.",
		parameters.WithStringDefault(""),
	)
	modelParameter := parameters.NewStringParameter(
		"model",
		"The model containing the explore (optional if query_id is provided).",
		parameters.WithStringDefault(""),
	)
	exploreParameter := parameters.NewStringParameter(
		"explore",
		"The explore to be queried (optional if query_id is provided).",
		parameters.WithStringDefault(""),
	)
	fieldsParameter := parameters.NewArrayParameter(
		"fields",
		"The fields to be retrieved (optional if query_id is provided).",
		parameters.NewStringParameter("field", "A field to be returned in the query"),
		parameters.WithArrayDefault([]any{}),
	)
	filtersParameter := parameters.NewMapParameter(
		"filters",
		"The filters for the query.",
		"",
		parameters.WithMapDefault(map[string]any{}),
	)
	pivotsParameter := parameters.NewArrayParameter(
		"pivots",
		"The query pivots.",
		parameters.NewStringParameter("pivot_field", "A field to be used as a pivot in the query"),
		parameters.WithArrayDefault([]any{}),
	)
	sortsParameter := parameters.NewArrayParameter(
		"sorts",
		"The sorts like \"field.id desc 0\".",
		parameters.NewStringParameter("sort_field", "A field to be used as a sort in the query"),
		parameters.WithArrayDefault([]any{}),
	)
	limitParameter := parameters.NewIntParameter("limit", "The row limit.", parameters.WithIntDefault(500))
	tzParameter := parameters.NewStringParameter("tz", "The query timezone.", parameters.WithStringDefault(""))
	filterExpressionParameter := parameters.NewStringParameter("filter_expression", "An optional filter expression string.", parameters.WithStringDefault(""))
	dynamicFieldsParameter := parameters.NewArrayParameter(
		"dynamic_fields",
		"An optional array of dynamic fields (table calculations, custom measures, custom dimensions).",
		parameters.NewMapParameter("dynamic_field", "A dynamic field definition", ""),
		parameters.WithArrayDefault([]any{}),
	)
	visConfigParameter := parameters.NewStringParameter(
		"vis_config",
		"Optional JSON string specifying the visualization configuration (e.g. chart type, options).",
		parameters.WithStringDefault(""),
	)

	return parameters.Parameters{
		queryIdParameter,
		modelParameter,
		exploreParameter,
		fieldsParameter,
		filtersParameter,
		pivotsParameter,
		sortsParameter,
		limitParameter,
		tzParameter,
		filterExpressionParameter,
		dynamicFieldsParameter,
		visConfigParameter,
	}
}

func (cfg Config) Initialize(context.Context) (tools.Tool, error) {
	if cfg.Description == "" {
		return nil, fmt.Errorf("description is required for tool %q", cfg.Name)
	}

	if cfg.UI == nil {
		cfg.UI = &tools.ToolUIMeta{
			ResourceURI: defaultResourceURI,
			Visibility:  []string{"model", "app"},
		}
	} else {
		if cfg.UI.ResourceURI == "" {
			cfg.UI.ResourceURI = defaultResourceURI
		}
		if len(cfg.UI.Visibility) == 0 {
			cfg.UI.Visibility = []string{"model", "app"}
		}
	}

	allParameters := getVisualizationParameters()

	state := &uiState{}

	var csp *resources.McpUiResourceCsp
	if cfg.UI != nil && cfg.UI.CSP != nil {
		cspCopy := *cfg.UI.CSP
		csp = &cspCopy
	} else {
		csp = &resources.McpUiResourceCsp{}
	}

	cloudRunURL := os.Getenv("CLOUD_RUN_URL")
	if cloudRunURL == "" {
		cloudRunURL = os.Getenv("SERVICE_URL")
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
		if parsed, err := url.Parse(rawURL); err == nil && parsed.Host != "" {
			return fmt.Sprintf("%s://%s", parsed.Scheme, parsed.Host)
		}
		return strings.TrimSuffix(rawURL, "/")
	}

	if origin := getOrigin(cloudRunURL); origin != "" {
		ensureDomain(&csp.ResourceDomains, origin)
		ensureDomain(&csp.ConnectDomains, origin)
	}
	if origin := getOrigin(lookerBaseURL); origin != "" {
		ensureDomain(&csp.ResourceDomains, origin)
		ensureDomain(&csp.ConnectDomains, origin)
	}
	if origin := getOrigin(lookerUIURL); origin != "" {
		ensureDomain(&csp.ResourceDomains, origin)
		ensureDomain(&csp.ConnectDomains, origin)
	}

	if cfg.UI != nil {
		cfg.UI.CSP = csp
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
	}

	uiResource := resources.NewDynamicUIResource(
		cfg.UI.ResourceURI,
		"render_visualization.html",
		"Looker visualization rendering interface",
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
)

type Tool struct {
	tools.BaseTool[Config]
	uiResource resources.Resource
	state      *uiState
}

func (t Tool) renderHTML(rawHTML string) string {
	assetBaseURL := ""
	if lookerUIURL := os.Getenv("LOOKER_UI_URL"); lookerUIURL != "" {
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
			assetBaseURL = strings.TrimSuffix(lookerBaseURL, "/")
		}
	}

	res := strings.ReplaceAll(rawHTML, "__MCP_ASSET_BASE_URL__", assetBaseURL)
	if assetBaseURL != "" {
		res = strings.ReplaceAll(res, `src="/webpack/`, fmt.Sprintf(`src="%s/webpack/`, assetBaseURL))
		res = strings.ReplaceAll(res, `src="/public/mcp/ui/assets/`, fmt.Sprintf(`src="%s/public/mcp/ui/render_visualization/assets/`, assetBaseURL))
		res = strings.ReplaceAll(res, `src="/public/mcp/ui/render_visualization/assets/`, fmt.Sprintf(`src="%s/public/mcp/ui/render_visualization/assets/`, assetBaseURL))
		res = strings.ReplaceAll(res, `src="/mcp/ui/assets/`, fmt.Sprintf(`src="%s/public/mcp/ui/render_visualization/assets/`, assetBaseURL))
	}
	// Strip any <base ...> tag to prevent CSP base-uri 'self' violation in sandboxed iframes
	res = baseTagRe.ReplaceAllString(res, "")
	return res
}

func (t Tool) fetchRemoteUI(ctx context.Context) (string, error) {
	if t.state == nil {
		return "", fmt.Errorf("tool state not initialized: unable to fetch visualization UI")
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
		lookerUIURL = strings.TrimSuffix(lookerUIURL, "/")
		if !strings.HasSuffix(lookerUIURL, "/assets") {
			remoteURL = fmt.Sprintf("%s/public/mcp/ui/render_visualization/assets", lookerUIURL)
		} else {
			remoteURL = lookerUIURL
		}
	} else if t.Cfg.UI != nil && t.Cfg.UI.RemoteURL != "" {
		remoteURL = t.Cfg.UI.RemoteURL
	} else if src != nil && src.LookerApiSettings() != nil && src.LookerApiSettings().BaseUrl != "" {
		baseURL := strings.TrimSuffix(src.LookerApiSettings().BaseUrl, "/")
		remoteURL = fmt.Sprintf("%s/public/mcp/ui/render_visualization/assets", baseURL)
	}

	if remoteURL == "" {
		return "", fmt.Errorf("no Looker instance URL configured to fetch visualization UI")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, remoteURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request for visualization UI at %s: %w", remoteURL, err)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	if src != nil && src.LookerApiSettings() != nil && !src.LookerApiSettings().VerifySsl {
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to fetch visualization UI from Looker at %s: %w", remoteURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return "", fmt.Errorf("tool UI access denied (HTTP %d): disabled by administrator", resp.StatusCode)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to fetch visualization UI from Looker at %s: HTTP %d", remoteURL, resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil || len(bodyBytes) == 0 {
		return "", fmt.Errorf("empty visualization UI returned from Looker at %s", remoteURL)
	}

	html := t.renderHTML(string(bodyBytes))
	t.state.mu.Lock()
	t.state.cachedHTML = html
	t.state.cacheTime = time.Now()
	t.state.mu.Unlock()

	return html, nil
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
	sdk, err := source.GetLookerSDK(ctx, string(accessToken))
	if err != nil {
		return nil, util.NewClientServerError("error getting sdk", http.StatusInternalServerError, err)
	}

	paramMap := params.AsMap()
	queryIdVal, _ := paramMap["query_id"].(string)
	visConfigVal := paramMap["vis_config"]

	var visConfigObj any
	if visConfigVal != nil && visConfigVal != "" {
		if str, ok := visConfigVal.(string); ok {
			var parsed any
			if json.Unmarshal([]byte(str), &parsed) == nil {
				visConfigObj = parsed
			} else {
				visConfigObj = str
			}
		} else {
			visConfigObj = visConfigVal
		}
	}

	// 1. If query_id is provided, fetch query by ID/slug and run it
	if queryIdVal != "" {
		queryDef, qErr := lookercommon.GetQuery(ctx, sdk, queryIdVal, source.LookerApiSettings())
		if qErr != nil {
			queryDef, qErr = lookercommon.GetQueryBySlug(ctx, sdk, queryIdVal, source.LookerApiSettings())
			if qErr != nil {
				logger.WarnContext(ctx, "could not fetch query definition by id or slug", "error", qErr)
			}
		}

		resp, rErr := lookercommon.RunSavedQuery(ctx, sdk, queryIdVal, "json_detail", source.LookerApiSettings())
		if rErr != nil && queryDef != nil && queryDef.Model != "" && queryDef.View != "" {
			wq := &v4.WriteQuery{
				Model:   queryDef.Model,
				View:    queryDef.View,
				Fields:  queryDef.Fields,
				Filters: queryDef.Filters,
				Pivots:  queryDef.Pivots,
				Sorts:   queryDef.Sorts,
				Limit:   queryDef.Limit,
			}
			resp, rErr = lookercommon.RunInlineQuery(ctx, sdk, wq, "json_detail", source.LookerApiSettings())
		}
		if rErr != nil {
			if strings.Contains(rErr.Error(), "status=401") {
				return nil, util.NewClientServerError("unauthorized error", http.StatusUnauthorized, rErr)
			}
			return nil, util.ProcessGeneralError(rErr)
		}

		var detailResp map[string]any
		if err := json.Unmarshal([]byte(resp), &detailResp); err != nil {
			return nil, util.NewClientServerError("error unmarshaling json_detail response", http.StatusInternalServerError, err)
		}

		if (visConfigObj == nil || visConfigObj == "") && queryDef != nil && queryDef.VisConfig != nil {
			visConfigObj = queryDef.VisConfig
		}
		if visConfigObj == nil || visConfigObj == "" {
			visConfigObj = map[string]any{"type": "table", "show_row_numbers": true}
		}

		modelVal, _ := paramMap["model"].(string)
		exploreVal, _ := paramMap["explore"].(string)

		title := "Looker Visualization"
		if queryDef != nil && queryDef.View != "" {
			title = queryDef.View
		} else if exploreVal != "" {
			title = exploreVal
		}

		queryMeta := map[string]any{
			"id": queryIdVal,
		}
		if modelVal != "" {
			queryMeta["model"] = modelVal
		}
		if exploreVal != "" {
			queryMeta["view"] = exploreVal
		}
		if queryDef != nil {
			if queryDef.Model != "" {
				queryMeta["model"] = queryDef.Model
			}
			if queryDef.View != "" {
				queryMeta["view"] = queryDef.View
			}
			if queryDef.Fields != nil {
				queryMeta["fields"] = *queryDef.Fields
			}
			if queryDef.Slug != nil && *queryDef.Slug != "" {
				queryMeta["slug"] = *queryDef.Slug
			}
			if queryDef.ShareUrl != nil && *queryDef.ShareUrl != "" {
				queryMeta["share_url"] = *queryDef.ShareUrl
			}
			if queryDef.ExpandedShareUrl != nil && *queryDef.ExpandedShareUrl != "" {
				queryMeta["expanded_share_url"] = *queryDef.ExpandedShareUrl
			}
			if queryDef.Url != nil && *queryDef.Url != "" {
				queryMeta["url"] = *queryDef.Url
			}
		}

		visualizationData := map[string]any{
			"queryResult": map[string]any{
				"data":        detailResp["data"],
				"fields":      detailResp["fields"],
				"pivots":      detailResp["pivots"],
				"totals_data": detailResp["totals_data"],
			},
			"visConfig": visConfigObj,
			"title":     title,
			"query":     queryMeta,
		}

		return map[string]any{
			"visualizationData": visualizationData,
			"data":              detailResp["data"],
			"fields":            detailResp["fields"],
			"pivots":            detailResp["pivots"],
			"vis_config":        visConfigObj,
			"query_id":          queryIdVal,
		}, nil
	}

	// 2. Inline query path with json_detail
	wq, err := lookercommon.ProcessQueryArgs(ctx, params)
	if err != nil {
		return nil, util.NewAgentError("error building WriteQuery request", err)
	}
	if escErr := lookercommon.EscapeUnquotedParameterFilters(ctx, sdk, wq, source.LookerApiSettings()); escErr != nil {
		logger.WarnContext(ctx, "skipping unquoted-parameter escape, metadata lookup failed", "error", escErr)
	}

	resp, err := lookercommon.RunInlineQuery(ctx, sdk, wq, "json_detail", source.LookerApiSettings())
	if err != nil {
		resp, err = lookercommon.RunInlineQuery(ctx, sdk, wq, "json", source.LookerApiSettings())
	}
	if err != nil {
		if strings.Contains(err.Error(), "status=401") {
			return nil, util.NewClientServerError("unauthorized error", http.StatusUnauthorized, err)
		}
		return nil, util.ProcessGeneralError(err)
	}

	var rawParsed any
	if err := json.Unmarshal([]byte(resp), &rawParsed); err != nil {
		return nil, util.NewClientServerError("error unmarshaling query response", http.StatusInternalServerError, err)
	}

	var dataList []any
	var fieldsObj any
	var pivotsList any
	var totalsObj any

	if detailMap, ok := rawParsed.(map[string]any); ok {
		if d, ok := detailMap["data"].([]any); ok {
			dataList = d
		}
		fieldsObj = detailMap["fields"]
		pivotsList = detailMap["pivots"]
		totalsObj = detailMap["totals_data"]
	} else if arr, ok := rawParsed.([]any); ok {
		dataList = arr
	}

	if visConfigObj == nil {
		visConfigObj = map[string]any{"type": "table", "show_row_numbers": true}
	}

	visualizationData := map[string]any{
		"queryResult": map[string]any{
			"data":        dataList,
			"fields":      fieldsObj,
			"pivots":      pivotsList,
			"totals_data": totalsObj,
		},
		"visConfig": visConfigObj,
		"title":     wq.View,
		"query": map[string]any{
			"model":   wq.Model,
			"view":    wq.View,
			"fields":  wq.Fields,
			"filters": wq.Filters,
			"pivots":  wq.Pivots,
			"sorts":   wq.Sorts,
			"limit":   wq.Limit,
		},
	}

	return map[string]any{
		"visualizationData": visualizationData,
		"data":              dataList,
		"fields":            fieldsObj,
		"model":             wq.Model,
		"explore":           wq.View,
		"vis_config":        visConfigObj,
	}, nil
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
