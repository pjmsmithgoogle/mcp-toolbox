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
package lookerquery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
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

const resourceType string = "looker-query"

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

func getLookerQueryParameters() parameters.Parameters {
	queryIdParameter := parameters.NewStringParameter(
		"query_id",
		"Optional unique identifier (numeric ID or slug) of a saved Looker query to run. If provided, model, explore, and fields are not required.",
		parameters.WithStringRequired(false),
	)
	mergeResultIdParameter := parameters.NewStringParameter(
		"merge_result_id",
		"Optional unique identifier of a Looker merged query (merge_result_id) to run. If provided, query_id, model, explore, and fields are not required.",
		parameters.WithStringRequired(false),
	)
	modelParameter := parameters.NewStringParameter(
		"model",
		"The model containing the explore (required if query_id and merge_result_id are not provided).",
		parameters.WithStringRequired(false),
	)
	exploreParameter := parameters.NewStringParameter(
		"explore",
		"The explore to be queried (required if query_id and merge_result_id are not provided).",
		parameters.WithStringRequired(false),
	)
	fieldsParameter := parameters.NewArrayParameter(
		"fields",
		"The fields to be retrieved (required if query_id and merge_result_id are not provided).",
		parameters.NewStringParameter("field", "A field to be returned in the query"),
		parameters.WithArrayRequired(false),
	)

	commonParams := lookercommon.GetQueryParameters()

	resultFormatParameter := parameters.NewStringParameter(
		"result_format",
		"The output format: 'json' (raw row array) or 'json_detail' (includes fields, pivots, and data). Default is 'json'.",
		parameters.WithStringDefault("json"),
	)
	visConfigParameter := parameters.NewStringParameter(
		"vis_config",
		"Optional visualization configuration JSON string to associate with the query response (e.g. {\"type\": \"looker_column\"} for vertical columns or {\"type\": \"looker_bar\"} for horizontal bars).",
		parameters.WithStringDefault(""),
	)
	generateDrillLinksParameter := parameters.NewBooleanParameter(
		"generate_drill_links",
		"Optional flag indicating whether to generate drill links in the query result.",
		parameters.WithBooleanRequired(false),
	)

	allParams := parameters.Parameters{
		queryIdParameter,
		mergeResultIdParameter,
		modelParameter,
		exploreParameter,
		fieldsParameter,
	}
	// Append filters, pivots, sorts, limit, tz, filter_expression, dynamic_fields from commonParams
	if len(commonParams) > 3 {
		allParams = append(allParams, commonParams[3:]...)
	}
	allParams = append(
		allParams,
		resultFormatParameter,
		visConfigParameter,
		generateDrillLinksParameter,
	)
	return allParams
}

