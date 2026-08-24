// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package v20241105

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/googleapis/mcp-toolbox/internal/group"
	"github.com/googleapis/mcp-toolbox/internal/prompts"
	"github.com/googleapis/mcp-toolbox/internal/server/primitives"
	"github.com/googleapis/mcp-toolbox/internal/sources"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

// generateToolManifest generates Tool for list tools result
func generateToolManifest(name, desc string, authInvoke []string, params parameters.Parameters, annotations *tools.ToolAnnotations, ui *tools.ToolUIMeta, urlParams map[string]string) Tool {
	inputSchema, authParams := generateParamManifest(params, urlParams)

	var toolAnnotations *ToolAnnotations
	if annotations != nil {
		toolAnnotations = &ToolAnnotations{
			DestructiveHint: annotations.DestructiveHint,
			IdempotentHint:  annotations.IdempotentHint,
			OpenWorldHint:   annotations.OpenWorldHint,
			ReadOnlyHint:    annotations.ReadOnlyHint,
		}
	}
	mcpManifest := Tool{
		Name:            name,
		Description:     desc,
		ToolInputSchema: inputSchema,
		Annotations:     toolAnnotations,
	}
	metadata := make(map[string]any)
	if len(authInvoke) > 0 {
		metadata["toolbox/authInvoke"] = authInvoke
	}
	if len(authParams) > 0 {
		metadata["toolbox/authParam"] = authParams
	}
	if ui != nil && ui.ResourceURI != "" {
		uiMap := map[string]any{
			"resourceUri": ui.ResourceURI,
		}
		if len(ui.Visibility) > 0 {
			uiMap["visibility"] = ui.Visibility
		}
		metadata["ui"] = uiMap
		metadata["ui/resourceUri"] = ui.ResourceURI
	}
	if len(metadata) > 0 {
		mcpManifest.Metadata = metadata
	}
	return mcpManifest
}

// generateParamManifest generates the input schema and get authParam
func generateParamManifest(ps parameters.Parameters, urlParams map[string]string) (InputSchema, map[string][]string) {
	properties := make(map[string]parameters.ParameterMcpManifest)
	required := make([]string, 0)
	authParam := make(map[string][]string)

	for _, p := range ps {
		// If the parameter is sourced from another param, skip it in the MCP manifest
		if p.GetValueFromParam() != "" {
			continue
		}

		name := p.GetName()
		if urlParams != nil {
			// If the parameter is sourced from URL params, skip it in the MCP manifest
			if _, exists := urlParams[name]; exists {
				continue
			}
		}

		paramManifest, authParamList := p.McpManifest()
		defaultV := p.GetDefault()
		if defaultV != nil {
			paramManifest.Default = defaultV
		}
		properties[name] = paramManifest
		// parameters that doesn't have a default value are added to the required field
		if parameters.CheckParamRequired(p.GetRequired(), defaultV) {
			required = append(required, name)
		}
		if len(authParamList) > 0 {
			authParam[name] = authParamList
		}
	}
	return InputSchema{
		Type:       "object",
		Properties: properties,
		Required:   required,
	}, authParam
}

// GenerateListToolsResult generates tools/list method result according to mcp schema
func GenerateListToolsResult(pMgr *primitives.PrimitiveManager, g group.Group, urlParams map[string]string) (ListToolsResult, error) {
	mcpManifest := make([]Tool, 0, len(g.ToolNames))
	for _, toolName := range g.ToolNames {
		tool, ok := pMgr.GetTool(toolName)
		if !ok {
			return ListToolsResult{}, fmt.Errorf("tool does not exist: %s", toolName)
		}
		srcName := tool.GetSourceName()
		var src sources.Source
		if srcName != "" {
			src, ok = pMgr.GetSource(srcName)
			if !ok {
				return ListToolsResult{}, fmt.Errorf("unable to retrieve %s source for tool %q", srcName, tool.GetName())
			}
		}
		params, err := tool.GetParameters(src)
		if err != nil {
			return ListToolsResult{}, fmt.Errorf("error getting parameters for tool %q: %w", toolName, err)
		}
		toolManifest := generateToolManifest(toolName, tool.GetDescription(), tool.GetAuthRequired(), params, tool.GetAnnotations(), tool.GetUIMeta(), urlParams)
		mcpManifest = append(mcpManifest, toolManifest)
	}
	return ListToolsResult{Tools: mcpManifest}, nil
}

// generatePromptManifest generates a version-specific Prompt manifest for list/prompts
func generatePromptManifest(name, desc string, args prompts.Arguments) Prompt {
	mcpArgs := make([]PromptArgument, 0, len(args))
	for _, arg := range args {
		promptArg := PromptArgument{
			Name:        arg.GetName(),
			Description: arg.GetDesc(),
			Required:    parameters.CheckParamRequired(arg.GetRequired(), arg.GetDefault()),
		}
		mcpArgs = append(mcpArgs, promptArg)
	}
	return Prompt{
		Name:        name,
		Description: desc,
		Arguments:   mcpArgs,
	}
}

// GenerateListPromptsResult generates the list/prompts result
func GenerateListPromptsResult(pMgr *primitives.PrimitiveManager, g group.Group) (ListPromptsResult, error) {
	mcpManifest := make([]Prompt, 0, len(g.PromptNames))
	for _, promptName := range g.PromptNames {
		prompt, ok := pMgr.GetPrompt(promptName)
		if !ok {
			return ListPromptsResult{}, fmt.Errorf("prompt does not exist: %s", promptName)
		}
		promptManifest := generatePromptManifest(promptName, prompt.GetDesc(), prompt.GetArguments())
		mcpManifest = append(mcpManifest, promptManifest)
	}
	return ListPromptsResult{Prompts: mcpManifest}, nil
}

// GenerateListResourcesResult generates the resources/list result according to mcp schema
func GenerateListResourcesResult(pMgr *primitives.PrimitiveManager, g group.Group) (ListResourcesResult, error) {
	groupResources := pMgr.GetResourcesForGroup(g)
	mcpResources := make([]Resource, 0, len(groupResources))
	for _, res := range groupResources {
		var metaMap map[string]any
		if meta := res.GetMeta(); meta != nil {
			metaBytes, err := json.Marshal(meta)
			if err == nil {
				_ = json.Unmarshal(metaBytes, &metaMap)
			}
		}
		mcpRes := Resource{
			BaseMetadata: BaseMetadata{Name: res.GetName()},
			URI:          res.GetURI(),
			Description:  res.GetDescription(),
			MIMEType:     res.GetMIMEType(),
			Metadata:     metaMap,
		}
		mcpResources = append(mcpResources, mcpRes)
	}
	return ListResourcesResult{Resources: mcpResources}, nil
}

// GenerateReadResourceResult generates the resources/read result according to mcp schema
func GenerateReadResourceResult(ctx context.Context, pMgr *primitives.PrimitiveManager, uri string) (ReadResourceResult, error) {
	res, ok := pMgr.GetResource(uri)
	if !ok {
		return ReadResourceResult{}, fmt.Errorf("resource with uri %q does not exist", uri)
	}
	content, err := res.Read(ctx)
	if err != nil {
		return ReadResourceResult{}, fmt.Errorf("error reading resource %q: %w", uri, err)
	}
	return ReadResourceResult{
		Contents: []any{content},
	}, nil
}
