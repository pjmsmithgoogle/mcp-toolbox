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
	"fmt"
	"strings"
)

// Standard MIME type for MCP Apps UI resources.
const UIResourceMimeType = "text/html;profile=mcp-app"

// McpUiResourceCsp defines Content Security Policy domains for an MCP UI resource according to SEP-1865.
type McpUiResourceCsp struct {
	ConnectDomains  []string `json:"connectDomains,omitempty" yaml:"connect_domains,omitempty"`
	ResourceDomains []string `json:"resourceDomains,omitempty" yaml:"resource_domains,omitempty"`
	FrameDomains    []string `json:"frameDomains,omitempty" yaml:"frame_domains,omitempty"`
	BaseUriDomains  []string `json:"baseUriDomains,omitempty" yaml:"base_uri_domains,omitempty"`
}

// UIResourceMeta defines metadata for an MCP UI resource.
type UIResourceMeta struct {
	CSP           *McpUiResourceCsp `json:"csp,omitempty" yaml:"csp,omitempty"`
	Permissions   map[string]any    `json:"permissions,omitempty" yaml:"permissions,omitempty"`
	Domain        string            `json:"domain,omitempty" yaml:"domain,omitempty"`
	PrefersBorder *bool             `json:"prefersBorder,omitempty" yaml:"prefers_border,omitempty"`
}

// ResourceMeta wraps the UI metadata inside the standard _meta field.
type ResourceMeta struct {
	UI *UIResourceMeta `json:"ui,omitempty" yaml:"ui,omitempty"`
}

// ResourceContent is the content of a resource returned by ReadResource.
type ResourceContent struct {
	URI      string        `json:"uri"`
	MIMEType string        `json:"mimeType"`
	Text     string        `json:"text,omitempty"`
	Blob     string        `json:"blob,omitempty"`
	Meta     *ResourceMeta `json:"_meta,omitempty"`
}

// Resource is the interface for an MCP resource.
type Resource interface {
	GetURI() string
	GetName() string
	GetDescription() string
	GetMIMEType() string
	GetMeta() *ResourceMeta
	Read(ctx context.Context) (ResourceContent, error)
}

// StaticResource is an in-memory Resource implementation.
type StaticResource struct {
	URI         string
	Name        string
	Description string
	MIMEType    string
	Text        string
	Blob        string
	Meta        *ResourceMeta
}

func (s StaticResource) GetURI() string         { return s.URI }
func (s StaticResource) GetName() string        { return s.Name }
func (s StaticResource) GetDescription() string { return s.Description }
func (s StaticResource) GetMIMEType() string    { return s.MIMEType }
func (s StaticResource) GetMeta() *ResourceMeta { return s.Meta }

func (s StaticResource) Read(_ context.Context) (ResourceContent, error) {
	mimeType := s.MIMEType
	if mimeType == "" {
		if strings.HasPrefix(s.URI, "ui://") {
			mimeType = UIResourceMimeType
		} else {
			mimeType = "text/plain"
		}
	}
	return ResourceContent{
		URI:      s.URI,
		MIMEType: mimeType,
		Text:     s.Text,
		Blob:     s.Blob,
		Meta:     s.Meta,
	}, nil
}

// NewUIResource constructs a StaticResource configured for MCP Apps UI.
func NewUIResource(uri, name, description, html string, csp *McpUiResourceCsp, prefersBorder *bool) StaticResource {
	var meta *ResourceMeta
	if csp != nil || prefersBorder != nil {
		meta = &ResourceMeta{
			UI: &UIResourceMeta{
				CSP:           csp,
				PrefersBorder: prefersBorder,
			},
		}
	}
	if html == "" {
		html = DefaultBaseUIHTML
	}
	return StaticResource{
		URI:         uri,
		Name:        name,
		Description: description,
		MIMEType:    UIResourceMimeType,
		Text:        html,
		Meta:        meta,
	}
}

// DynamicResource is a Resource implementation that calls a function on Read.
type DynamicResource struct {
	URI         string
	Name        string
	Description string
	MIMEType    string
	Meta        *ResourceMeta
	ReadFunc    func(ctx context.Context) (ResourceContent, error)
}

func (d DynamicResource) GetURI() string         { return d.URI }
func (d DynamicResource) GetName() string        { return d.Name }
func (d DynamicResource) GetDescription() string { return d.Description }
func (d DynamicResource) GetMIMEType() string    { return d.MIMEType }
func (d DynamicResource) GetMeta() *ResourceMeta { return d.Meta }

func (d DynamicResource) Read(ctx context.Context) (ResourceContent, error) {
	if d.ReadFunc != nil {
		return d.ReadFunc(ctx)
	}
	mimeType := d.MIMEType
	if mimeType == "" {
		if strings.HasPrefix(d.URI, "ui://") {
			mimeType = UIResourceMimeType
		} else {
			mimeType = "text/plain"
		}
	}
	return ResourceContent{
		URI:      d.URI,
		MIMEType: mimeType,
		Meta:     d.Meta,
	}, nil
}

// NewDynamicUIResource constructs a DynamicResource configured for MCP Apps UI.
func NewDynamicUIResource(uri, name, description string, csp *McpUiResourceCsp, prefersBorder *bool, readFunc func(ctx context.Context) (string, error)) DynamicResource {
	var meta *ResourceMeta
	if csp != nil || prefersBorder != nil {
		meta = &ResourceMeta{
			UI: &UIResourceMeta{
				CSP:           csp,
				PrefersBorder: prefersBorder,
			},
		}
	}
	return DynamicResource{
		URI:         uri,
		Name:        name,
		Description: description,
		MIMEType:    UIResourceMimeType,
		Meta:        meta,
		ReadFunc: func(ctx context.Context) (ResourceContent, error) {
			text, err := readFunc(ctx)
			if err != nil {
				return ResourceContent{}, err
			}
			if text == "" {
				text = DefaultBaseUIHTML
			}
			return ResourceContent{
				URI:      uri,
				MIMEType: UIResourceMimeType,
				Text:     text,
				Meta:     meta,
			}, nil
		},
	}
}

// ResourceProvider is an optional interface tools can implement to expose UI resources.
type ResourceProvider interface {
	GetResources() []Resource
}

// Validate checks that a Resource has valid fields according to MCP specifications.
func Validate(r Resource) error {
	if r == nil {
		return fmt.Errorf("resource cannot be nil")
	}
	if r.GetURI() == "" {
		return fmt.Errorf("resource URI cannot be empty")
	}
	return nil
}
