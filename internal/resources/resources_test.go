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

package resources_test

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/googleapis/mcp-toolbox/internal/resources"
)

func TestNewUIResource(t *testing.T) {
	border := true
	csp := &resources.McpUiResourceCsp{
		ConnectDomains:  []string{"https://api.example.com"},
		ResourceDomains: []string{"https://cdn.example.com"},
	}
	r := resources.NewUIResource(
		"ui://test/view.html",
		"Test View",
		"A test UI view",
		"<!DOCTYPE html><html><body>Test</body></html>",
		csp,
		&border,
	)

	if diff := cmp.Diff("ui://test/view.html", r.GetURI()); diff != "" {
		t.Errorf("GetURI() mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(resources.UIResourceMimeType, r.GetMIMEType()); diff != "" {
		t.Errorf("GetMIMEType() mismatch (-want +got):\n%s", diff)
	}

	content, err := r.Read(context.Background())
	if err != nil {
		t.Fatalf("Read() failed: %v", err)
	}

	expectedContent := resources.ResourceContent{
		URI:      "ui://test/view.html",
		MIMEType: resources.UIResourceMimeType,
		Text:     "<!DOCTYPE html><html><body>Test</body></html>",
		Meta: &resources.ResourceMeta{
			UI: &resources.UIResourceMeta{
				CSP:           csp,
				PrefersBorder: &border,
			},
		},
	}

	if diff := cmp.Diff(expectedContent, content); diff != "" {
		t.Errorf("Read() content mismatch (-want +got):\n%s", diff)
	}
}

func TestNewDynamicUIResource(t *testing.T) {
	border := false
	csp := &resources.McpUiResourceCsp{
		ResourceDomains: []string{"https://static.lookercdn.com"},
	}
	r := resources.NewDynamicUIResource(
		"ui://test/dynamic.html",
		"Dynamic View",
		"A dynamic UI view",
		csp,
		&border,
		func(ctx context.Context) (string, error) {
			return "<html><body>Dynamic Content</body></html>", nil
		},
	)

	if r.GetURI() != "ui://test/dynamic.html" {
		t.Errorf("GetURI() mismatch: %s", r.GetURI())
	}

	content, err := r.Read(context.Background())
	if err != nil {
		t.Fatalf("Read() failed: %v", err)
	}
	if content.Text != "<html><body>Dynamic Content</body></html>" {
		t.Errorf("Read() text mismatch: %s", content.Text)
	}
	if content.Meta == nil || content.Meta.UI == nil || content.Meta.UI.CSP == nil {
		t.Fatalf("Read() meta CSP is nil")
	}
	if len(content.Meta.UI.CSP.ResourceDomains) != 1 || content.Meta.UI.CSP.ResourceDomains[0] != "https://static.lookercdn.com" {
		t.Errorf("Read() meta ResourceDomains mismatch: %v", content.Meta.UI.CSP.ResourceDomains)
	}
}

func TestValidate(t *testing.T) {
	if err := resources.Validate(nil); err == nil {
		t.Errorf("Validate(nil) expected error, got nil")
	}

	invalid := resources.StaticResource{}
	if err := resources.Validate(invalid); err == nil {
		t.Errorf("Validate(empty URI) expected error, got nil")
	}

	valid := resources.StaticResource{URI: "ui://example"}
	if err := resources.Validate(valid); err != nil {
		t.Errorf("Validate(valid) unexpected error: %v", err)
	}
}
