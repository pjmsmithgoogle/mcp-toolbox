// Copyright 2025 Google LLC
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
package lookergetdashboards

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	yaml "github.com/goccy/go-yaml"
	"github.com/googleapis/mcp-toolbox/internal/sources"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"

	"github.com/googleapis/mcp-toolbox/internal/tools/looker/lookercommon"
	"github.com/looker-open-source/sdk-codegen/go/rtl"
	v4 "github.com/looker-open-source/sdk-codegen/go/sdk/v4"
)

const resourceType string = "looker-get-dashboards"

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

func (cfg Config) Initialize(context.Context) (tools.Tool, error) {
	if cfg.Description == "" {
		return nil, fmt.Errorf("description is required for tool %q", cfg.Name)
	}

	titleParameter := parameters.NewStringParameter("title", "The title of the dashboard.", parameters.WithStringDefault(""))
	descParameter := parameters.NewStringParameter("desc", "The description of the dashboard.", parameters.WithStringDefault(""))
	limitParameter := parameters.NewIntParameter("limit", "The number of dashboards to fetch. Default 100", parameters.WithIntDefault(100))
	offsetParameter := parameters.NewIntParameter("offset", "The number of dashboards to skip before fetching. Default 0", parameters.WithIntDefault(0))
	params := parameters.Parameters{
		titleParameter,
		descParameter,
		limitParameter,
		offsetParameter,
	}

	// finish tool setup
	return Tool{
		BaseTool: tools.NewBaseTool(
			cfg,
			lookercommon.ReadOnlyAnnotations(cfg.Annotations),
			tools.Manifest{Description: cfg.Description, Parameters: params.Manifest(), AuthRequired: cfg.AuthRequired},
			params,
		),
	}, nil
}

// validate interface
var _ tools.Tool = Tool{}

type Tool struct {
	tools.BaseTool[Config]
}

func (t Tool) GetSourceName() string {
	return t.Cfg.Source
}

func (t Tool) ToConfig() tools.ToolConfig {
	return t.Cfg
}

func (t Tool) ValidateSource(source sources.Source) error {
	_, ok := source.(compatibleSource)
	if !ok {
		return fmt.Errorf("invalid source for %q tool: source %q is not a compatible type", t.Cfg.Type, t.Cfg.Source)
	}
	return nil
}

func dashboardToMap(v v4.Dashboard) map[string]any {
	vMap := make(map[string]any)
	if v.Id != nil {
		vMap["id"] = *v.Id
	}
	if v.Title != nil {
		vMap["title"] = *v.Title
	}
	if v.Description != nil {
		vMap["description"] = *v.Description
	}
	if v.CertificationMetadata != nil {
		vMap["certification_metadata"] = v.CertificationMetadata
	}
	return vMap
}

func isNullFilterExpr(pattern string) bool {
	trimmed := strings.TrimSpace(pattern)
	return strings.EqualFold(trimmed, "IS NULL") || strings.EqualFold(trimmed, "NOT NULL")
}

// matchLookerPattern matches a string against a Looker search pattern
// (case-insensitive, supporting '%' and '_' SQL LIKE wildcards; if no '%'
// wildcard is specified, also matches case-insensitive substring so searching
// by partial title or LookML dashboard ID/slug works naturally).
func matchLookerPattern(value, pattern string) bool {
	if pattern == "" || pattern == "%" {
		return true
	}
	valLower := strings.ToLower(value)
	patLower := strings.ToLower(pattern)

	var sb strings.Builder
	sb.WriteString("(?s)^")
	for _, ch := range patLower {
		switch ch {
		case '%':
			sb.WriteString(".*")
		case '_':
			sb.WriteString(".")
		default:
			sb.WriteString(regexp.QuoteMeta(string(ch)))
		}
	}
	sb.WriteString("$")
	if matched, err := regexp.MatchString(sb.String(), valLower); err == nil && matched {
		return true
	}

	if !strings.Contains(patLower, "%") {
		return strings.Contains(valLower, patLower)
	}
	return false
}

func matchNullableLookerField(val *string, pattern string) bool {
	if pattern == "" {
		return true
	}
	trimmed := strings.TrimSpace(pattern)
	if strings.EqualFold(trimmed, "IS NULL") {
		return val == nil || *val == ""
	}
	if strings.EqualFold(trimmed, "NOT NULL") {
		return val != nil && *val != ""
	}
	if val == nil {
		return false
	}
	return matchLookerPattern(*val, pattern)
}

func matchesLookmlDashboard(v v4.Dashboard, titlePattern, descPattern string) bool {
	if titlePattern != "" {
		if isNullFilterExpr(titlePattern) {
			if !matchNullableLookerField(v.Title, titlePattern) {
				return false
			}
		} else if !matchNullableLookerField(v.Title, titlePattern) && !matchNullableLookerField(v.Id, titlePattern) {
			return false
		}
	}
	if descPattern != "" {
		if !matchNullableLookerField(v.Description, descPattern) {
			return false
		}
	}
	return true
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
	paramsMap := params.AsMap()
	title := paramsMap["title"].(string)
	title_ptr := &title
	if *title_ptr == "" {
		title_ptr = nil
	}
	desc := paramsMap["desc"].(string)
	desc_ptr := &desc
	if *desc_ptr == "" {
		desc_ptr = nil
	}
	limit := int64(paramsMap["limit"].(int))
	offset := int64(paramsMap["offset"].(int))

	sdk, err := source.GetLookerSDK(ctx, string(accessToken))
	if err != nil {
		return nil, util.NewClientServerError("error getting sdk", http.StatusInternalServerError, err)
	}
	fields := "id,title,description,certification_metadata"
	req := v4.RequestSearchDashboards{
		Title:       title_ptr,
		Description: desc_ptr,
		Limit:       &limit,
		Offset:      &offset,
		Fields:      &fields,
	}
	logger.DebugContext(ctx, "Making request %v", req)
	resp, err := sdk.SearchDashboards(req, source.LookerApiSettings())
	if err != nil {
		if strings.Contains(err.Error(), "status=401") {
			return nil, util.NewClientServerError("unauthorized error", http.StatusUnauthorized, err)
		}
		return nil, util.ProcessGeneralError(err)
	}
	logger.DebugContext(ctx, "Got response %v", resp)
	var data []any
	seenIDs := make(map[string]bool)
	for _, v := range resp {
		logger.DebugContext(ctx, "Got response element of %v\n", v)
		if v.Id != nil {
			seenIDs[*v.Id] = true
		}
		vMap := dashboardToMap(v)
		logger.DebugContext(ctx, "Converted to %v\n", vMap)
		data = append(data, vMap)
	}

	if limit <= 0 || int64(len(data)) < limit {
		lookmlResp, lookmlErr := lookercommon.SearchLookmlDashboards(sdk, map[string]any{"fields": fields}, source.LookerApiSettings())
		if lookmlErr != nil {
			logger.DebugContext(ctx, "error searching lookml dashboards: %v", lookmlErr)
		} else {
			for _, v := range lookmlResp {
				if v.Id != nil && seenIDs[*v.Id] {
					continue
				}
				if !matchesLookmlDashboard(v, title, desc) {
					continue
				}
				if v.Id != nil {
					seenIDs[*v.Id] = true
				}
				vMap := dashboardToMap(v)
				logger.DebugContext(ctx, "Converted LookML dashboard to %v\n", vMap)
				data = append(data, vMap)
				if limit > 0 && int64(len(data)) >= limit {
					break
				}
			}
		}
	}

	logger.DebugContext(ctx, "data = ", data)

	return data, nil
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