func (cfg Config) Initialize(context.Context) (tools.Tool, error) {
	if cfg.Description == "" {
		return nil, fmt.Errorf("description is required for tool %q", cfg.Name)
	}

	allParameters := getLookerQueryParameters()

	// finish tool setup
	return Tool{
		BaseTool: tools.NewBaseTool(
			cfg,
			lookercommon.ReadOnlyAnnotations(cfg.Annotations),
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

// BuildWriteQueryWithOverrides creates a v4.WriteQuery from a base v4.Query,
// merging any provided filterOverrides into the query's base filters and
// applying sortOverrides if non-empty. FilterConfig and ClientId are intentionally
// omitted so Looker does not allow explore UI filter_config to override Filters.
func BuildWriteQueryWithOverrides(baseQuery v4.Query, filterOverrides map[string]any, sortOverrides []string) v4.WriteQuery {
	var knownFields []string
	if baseQuery.Fields != nil {
		knownFields = append(knownFields, *baseQuery.Fields...)
	}
	if baseQuery.Filters != nil {
		for k := range *baseQuery.Filters {
			knownFields = append(knownFields, k)
		}
	}
	normalizedOverrides := lookercommon.NormalizeFilterKeys(filterOverrides, knownFields, baseQuery.View)

	mergedFilters := make(map[string]any)
	if baseQuery.Filters != nil {
		for k, v := range *baseQuery.Filters {
			mergedFilters[k] = v
		}
	}
	for k, v := range normalizedOverrides {
		mergedFilters[k] = v
	}

	var sortsPtr *[]string
	if len(sortOverrides) > 0 {
		sCopy := make([]string, len(sortOverrides))
		copy(sCopy, sortOverrides)
		sortsPtr = &sCopy
	} else {
		sortsPtr = baseQuery.Sorts
	}

	return v4.WriteQuery{
		Model:            baseQuery.Model,
		View:             baseQuery.View,
		Fields:           baseQuery.Fields,
		Pivots:           baseQuery.Pivots,
		FillFields:       baseQuery.FillFields,
		Filters:          &mergedFilters,
		FilterExpression: baseQuery.FilterExpression,
		Sorts:            sortsPtr,
		Limit:            baseQuery.Limit,
		ColumnLimit:      baseQuery.ColumnLimit,
		Total:            baseQuery.Total,
		RowTotal:         baseQuery.RowTotal,
		Subtotals:        baseQuery.Subtotals,
		VisConfig:        baseQuery.VisConfig,
		DynamicFields:    baseQuery.DynamicFields,
		QueryTimezone:    baseQuery.QueryTimezone,
	}
}

func sortsMatchBaseQuery(sortOverrides []string, baseSorts *[]string) bool {
	if len(sortOverrides) == 0 {
		return true
	}
	if baseSorts == nil || len(*baseSorts) != len(sortOverrides) {
		return false
	}
	for i := range sortOverrides {
		if strings.TrimSpace(sortOverrides[i]) != strings.TrimSpace((*baseSorts)[i]) {
			return false
		}
	}
	return true
}

func applySavedQueryPayloadLimit(result map[string]any) tools.StructuredToolResult {
	if _, tooLarge := lookercommon.ExceedsMCPPayloadLimit(result, lookercommon.MaxMCPPayloadBytes); tooLarge {
		result["data"] = []any{}
		delete(result, "totals_data")
		delete(result, "pivots")
		result["status"] = "error"
		result["response_too_large"] = true
		result["error"] = "This query returned too much data to display here. Open it in Looker Explore to view the full result."
		return tools.StructuredToolResult{
			Content:           result,
			StructuredContent: result,
		}
	}

	var rowCount int
	if rows, ok := result["data"].([]any); ok {
		rowCount = len(rows)
	}
	summary := map[string]any{
		"query_id":  result["query_id"],
		"row_count": rowCount,
		"status":    result["status"],
	}
	if mrID, ok := result["merge_result_id"]; ok && mrID != nil {
		summary["merge_result_id"] = mrID
	}
	return tools.StructuredToolResult{
		Content:           summary,
		StructuredContent: result,
	}
}

func extractCellJoinKey(row map[string]any, field string) string {
	val, ok := row[field]
	if !ok || val == nil {
		return ""
	}
	if cellMap, ok := val.(map[string]any); ok {
		if v, exists := cellMap["value"]; exists {
			if v == nil {
				return ""
			}
			return fmt.Sprintf("%v", v)
		}
	}
	return fmt.Sprintf("%v", val)
}

func parseSortSpec(spec string) (field string, desc bool) {
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" {
		return "", false
	}
	parts := strings.Fields(trimmed)
	field = parts[0]
	for _, p := range parts[1:] {
		if strings.EqualFold(p, "desc") {
			desc = true
		}
	}
	return field, desc
}

func filterSortsForSourceQuery(baseQuery v4.Query, sortOverrides []string) []string {
	if len(sortOverrides) == 0 || baseQuery.Fields == nil {
		return nil
	}
	known := make(map[string]bool, len(*baseQuery.Fields))
	for _, f := range *baseQuery.Fields {
		known[strings.ToLower(strings.TrimSpace(f))] = true
	}
	var matched []string
	for _, s := range sortOverrides {
		field, _ := parseSortSpec(s)
		if field != "" && known[strings.ToLower(field)] {
			matched = append(matched, s)
		}
	}
	return matched
}

func extractRawCellValue(row map[string]any, field string) any {
	val, ok := row[field]
	if !ok || val == nil {
		return nil
	}
	if cellMap, ok := val.(map[string]any); ok {
		if v, exists := cellMap["value"]; exists {
			return v
		}
	}
	return val
}

func toFloat64(val any) (float64, bool) {
	switch v := val.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(trimmed, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func compareMergedCellValues(a, b any) int {
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		return -1
	}
	if b == nil {
		return 1
	}
	if fa, okA := toFloat64(a); okA {
		if fb, okB := toFloat64(b); okB {
			if fa < fb {
				return -1
			}
			if fa > fb {
				return 1
			}
			return 0
		}
	}
	sa := fmt.Sprintf("%v", a)
	sb := fmt.Sprintf("%v", b)
	if sa < sb {
		return -1
	}
	if sa > sb {
		return 1
	}
	return 0
}

func sortMergedRows(rows []any, sortOverrides []string) {
	if len(rows) <= 1 || len(sortOverrides) == 0 {
		return
	}
	type parsedSort struct {
		field string
		desc  bool
	}
	var specs []parsedSort
	for _, s := range sortOverrides {
		f, desc := parseSortSpec(s)
		if f != "" {
			specs = append(specs, parsedSort{field: f, desc: desc})
		}
	}
	if len(specs) == 0 {
		return
	}
	sort.SliceStable(rows, func(i, j int) bool {
		rowA, okA := rows[i].(map[string]any)
		rowB, okB := rows[j].(map[string]any)
		if !okA || !okB {
			return false
		}
		for _, sp := range specs {
			valA := extractRawCellValue(rowA, sp.field)
			valB := extractRawCellValue(rowB, sp.field)
			cmp := compareMergedCellValues(valA, valB)
			if cmp != 0 {
				if sp.desc {
					return cmp > 0
				}
				return cmp < 0
			}
		}
		return false
	})
}

func filterOverridesForSourceQuery(baseQuery v4.Query, mergeFields *[]v4.MergeFields, filterOverrides map[string]any, totalSourceQueries int) map[string]any {
	if len(filterOverrides) == 0 {
		return nil
	}
	var knownFields []string
	knownViews := make(map[string]bool)
	if baseQuery.View != "" {
		knownViews[strings.ToLower(baseQuery.View)] = true
	}
	if baseQuery.Fields != nil {
		for _, f := range *baseQuery.Fields {
			knownFields = append(knownFields, f)
			if dot := strings.IndexByte(f, '.'); dot > 0 {
				knownViews[strings.ToLower(f[:dot])] = true
			}
		}
	}
	if baseQuery.Filters != nil {
		for f := range *baseQuery.Filters {
			knownFields = append(knownFields, f)
			if dot := strings.IndexByte(f, '.'); dot > 0 {
				knownViews[strings.ToLower(f[:dot])] = true
			}
		}
	}
	if mergeFields != nil {
		for _, mf := range *mergeFields {
			if mf.SourceFieldName != nil && *mf.SourceFieldName != "" {
				knownFields = append(knownFields, *mf.SourceFieldName)
				if dot := strings.IndexByte(*mf.SourceFieldName, '.'); dot > 0 {
					knownViews[strings.ToLower((*mf.SourceFieldName)[:dot])] = true
				}
			}
		}
	}

	normalized := lookercommon.NormalizeFilterKeys(filterOverrides, knownFields, baseQuery.View)
	if totalSourceQueries <= 1 {
		return normalized
	}
	matched := make(map[string]any)
	for k, v := range normalized {
		if dot := strings.IndexByte(k, '.'); dot > 0 {
			viewPrefix := strings.ToLower(k[:dot])
			if knownViews[viewPrefix] {
				matched[k] = v
			}
		} else {
			matched[k] = v
		}
	}
	return matched
}

func mergeFieldsMetadata(dstFields, srcFields map[string]any) map[string]any {
	if dstFields == nil {
		return srcFields
	}
	if srcFields == nil {
		return dstFields
	}
	for _, section := range []string{"dimensions", "measures", "table_calculations", "pivots"} {
		srcList, ok := srcFields[section].([]any)
		if !ok || len(srcList) == 0 {
			continue
		}
		dstList, _ := dstFields[section].([]any)
		seen := make(map[string]bool, len(dstList))
		for _, item := range dstList {
			if m, ok := item.(map[string]any); ok {
				if name, ok := m["name"].(string); ok && name != "" {
					seen[name] = true
				}
			}
		}
		for _, item := range srcList {
			if m, ok := item.(map[string]any); ok {
				if name, ok := m["name"].(string); ok && name != "" {
					if seen[name] {
						continue
					}
					seen[name] = true
				}
			}
			dstList = append(dstList, item)
		}
		dstFields[section] = dstList
	}
	return dstFields
}

func runMergeQuery(
	ctx context.Context,
	sdk *v4.LookerSDK,
	mergeResultId string,
	resultFormat string,
	filterOverrides map[string]any,
	sortOverrides []string,
	visConfigObj any,
	generateDrillLinks *bool,
	settings *rtl.ApiSettings,
) (any, util.ToolboxError) {
	logger, _ := util.LoggerFromContext(ctx)

	mq, err := sdk.MergeQuery(mergeResultId, "", settings)
	if err != nil || mq.SourceQueries == nil || len(*mq.SourceQueries) == 0 {
		var directResp string
		directPath := fmt.Sprintf("/merge_queries/%s/run/%s", url.PathEscape(mergeResultId), url.PathEscape(resultFormat))
		if dErr := sdk.AuthSession.Do(&directResp, "GET", "/4.0", directPath, nil, nil, settings); dErr == nil && directResp != "" {
			if resultFormat == "json_detail" {
				var detailResp map[string]any
				if uErr := json.Unmarshal([]byte(directResp), &detailResp); uErr != nil {
					return nil, util.NewClientServerError("error unmarshaling json_detail response", http.StatusInternalServerError, uErr)
				}
				if generateDrillLinks != nil && !*generateDrillLinks {
					lookercommon.StripDrillLinks(detailResp)
				}
				result := map[string]any{
					"data":            detailResp["data"],
					"fields":          detailResp["fields"],
					"pivots":          detailResp["pivots"],
					"totals_data":     detailResp["totals_data"],
					"query_id":        mergeResultId,
					"merge_result_id": mergeResultId,
					"status":          "success",
				}
				if visConfigObj != nil {
					result["vis_config"] = visConfigObj
				}
				return applySavedQueryPayloadLimit(result), nil
			}
			var rawData []any
			if uErr := json.Unmarshal([]byte(directResp), &rawData); uErr != nil {
				return nil, util.NewClientServerError("error unmarshaling query response", http.StatusInternalServerError, uErr)
			}
			if visConfigObj != nil {
				result := map[string]any{
					"data":            rawData,
					"query_id":        mergeResultId,
					"merge_result_id": mergeResultId,
					"status":          "success",
					"vis_config":      visConfigObj,
				}
				return applySavedQueryPayloadLimit(result), nil
			}
			return rawData, nil
		}
		if err != nil {
			if strings.Contains(err.Error(), "status=401") {
				return nil, util.NewClientServerError("unauthorized error", http.StatusUnauthorized, err)
			}
			return nil, util.ProcessGeneralError(lookercommon.ExtractLookerErrorMessage(err))
		}
		return nil, util.NewAgentError(fmt.Sprintf("merged query %q has no source_queries", mergeResultId), nil)
	}

	sourceQueries := *mq.SourceQueries
	type sourceResult struct {
		rows       []any
		detailResp map[string]any
		mergeSpecs []v4.MergeFields
	}
	results := make([]sourceResult, 0, len(sourceQueries))

	for idx, sq := range sourceQueries {
		var baseQuery v4.Query
		var qErr error
		if sq.QueryId != nil && *sq.QueryId != "" {
			baseQuery, qErr = sdk.Query(*sq.QueryId, "", settings)
			if qErr != nil {
				baseQuery, qErr = sdk.QueryForSlug(*sq.QueryId, "", settings)
			}
		} else if sq.QuerySlug != nil && *sq.QuerySlug != "" {
			baseQuery, qErr = sdk.QueryForSlug(*sq.QuerySlug, "", settings)
			if qErr != nil {
				baseQuery, qErr = sdk.Query(*sq.QuerySlug, "", settings)
			}
		} else {
			return nil, util.NewAgentError(fmt.Sprintf("source query %d in merged query %q has neither query_id nor query_slug", idx, mergeResultId), nil)
		}
		if qErr != nil {
			if strings.Contains(qErr.Error(), "status=401") {
				return nil, util.NewClientServerError("unauthorized error", http.StatusUnauthorized, qErr)
			}
			return nil, util.ProcessGeneralError(lookercommon.ExtractLookerErrorMessage(qErr))
		}

		sqFilters := filterOverridesForSourceQuery(baseQuery, sq.MergeFields, filterOverrides, len(sourceQueries))
		var sqSorts []string
		if idx == 0 && len(sortOverrides) > 0 {
			sqSorts = filterSortsForSourceQuery(baseQuery, sortOverrides)
		}
		wq := BuildWriteQueryWithOverrides(baseQuery, sqFilters, sqSorts)
		if escErr := lookercommon.EscapeUnquotedParameterFilters(ctx, sdk, &wq, settings); escErr != nil && logger != nil {
			logger.WarnContext(ctx, "skipping unquoted-parameter escape, metadata lookup failed", "error", escErr)
		}

		rawResp, rErr := lookercommon.RunInlineQueryWithDrillLinks(ctx, sdk, &wq, resultFormat, generateDrillLinks, settings)
		if rErr != nil {
			if strings.Contains(rErr.Error(), "status=401") {
				return nil, util.NewClientServerError("unauthorized error", http.StatusUnauthorized, rErr)
			}
			return nil, util.ProcessGeneralError(lookercommon.ExtractLookerErrorMessage(rErr))
		}

		var sr sourceResult
		if sq.MergeFields != nil {
			sr.mergeSpecs = *sq.MergeFields
		}
		if resultFormat == "json_detail" {
			var detail map[string]any
			if uErr := json.Unmarshal([]byte(rawResp), &detail); uErr != nil {
				return nil, util.NewClientServerError("error unmarshaling json_detail response for merged query", http.StatusInternalServerError, uErr)
			}
			if generateDrillLinks != nil && !*generateDrillLinks {
				lookercommon.StripDrillLinks(detail)
			}
			sr.detailResp = detail
			if rows, ok := detail["data"].([]any); ok {
				sr.rows = rows
			}
		} else {
			var rows []any
			if uErr := json.Unmarshal([]byte(rawResp), &rows); uErr != nil {
				return nil, util.NewClientServerError("error unmarshaling json response for merged query", http.StatusInternalServerError, uErr)
			}
			sr.rows = rows
		}
		results = append(results, sr)
	}

	// Merge secondary source query rows into primary source query rows (left join on merge_fields)
	primaryRows := results[0].rows
	for i := 1; i < len(results); i++ {
		sec := results[i]
		specs := sec.mergeSpecs
		if len(specs) == 0 {
			continue
		}
		secIndex := make(map[string]map[string]any, len(sec.rows))
		for _, r := range sec.rows {
			rMap, ok := r.(map[string]any)
			if !ok {
				continue
			}
			var keyParts []string
			for _, mf := range specs {
				if mf.SourceFieldName != nil {
					keyParts = append(keyParts, extractCellJoinKey(rMap, *mf.SourceFieldName))
				}
			}
			compositeKey := strings.Join(keyParts, "\x00")
			if _, exists := secIndex[compositeKey]; !exists {
				secIndex[compositeKey] = rMap
			}
		}

		for _, pr := range primaryRows {
			pMap, ok := pr.(map[string]any)
			if !ok {
				continue
			}
			var keyParts []string
			for _, mf := range specs {
				if mf.FieldName != nil {
					keyParts = append(keyParts, extractCellJoinKey(pMap, *mf.FieldName))
				}
			}
			compositeKey := strings.Join(keyParts, "\x00")
			if secRow, matched := secIndex[compositeKey]; matched {
				for k, v := range secRow {
					if _, exists := pMap[k]; !exists {
						pMap[k] = v
					}
				}
			}
		}
	}

	if len(sortOverrides) > 0 {
		sortMergedRows(primaryRows, sortOverrides)
	}

	if visConfigObj == nil && mq.VisConfig != nil && len(*mq.VisConfig) > 0 {
		visConfigObj = *mq.VisConfig
	}

	if resultFormat == "json_detail" {
		primaryDetail := results[0].detailResp
		mergedFields, _ := primaryDetail["fields"].(map[string]any)
		mergedPivots := primaryDetail["pivots"]
		for i := 1; i < len(results); i++ {
			secFields, _ := results[i].detailResp["fields"].(map[string]any)
			mergedFields = mergeFieldsMetadata(mergedFields, secFields)
			if mergedPivots == nil && results[i].detailResp["pivots"] != nil {
				mergedPivots = results[i].detailResp["pivots"]
			}
		}
		result := map[string]any{
			"data":            primaryRows,
			"fields":          mergedFields,
			"pivots":          mergedPivots,
			"totals_data":     primaryDetail["totals_data"],
			"query_id":        mergeResultId,
			"merge_result_id": mergeResultId,
			"status":          "success",
		}
		if visConfigObj != nil {
			result["vis_config"] = visConfigObj
		}
		return applySavedQueryPayloadLimit(result), nil
	}

	if visConfigObj != nil {
		result := map[string]any{
			"data":            primaryRows,
			"query_id":        mergeResultId,
			"merge_result_id": mergeResultId,
			"status":          "success",
			"vis_config":      visConfigObj,
		}
		return applySavedQueryPayloadLimit(result), nil
	}
	return primaryRows, nil
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
	queryId, _ := paramsMap["query_id"].(string)
	queryId = strings.TrimSpace(queryId)
	mergeResultId, _ := paramsMap["merge_result_id"].(string)
	mergeResultId = strings.TrimSpace(mergeResultId)

	resultFormat := "json"
	if rf, ok := paramsMap["result_format"].(string); ok && strings.TrimSpace(rf) != "" {
		resultFormat = strings.TrimSpace(rf)
	}

	var visConfigObj any
	if vcStr, ok := paramsMap["vis_config"].(string); ok && strings.TrimSpace(vcStr) != "" {
		var parsed any
		if err := json.Unmarshal([]byte(vcStr), &parsed); err != nil {
			return nil, util.NewAgentError("invalid vis_config JSON", err)
		}
		visConfigObj = parsed
	}

	var generateDrillLinks *bool
	if gdl, ok := paramsMap["generate_drill_links"].(bool); ok {
		generateDrillLinks = &gdl
	}

	var filterOverrides map[string]any
	if fMap, ok := paramsMap["filters"].(map[string]any); ok && len(fMap) > 0 {
		filterOverrides = make(map[string]any, len(fMap))
		for k, v := range fMap {
			newKey := k
			if len(k) >= 2 && (k[0] == '\'' || k[0] == '"') && k[0] == k[len(k)-1] {
				newKey = k[1 : len(k)-1]
			}
			newVal := v
			if s, ok := v.(string); ok && len(s) >= 2 &&
				(s[0] == '\'' || s[0] == '"') && s[0] == s[len(s)-1] {
				newVal = s[1 : len(s)-1]
			}
			filterOverrides[newKey] = newVal
		}
	}

	var sortOverrides []string
	if sSlice, ok := paramsMap["sorts"].([]any); ok && len(sSlice) > 0 {
		converted, err := parameters.ConvertAnySliceToTyped(sSlice, "string")
		if err != nil {
			return nil, util.NewAgentError("can't convert sorts to array of strings", err)
		}
		sortOverrides = converted.([]string)
	}

	sdk, err := source.GetLookerSDK(ctx, string(accessToken))
	if err != nil {
		return nil, util.NewClientServerError("error getting sdk", http.StatusInternalServerError, err)
	}

	if mergeResultId != "" {
		return runMergeQuery(ctx, sdk, mergeResultId, resultFormat, filterOverrides, sortOverrides, visConfigObj, generateDrillLinks, source.LookerApiSettings())
	}

	if queryId != "" {
		hasOverrides := len(filterOverrides) > 0 || len(sortOverrides) > 0

		var resp string
		var rErr error

		if hasOverrides {
			baseQuery, qErr := sdk.Query(queryId, "", source.LookerApiSettings())
			if qErr != nil {
				logger.DebugContext(ctx, "sdk.Query failed, attempting QueryForSlug", "query_id", queryId, "error", qErr)
				baseQuery, qErr = sdk.QueryForSlug(queryId, "", source.LookerApiSettings())
			}
			if qErr != nil {
				if strings.Contains(qErr.Error(), "status=401") {
					return nil, util.NewClientServerError("unauthorized error", http.StatusUnauthorized, qErr)
				}
				return nil, util.ProcessGeneralError(lookercommon.ExtractLookerErrorMessage(qErr))
			}

			if len(filterOverrides) == 0 && sortsMatchBaseQuery(sortOverrides, baseQuery.Sorts) {
				resp, rErr = lookercommon.RunSavedQueryWithDrillLinks(ctx, sdk, queryId, resultFormat, generateDrillLinks, source.LookerApiSettings())
				if rErr != nil {
					wq := BuildWriteQueryWithOverrides(baseQuery, nil, nil)
					resp, rErr = lookercommon.RunInlineQueryWithDrillLinks(ctx, sdk, &wq, resultFormat, generateDrillLinks, source.LookerApiSettings())
				}
			} else {
				wq := BuildWriteQueryWithOverrides(baseQuery, filterOverrides, sortOverrides)
				if escErr := lookercommon.EscapeUnquotedParameterFilters(ctx, sdk, &wq, source.LookerApiSettings()); escErr != nil {
					logger.WarnContext(ctx, "skipping unquoted-parameter escape, metadata lookup failed", "error", escErr)
				}
				resp, rErr = lookercommon.RunInlineQueryWithDrillLinks(ctx, sdk, &wq, resultFormat, generateDrillLinks, source.LookerApiSettings())
			}
		} else {
			resp, rErr = lookercommon.RunSavedQueryWithDrillLinks(ctx, sdk, queryId, resultFormat, generateDrillLinks, source.LookerApiSettings())
			if rErr != nil {
				logger.WarnContext(ctx, "RunSavedQuery failed, attempting slug lookup", "query_id", queryId, "error", rErr)

				slugQuery, qErr := sdk.QueryForSlug(queryId, "", source.LookerApiSettings())
				if qErr == nil {
					wq := BuildWriteQueryWithOverrides(slugQuery, nil, nil)
					resp, rErr = lookercommon.RunInlineQueryWithDrillLinks(ctx, sdk, &wq, resultFormat, generateDrillLinks, source.LookerApiSettings())
				}
			}
		}

		if rErr != nil {
			if strings.Contains(rErr.Error(), "status=401") {
				return nil, util.NewClientServerError("unauthorized error", http.StatusUnauthorized, rErr)
			}
			return nil, util.ProcessGeneralError(lookercommon.ExtractLookerErrorMessage(rErr))
		}

		if resultFormat == "json_detail" {
			var detailResp map[string]any
			if err := json.Unmarshal([]byte(resp), &detailResp); err != nil {
				return nil, util.NewClientServerError("error unmarshaling json_detail response", http.StatusInternalServerError, err)
			}
			if generateDrillLinks != nil && !*generateDrillLinks {
				lookercommon.StripDrillLinks(detailResp)
			}

			result := map[string]any{
				"data":        detailResp["data"],
				"fields":      detailResp["fields"],
				"pivots":      detailResp["pivots"],
				"totals_data": detailResp["totals_data"],
				"query_id":    queryId,
				"status":      "success",
			}
			if errs, ok := detailResp["errors"]; ok && errs != nil {
				result["errors"] = errs
			}
			if visConfigObj != nil {
				result["vis_config"] = visConfigObj
			}
			return applySavedQueryPayloadLimit(result), nil
		}

		var rawData []any
		if err := json.Unmarshal([]byte(resp), &rawData); err != nil {
			return nil, util.NewClientServerError("error unmarshaling query response", http.StatusInternalServerError, err)
		}
		if visConfigObj != nil {
			result := map[string]any{
				"data":       rawData,
				"query_id":   queryId,
				"status":     "success",
				"vis_config": visConfigObj,
			}
			return applySavedQueryPayloadLimit(result), nil
		}
		if _, tooLarge := lookercommon.ExceedsMCPPayloadLimit(rawData, lookercommon.MaxMCPPayloadBytes); tooLarge {
			return applySavedQueryPayloadLimit(map[string]any{
				"data":     rawData,
				"query_id": queryId,
				"status":   "success",
			}), nil
		}
		return rawData, nil
	}

	// Inline query path (query_id and merge_result_id are empty)
	modelVal, _ := paramsMap["model"].(string)
	if strings.TrimSpace(modelVal) == "" {
		return nil, util.NewAgentError("model is required when query_id is not provided; use get_models to discover available models and retry", nil)
	}
	exploreVal, _ := paramsMap["explore"].(string)
	if strings.TrimSpace(exploreVal) == "" {
		return nil, util.NewAgentError("explore is required when query_id is not provided; use get_explores to discover available explores and retry", nil)
	}
	fieldsSlice, _ := paramsMap["fields"].([]any)
	if len(fieldsSlice) == 0 {
		return nil, util.NewAgentError("fields is required when query_id is not provided; use get_dimensions and get_measures to discover available fields and retry", nil)
	}

	wq, err := lookercommon.ProcessQueryArgs(ctx, params)
	if err != nil {
		return nil, util.NewAgentError("error building WriteQuery request", err)
	}
	if escErr := lookercommon.EscapeUnquotedParameterFilters(ctx, sdk, wq, source.LookerApiSettings()); escErr != nil {
		logger.WarnContext(ctx, "skipping unquoted-parameter escape, metadata lookup failed", "error", escErr)
	}
	resp, err := lookercommon.RunInlineQueryWithDrillLinks(ctx, sdk, wq, resultFormat, generateDrillLinks, source.LookerApiSettings())
	if err != nil {
		if strings.Contains(err.Error(), "status=401") {
			return nil, util.NewClientServerError("unauthorized error", http.StatusUnauthorized, err)
		}
		return nil, util.ProcessGeneralError(lookercommon.ExtractLookerErrorMessage(err))
	}

	logger.DebugContext(ctx, "resp = ", resp)

	if resultFormat == "json_detail" {
		var detailResp map[string]any
		if err := json.Unmarshal([]byte(resp), &detailResp); err != nil {
			return nil, util.NewClientServerError("error unmarshaling json_detail response", http.StatusInternalServerError, err)
		}
		if wq.Pivots != nil && len(*wq.Pivots) > 0 {
			if pivErr := lookercommon.ValidatePivotsInDetailResponse(*wq.Pivots, detailResp); pivErr != nil {
				return nil, util.NewAgentError(pivErr.Error(), pivErr)
			}
		}
		if generateDrillLinks != nil && !*generateDrillLinks {
			lookercommon.StripDrillLinks(detailResp)
		}
		result := map[string]any{
			"data":        detailResp["data"],
			"fields":      detailResp["fields"],
			"pivots":      detailResp["pivots"],
			"totals_data": detailResp["totals_data"],
			"status":      "success",
		}
		if errs, ok := detailResp["errors"]; ok && errs != nil {
			result["errors"] = errs
		}
		if visConfigObj != nil {
			result["vis_config"] = visConfigObj
		}
		return applySavedQueryPayloadLimit(result), nil
	}

	var data []any
	e := json.Unmarshal([]byte(resp), &data)
	if e != nil {
		return nil, util.NewClientServerError("error unmarshaling query response", http.StatusInternalServerError, e)
	}

	logger.DebugContext(ctx, "data = ", data)

	if visConfigObj != nil {
		result := map[string]any{
			"data":       data,
			"status":     "success",
			"vis_config": visConfigObj,
		}
		return applySavedQueryPayloadLimit(result), nil
	}
	if _, tooLarge := lookercommon.ExceedsMCPPayloadLimit(data, lookercommon.MaxMCPPayloadBytes); tooLarge {
		return applySavedQueryPayloadLimit(map[string]any{
			"data":   data,
			"status": "success",
		}), nil
	}

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
