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
package lookerrunquery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	yaml "github.com/goccy/go-yaml"
	"github.com/googleapis/mcp-toolbox/internal/sources"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/tools/looker/lookercommon"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"

	"github.com/looker-open-source/sdk-codegen/go/rtl"
	v4 "github.com/looker-open-source/sdk-codegen/go/sdk/v4"
)

const resourceType string = "looker-run-query"

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

	queryIdParameter := parameters.NewStringParameter(
		"query_id",
		"The unique identifier (numeric ID or slug) of the Looker query to run.",
	)
	resultFormatParameter := parameters.NewStringParameter(
		"result_format",
		"The output format: 'json_detail' (includes fields, pivots, and data) or 'json' (raw row array). Default is 'json_detail'.",
		parameters.WithStringDefault("json_detail"),
	)
	visConfigParameter := parameters.NewStringParameter(
		"vis_config",
		"Optional visualization configuration JSON string to associate with the query response.",
		parameters.WithStringDefault(""),
	)

	allParameters := parameters.Parameters{
		queryIdParameter,
		resultFormatParameter,
		visConfigParameter,
	}

	return Tool{
		BaseTool: tools.NewBaseTool(
			cfg,
			tools.GetAnnotationsOrDefault(cfg.Annotations, tools.NewReadOnlyAnnotations),
			tools.Manifest{Description: cfg.Description, Parameters: allParameters.Manifest(), AuthRequired: cfg.AuthRequired},
			allParameters,
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
	queryId, ok := paramsMap["query_id"].(string)
	if !ok || strings.TrimSpace(queryId) == "" {
		return nil, util.NewAgentError("query_id is required", nil)
	}
	queryId = strings.TrimSpace(queryId)

	resultFormat := "json_detail"
	if rf, ok := paramsMap["result_format"].(string); ok && strings.TrimSpace(rf) != "" {
		resultFormat = strings.TrimSpace(rf)
	}

	var visConfigObj any
	if vcStr, ok := paramsMap["vis_config"].(string); ok && strings.TrimSpace(vcStr) != "" {
		var parsed any
		if err := json.Unmarshal([]byte(vcStr), &parsed); err == nil {
			visConfigObj = parsed
		}
	}

	sdk, err := source.GetLookerSDK(ctx, string(accessToken))
	if err != nil {
		return nil, util.NewClientServerError("error getting sdk", http.StatusInternalServerError, err)
	}

	// 1. Attempt to run saved query by ID or slug via API endpoint
	resp, rErr := lookercommon.RunSavedQuery(ctx, sdk, queryId, resultFormat, source.LookerApiSettings())
	if rErr != nil {
		logger.WarnContext(ctx, "RunSavedQuery failed, attempting slug lookup", "query_id", queryId, "error", rErr)

		// 2. If saved query execution failed, check if queryId is a query slug
		slugQuery, qErr := sdk.QueryForSlug(queryId, "", source.LookerApiSettings())
		if qErr == nil {
			wq := v4.WriteQuery{
				Model:         slugQuery.Model,
				View:          slugQuery.View,
				Fields:        slugQuery.Fields,
				Pivots:        slugQuery.Pivots,
				Filters:       slugQuery.Filters,
				Sorts:         slugQuery.Sorts,
				QueryTimezone: slugQuery.QueryTimezone,
				Limit:         slugQuery.Limit,
			}
			resp, rErr = lookercommon.RunInlineQuery(ctx, sdk, &wq, resultFormat, source.LookerApiSettings())
		}
	}

	if rErr != nil {
		if strings.Contains(rErr.Error(), "status=401") {
			return nil, util.NewClientServerError("unauthorized error", http.StatusUnauthorized, rErr)
		}
		return nil, util.ProcessGeneralError(rErr)
	}

	if resultFormat == "json_detail" {
		var detailResp map[string]any
		if err := json.Unmarshal([]byte(resp), &detailResp); err != nil {
			return nil, util.NewClientServerError("error unmarshaling json_detail response", http.StatusInternalServerError, err)
		}

		result := map[string]any{
			"data":        detailResp["data"],
			"fields":      detailResp["fields"],
			"pivots":      detailResp["pivots"],
			"totals_data": detailResp["totals_data"],
			"query_id":    queryId,
			"status":      "success",
		}
		if visConfigObj != nil {
			result["vis_config"] = visConfigObj
		}
		return result, nil
	}

	var rawData []any
	if err := json.Unmarshal([]byte(resp), &rawData); err != nil {
		return nil, util.NewClientServerError("error unmarshaling json response", http.StatusInternalServerError, err)
	}

	result := map[string]any{
		"data":     rawData,
		"query_id": queryId,
		"status":   "success",
	}
	if visConfigObj != nil {
		result["vis_config"] = visConfigObj
	}
	return result, nil
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
