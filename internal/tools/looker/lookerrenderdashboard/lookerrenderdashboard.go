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
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	yaml "github.com/goccy/go-yaml"
	"github.com/googleapis/mcp-toolbox/internal/resources"
	"github.com/googleapis/mcp-toolbox/internal/sources"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/tools/looker/lookercommon"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
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

type compatibleSource = lookercommon.LookerUISource

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

	lookercommon.EnsureDefaultToolUIMetadata(&cfg.UI, defaultResourceName)

	allParameters := getDashboardParameters()

	uiMgr := lookercommon.NewLookerUIResourceManager(
		cfg.UI.Resource,
		defaultResourceURI,
		"Looker dashboard rendering interface",
		"render_dashboard",
		"render_visualization",
		"dashboard",
	)

	return Tool{
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
		uiMgr: uiMgr,
	}, nil
}

// validate interfaces
var (
	_ tools.Tool                 = Tool{}
	_ resources.ResourceProvider = Tool{}
)

type Tool struct {
	tools.BaseTool[Config]
	uiMgr *lookercommon.LookerUIResourceManager
}

func (t Tool) GetResources() []resources.Resource {
	return t.uiMgr.GetResources()
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
	t.uiMgr.SetSource(cs)
	return nil
}

func appendQueryFieldsAndView(qMap map[string]any, knownFields *[]string, viewHints *[]string) {
	if qMap == nil {
		return
	}
	if view, ok := qMap["view"].(string); ok && view != "" {
		*viewHints = append(*viewHints, view)
	}
	switch fields := qMap["fields"].(type) {
	case []string:
		*knownFields = append(*knownFields, fields...)
	case []any:
		for _, rawF := range fields {
			if f, ok := rawF.(string); ok && f != "" {
				*knownFields = append(*knownFields, f)
			}
		}
	}
}

func normalizeDashboardFilters(filtersMap map[string]any, rawDashboard map[string]any) map[string]any {
	if len(filtersMap) == 0 || rawDashboard == nil {
		return filtersMap
	}
	var knownFields []string
	var viewHints []string
	filterNamesBySpaced := make(map[string]string)

	if filters, ok := rawDashboard["dashboard_filters"].([]any); ok {
		for _, rawDf := range filters {
			dfMap, ok := rawDf.(map[string]any)
			if !ok {
				continue
			}
			if dim, ok := dfMap["dimension"].(string); ok && dim != "" {
				knownFields = append(knownFields, dim)
			}
			if name, ok := dfMap["name"].(string); ok && name != "" {
				filterNamesBySpaced[strings.ToLower(name)] = name
			}
			if title, ok := dfMap["title"].(string); ok && title != "" {
				filterNamesBySpaced[strings.ToLower(title)] = title
			}
		}
	}
	if elements, ok := rawDashboard["dashboard_elements"].([]any); ok {
		for _, rawEl := range elements {
			elMap, ok := rawEl.(map[string]any)
			if !ok {
				continue
			}
			if qMap, ok := elMap["query"].(map[string]any); ok {
				appendQueryFieldsAndView(qMap, &knownFields, &viewHints)
			}
			if rmMap, ok := elMap["result_maker"].(map[string]any); ok {
				if rmqMap, ok := rmMap["query"].(map[string]any); ok {
					appendQueryFieldsAndView(rmqMap, &knownFields, &viewHints)
				}
				if filterables, ok := rmMap["filterables"].([]any); ok {
					for _, rawFb := range filterables {
						fbMap, ok := rawFb.(map[string]any)
						if !ok {
							continue
						}
						if listens, ok := fbMap["listen"].([]any); ok {
							for _, rawL := range listens {
								lMap, ok := rawL.(map[string]any)
								if !ok {
									continue
								}
								if field, ok := lMap["field"].(string); ok && field != "" {
									knownFields = append(knownFields, field)
								}
								if dfName, ok := lMap["dashboard_filter_name"].(string); ok && dfName != "" {
									filterNamesBySpaced[strings.ToLower(dfName)] = dfName
								}
							}
						}
					}
				}
			}
		}
	}

	out := make(map[string]any, len(filtersMap))
	toNormalize := make(map[string]any)
	for k, v := range filtersMap {
		trimmed := strings.TrimSpace(k)
		if canonical, ok := filterNamesBySpaced[strings.ToLower(trimmed)]; ok {
			out[canonical] = v
			continue
		}
		spaced := strings.ReplaceAll(trimmed, "_", " ")
		if canonical, ok := filterNamesBySpaced[strings.ToLower(spaced)]; ok {
			out[canonical] = v
			continue
		}
		toNormalize[k] = v
	}
	if len(toNormalize) > 0 {
		normalized := lookercommon.NormalizeFilterKeys(toNormalize, knownFields, viewHints...)
		for k, v := range normalized {
			out[k] = v
		}
	}
	return out
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

	var (
		appState   map[string]any
		appStateWg sync.WaitGroup
	)
	appStateWg.Add(1)
	go func() {
		defer appStateWg.Done()
		appState = lookercommon.FetchMcpAppState(ctx, sdk, source.LookerApiSettings())
	}()

	var rawDashboard map[string]any
	err = sdk.AuthSession.Do(&rawDashboard, "GET", "/4.0", fmt.Sprintf("/dashboards/%s", url.PathEscape(dashboardId)), nil, nil, source.LookerApiSettings())
	if err != nil {
		if strings.Contains(err.Error(), "status=401") {
			return nil, util.NewClientServerError("unauthorized error", http.StatusUnauthorized, err)
		}
		return nil, util.ProcessGeneralError(lookercommon.ExtractLookerErrorMessage(err))
	}

	filtersMap = normalizeDashboardFilters(filtersMap, rawDashboard)

	dashTitle, _ := rawDashboard["title"].(string)
	var tileCount int
	if els, ok := rawDashboard["dashboard_elements"].([]any); ok {
		tileCount = len(els)
	}
	var filterCount int
	if flts, ok := rawDashboard["dashboard_filters"].([]any); ok {
		filterCount = len(flts)
	}

	dashboardPayload := PruneDashboardMap(rawDashboard)

	appStateWg.Wait()

	dashboardData := map[string]any{
		"dashboard": dashboardPayload,
		"filters":   filtersMap,
	}

	logger.DebugContext(ctx, "dashboardData prepared", "dashboard_id", dashboardId)

	resultPayload := map[string]any{
		"dashboardData": dashboardData,
		"dashboard_id":  dashboardId,
	}
	if appState != nil {
		resultPayload["app_state"] = appState
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
	delete(qMap, "filter_config")
	delete(qMap, "runtime")
	delete(qMap, "expanded_share_url")
	delete(qMap, "url")
	if vcMap, ok := qMap["vis_config"].(map[string]any); ok {
		delete(vcMap, "query_fields")
	}
}

// PruneDashboardMap strips bulky, unused Looker API metadata (such as
// explore UI filter_config blobs, expanded_share_url/url, LookML SQL
// definitions, drill_fields, and duplicate nested query objects) in-place from
// a dashboard map so large multi-tile dashboards stay well within MCP host tool
// response limits while preserving object-level `can` permission maps needed by
// the UI (e.g. `can.explore`).
func PruneDashboardMap(dashMap map[string]any) map[string]any {
	if dashMap == nil {
		return nil
	}

	if elements, ok := dashMap["dashboard_elements"].([]any); ok {
		for _, rawEl := range elements {
			elMap, ok := rawEl.(map[string]any)
			if !ok {
				continue
			}

			if rmMap, ok := elMap["result_maker"].(map[string]any); ok {
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
