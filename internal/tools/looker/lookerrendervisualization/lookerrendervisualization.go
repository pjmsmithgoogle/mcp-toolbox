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
	resourceType        string = "looker-render-visualization"
	defaultResourceName string = "looker_render_visualization_ui"
	defaultResourceURI  string = "ui://looker/render_visualization.html"
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
	limitParameter := parameters.NewIntParameter("limit", "The row limit.", parameters.WithIntRequired(false))
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

	allParameters := getVisualizationParameters()

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
			"https://*.googleapis.com",
			"https://*.google.com",
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
	nonceAttrRe                  = regexp.MustCompile(`(?i)\s*nonce="[^"]*"`)
)

type Tool struct {
	tools.BaseTool[Config]
	uiResource resources.Resource
	state      *uiState
	csp        *resources.CSPConfig
}

func (t Tool) renderHTML(rawHTML string) string {
	assetBaseURL := ""
	if lookerUIURL := os.Getenv("LOOKER_UI_URL"); lookerUIURL != "" {
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
		res = strings.ReplaceAll(res, `src="/public/mcp/ui/assets/`, fmt.Sprintf(`src="%s/public/mcp/ui/render_visualization/assets/`, assetBaseURL))
		res = strings.ReplaceAll(res, `src="/public/mcp/ui/render_visualization/assets/`, fmt.Sprintf(`src="%s/public/mcp/ui/render_visualization/assets/`, assetBaseURL))
		res = strings.ReplaceAll(res, `src="/mcp/ui/assets/`, fmt.Sprintf(`src="%s/public/mcp/ui/render_visualization/assets/`, assetBaseURL))
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
		lookerUIURL = strings.TrimSpace(lookerUIURL)
		if !strings.Contains(lookerUIURL, "://") {
			lookerUIURL = "http://" + lookerUIURL
		}
		lookerUIURL = strings.TrimSuffix(lookerUIURL, "/")
		if strings.HasSuffix(lookerUIURL, "/render_visualization/assets") {
			remoteURL = lookerUIURL
		} else if strings.Contains(lookerUIURL, "/render_dashboard/assets") {
			remoteURL = strings.ReplaceAll(lookerUIURL, "/render_dashboard/assets", "/render_visualization/assets")
		} else if !strings.HasSuffix(lookerUIURL, "/assets") {
			remoteURL = fmt.Sprintf("%s/public/mcp/ui/render_visualization/assets", lookerUIURL)
		} else {
			remoteURL = lookerUIURL
		}
	} else if src != nil && src.LookerApiSettings() != nil && src.LookerApiSettings().BaseUrl != "" {
		baseURL := strings.TrimSuffix(src.LookerApiSettings().BaseUrl, "/")
		remoteURL = fmt.Sprintf("%s/public/mcp/ui/render_visualization/assets", baseURL)
	}

	if remoteURL == "" {
		return "", fmt.Errorf("no Looker instance URL configured to fetch visualization UI")
	}

	if parsed, err := url.Parse(remoteURL); err == nil && parsed.Host != "" && t.csp != nil {
		origin := fmt.Sprintf("%s://%s", parsed.Scheme, parsed.Host)
		t.state.mu.Lock()
		found := false
		for _, d := range t.csp.ResourceDomains {
			if strings.TrimSuffix(d, "/") == origin {
				found = true
				break
			}
		}
		if !found {
			t.csp.ResourceDomains = append(t.csp.ResourceDomains, origin)
		}
		t.state.mu.Unlock()
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

	// 1. If query_id is provided, fetch query by ID/slug and run it (merging parameter overrides if provided)
	if queryIdVal != "" {
		queryDef, qErr := lookercommon.GetQuery(ctx, sdk, queryIdVal, source.LookerApiSettings())
		if qErr != nil {
			queryDef, qErr = lookercommon.GetQueryBySlug(ctx, sdk, queryIdVal, source.LookerApiSettings())
			if qErr != nil {
				logger.WarnContext(ctx, "could not fetch query definition by id or slug", "error", qErr)
			}
		}

		var wq *v4.WriteQuery
		hasOverrides := false

		if queryDef != nil && queryDef.Model != "" && queryDef.View != "" {
			wq = &v4.WriteQuery{
				Model:            queryDef.Model,
				View:             queryDef.View,
				Fields:           queryDef.Fields,
				Filters:          queryDef.Filters,
				Pivots:           queryDef.Pivots,
				Sorts:            queryDef.Sorts,
				Limit:            queryDef.Limit,
				FilterExpression: queryDef.FilterExpression,
				DynamicFields:    queryDef.DynamicFields,
				QueryTimezone:    queryDef.QueryTimezone,
			}

			// Apply parameter overrides if provided by caller
			if limitVal, ok := paramMap["limit"].(int); ok && limitVal > 0 {
				limitStr := fmt.Sprintf("%d", limitVal)
				wq.Limit = &limitStr
				hasOverrides = true
			}
			if sortsSlice, ok := paramMap["sorts"].([]any); ok && len(sortsSlice) > 0 {
				if s, sErr := parameters.ConvertAnySliceToTyped(sortsSlice, "string"); sErr == nil {
					sorts := s.([]string)
					wq.Sorts = &sorts
					hasOverrides = true
				}
			}
			if filtersMap, ok := paramMap["filters"].(map[string]any); ok && len(filtersMap) > 0 {
				mergedFilters := make(map[string]any)
				if queryDef.Filters != nil {
					for k, v := range *queryDef.Filters {
						mergedFilters[k] = v
					}
				}
				for k, v := range filtersMap {
					mergedFilters[k] = v
				}
				wq.Filters = &mergedFilters
				hasOverrides = true
			}
			if fieldsSlice, ok := paramMap["fields"].([]any); ok && len(fieldsSlice) > 0 {
				if f, fErr := parameters.ConvertAnySliceToTyped(fieldsSlice, "string"); fErr == nil {
					fields := f.([]string)
					wq.Fields = &fields
					hasOverrides = true
				}
			}
			if pivotsSlice, ok := paramMap["pivots"].([]any); ok && len(pivotsSlice) > 0 {
				if p, pErr := parameters.ConvertAnySliceToTyped(pivotsSlice, "string"); pErr == nil {
					pivots := p.([]string)
					wq.Pivots = &pivots
					hasOverrides = true
				}
			}
			if modelVal, _ := paramMap["model"].(string); modelVal != "" {
				wq.Model = modelVal
				hasOverrides = true
			}
			if exploreVal, _ := paramMap["explore"].(string); exploreVal != "" {
				wq.View = exploreVal
				hasOverrides = true
			}
			if tzVal, _ := paramMap["tz"].(string); tzVal != "" {
				wq.QueryTimezone = &tzVal
				hasOverrides = true
			}
			if feVal, _ := paramMap["filter_expression"].(string); feVal != "" {
				wq.FilterExpression = &feVal
				hasOverrides = true
			}
			if dfSlice, ok := paramMap["dynamic_fields"].([]any); ok && len(dfSlice) > 0 {
				if jsonBytes, mErr := json.Marshal(dfSlice); mErr == nil {
					jsonStr := string(jsonBytes)
					wq.DynamicFields = &jsonStr
					hasOverrides = true
				}
			}
		}

		var resp string
		var rErr error

		var newSlug, newShareUrl, newExpandedShareUrl, newUrl string
		if wq != nil {
			if visConfigObj != nil {
				if m, ok := visConfigObj.(map[string]any); ok {
					mergedVisConfig := make(map[string]any)
					if queryDef != nil && queryDef.VisConfig != nil {
						for k, v := range *queryDef.VisConfig {
							mergedVisConfig[k] = v
						}
					}
					for k, v := range m {
						mergedVisConfig[k] = v
					}
					wq.VisConfig = &mergedVisConfig
					visConfigObj = mergedVisConfig
					hasOverrides = true
				}
			}

			// When parameters or vis_config are modified, save a new query in Looker to obtain an updated query ID, slug, and share URL
			if hasOverrides {
				createdQuery, cErr := sdk.CreateQuery(*wq, "id,slug,client_id,share_url,expanded_share_url,url", source.LookerApiSettings())
				if cErr == nil {
					// In Looker, Explore URLs require the 22-character client slug GUID (client_id) for ?qid= (stored in `slugs` table).
					if createdQuery.ClientId != nil && *createdQuery.ClientId != "" {
						newSlug = *createdQuery.ClientId
						queryIdVal = *createdQuery.ClientId
					} else if createdQuery.Slug != nil && *createdQuery.Slug != "" {
						newSlug = *createdQuery.Slug
						queryIdVal = *createdQuery.Slug
					} else if createdQuery.Id != nil {
						queryIdVal = *createdQuery.Id
					}
					if createdQuery.ShareUrl != nil {
						newShareUrl = *createdQuery.ShareUrl
					}
					if createdQuery.ExpandedShareUrl != nil {
						newExpandedShareUrl = *createdQuery.ExpandedShareUrl
					}
					if createdQuery.Url != nil {
						newUrl = *createdQuery.Url
					}
				} else {
					logger.WarnContext(ctx, "could not create query with overrides in looker", "error", cErr)
				}
			}

			resp, rErr = lookercommon.RunInlineQuery(ctx, sdk, wq, "json_detail", source.LookerApiSettings())
			if rErr != nil && !hasOverrides {
				logger.WarnContext(ctx, "error running inline query from query definition, falling back to saved query", "error", rErr)
				resp, rErr = lookercommon.RunSavedQuery(ctx, sdk, queryIdVal, "json_detail", source.LookerApiSettings())
			}
		} else {
			resp, rErr = lookercommon.RunSavedQuery(ctx, sdk, queryIdVal, "json_detail", source.LookerApiSettings())
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
		if wq != nil && wq.View != "" {
			title = wq.View
		} else if queryDef != nil && queryDef.View != "" {
			title = queryDef.View
		} else if exploreVal != "" {
			title = exploreVal
		}

		queryMeta := map[string]any{
			"id": queryIdVal,
		}
		if newSlug != "" {
			queryMeta["slug"] = newSlug
		}
		if newShareUrl != "" {
			queryMeta["share_url"] = newShareUrl
		}
		if newExpandedShareUrl != "" {
			queryMeta["expanded_share_url"] = newExpandedShareUrl
		}
		if modelVal != "" {
			queryMeta["model"] = modelVal
		}
		if exploreVal != "" {
			queryMeta["view"] = exploreVal
		}
		if wq != nil {
			if wq.Model != "" {
				queryMeta["model"] = wq.Model
			}
			if wq.View != "" {
				queryMeta["view"] = wq.View
			}
			if wq.Fields != nil {
				queryMeta["fields"] = *wq.Fields
			}
			if wq.Limit != nil && *wq.Limit != "" {
				queryMeta["limit"] = *wq.Limit
			}
			if wq.Sorts != nil && len(*wq.Sorts) > 0 {
				queryMeta["sorts"] = *wq.Sorts
			}
		}
		if queryDef != nil {
			if queryMeta["model"] == nil && queryDef.Model != "" {
				queryMeta["model"] = queryDef.Model
			}
			if queryMeta["view"] == nil && queryDef.View != "" {
				queryMeta["view"] = queryDef.View
			}
			if queryMeta["fields"] == nil && queryDef.Fields != nil {
				queryMeta["fields"] = *queryDef.Fields
			}
			if queryDef.Slug != nil && *queryDef.Slug != "" {
				queryMeta["slug"] = *queryDef.Slug
			}
			if queryDef.ClientId != nil && *queryDef.ClientId != "" {
				queryMeta["client_id"] = *queryDef.ClientId
				queryMeta["qid"] = *queryDef.ClientId
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

		if newSlug != "" {
			queryMeta["slug"] = newSlug
			queryMeta["client_id"] = newSlug
			queryMeta["qid"] = newSlug
			queryMeta["id"] = newSlug
		}
		if newShareUrl != "" {
			queryMeta["share_url"] = newShareUrl
		}
		if newExpandedShareUrl != "" {
			queryMeta["expanded_share_url"] = newExpandedShareUrl
		}
		if newUrl != "" {
			queryMeta["url"] = newUrl
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
			"visualizationData":  visualizationData,
			"data":               detailResp["data"],
			"fields":             detailResp["fields"],
			"pivots":             detailResp["pivots"],
			"vis_config":         visConfigObj,
			"query_id":           queryIdVal,
			"slug":               queryMeta["slug"],
			"client_id":          queryMeta["client_id"],
			"share_url":          queryMeta["share_url"],
			"expanded_share_url": queryMeta["expanded_share_url"],
		}, nil
	}

	// 2. Inline query path with json_detail
	wq, err := lookercommon.ProcessQueryArgs(ctx, params)
	if err != nil {
		return nil, util.NewAgentError("error building WriteQuery request", err)
	}
	if visConfigObj != nil {
		if m, ok := visConfigObj.(map[string]any); ok {
			wq.VisConfig = &m
		}
	}
	if escErr := lookercommon.EscapeUnquotedParameterFilters(ctx, sdk, wq, source.LookerApiSettings()); escErr != nil {
		logger.WarnContext(ctx, "skipping unquoted-parameter escape, metadata lookup failed", "error", escErr)
	}

	createdQuery, cErr := sdk.CreateQuery(*wq, "id,slug,client_id,share_url,expanded_share_url,url", source.LookerApiSettings())
	var queryId string
	var slug string
	var shareUrl string
	var expandedShareUrl string
	var url string
	if cErr == nil {
		// In Looker, Explore URLs require the 22-character client slug GUID (client_id) for ?qid= (stored in `slugs` table).
		if createdQuery.ClientId != nil && *createdQuery.ClientId != "" {
			slug = *createdQuery.ClientId
			queryId = *createdQuery.ClientId
		} else if createdQuery.Slug != nil && *createdQuery.Slug != "" {
			slug = *createdQuery.Slug
			queryId = *createdQuery.Slug
		} else if createdQuery.Id != nil {
			queryId = *createdQuery.Id
		}
		if createdQuery.ShareUrl != nil {
			shareUrl = *createdQuery.ShareUrl
		}
		if createdQuery.ExpandedShareUrl != nil {
			expandedShareUrl = *createdQuery.ExpandedShareUrl
		}
		if createdQuery.Url != nil {
			url = *createdQuery.Url
		}
	} else {
		logger.WarnContext(ctx, "could not create query in looker", "error", cErr)
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
			"id":                 queryId,
			"slug":               slug,
			"client_id":          slug,
			"qid":                slug,
			"share_url":          shareUrl,
			"expanded_share_url": expandedShareUrl,
			"url":                url,
			"model":              wq.Model,
			"view":               wq.View,
			"fields":             wq.Fields,
			"filters":            wq.Filters,
			"pivots":             wq.Pivots,
			"sorts":              wq.Sorts,
			"limit":              wq.Limit,
		},
	}

	return map[string]any{
		"visualizationData":  visualizationData,
		"data":               dataList,
		"fields":             fieldsObj,
		"model":              wq.Model,
		"explore":            wq.View,
		"vis_config":         visConfigObj,
		"query_id":           queryId,
		"slug":               slug,
		"client_id":          slug,
		"share_url":          shareUrl,
		"expanded_share_url": expandedShareUrl,
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
