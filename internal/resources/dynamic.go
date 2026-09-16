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

package resources

import (
	"context"
)

// DynamicUIResource represents an interactive MCP Apps UI resource that fetches or generates content dynamically.
type DynamicUIResource struct {
	ResourceConfigBase
	ReadFunc func(ctx context.Context) (string, error)
}

var _ Resource = &DynamicUIResource{}

// NewDynamicUIResource constructs a DynamicUIResource configured for MCP Apps UI.
func NewDynamicUIResource(name, uri, description string, csp *CSPConfig, prefersBorder *bool, readFunc func(ctx context.Context) (string, error)) *DynamicUIResource {
	return &DynamicUIResource{
		ResourceConfigBase: ResourceConfigBase{
			ConfigBase: ConfigBase{
				Name:          name,
				Type:          "dynamic_ui",
				Description:   description,
				MimeType:      "text/html;profile=mcp-app",
				UI:            true,
				PrefersBorder: prefersBorder,
				CSP:           csp,
			},
			URI: uri,
		},
		ReadFunc: readFunc,
	}
}

// GetSize returns nil for dynamic resources.
func (r *DynamicUIResource) GetSize() *int64 {
	return nil
}

// Read executes the dynamic content generation function.
func (r *DynamicUIResource) Read(ctx context.Context, _ map[string]any) (any, error) {
	if r.ReadFunc != nil {
		text, err := r.ReadFunc(ctx)
		if err != nil {
			return "", err
		}
		if text == "" {
			text = DefaultBaseUIHTML
		}
		return text, nil
	}
	return DefaultBaseUIHTML, nil
}

// ToConfig returns nil for dynamic resources.
func (r *DynamicUIResource) ToConfig() ResourceConfig {
	return nil
}
