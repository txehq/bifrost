package openapimcp

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"sort"
	"strings"

	"github.com/capsohq/bifrost/core/schemas"
)

// ResolveBaseURL decides the upstream base: an explicit override wins, else the
// spec's first absolute server, else a relative server resolved against the
// spec URL, else OpenAPI 3's default server. explicit reports whether the admin
// override was used (op-level servers are then ignored by the executor).
func ResolveBaseURL(doc *Document, override *string, specURL string) (base string, explicit bool, err error) {
	if override != nil && strings.TrimSpace(*override) != "" {
		base = strings.TrimSpace(*override)
		if !isAbsoluteHTTPURL(base) {
			return "", false, errors.New("openapi_config.base_url must be an absolute http(s) URL")
		}
		return strings.TrimSuffix(base, "/"), true, nil
	}
	if doc != nil {
		for _, srv := range doc.Servers {
			if isAbsoluteHTTPURL(srv) {
				return strings.TrimSuffix(srv, "/"), false, nil
			}
			if resolved := resolveRelativeURL(srv, doc.Self, specURL); isAbsoluteHTTPURL(resolved) {
				return strings.TrimSuffix(resolved, "/"), false, nil
			}
		}
		if server := defaultServerURL(doc, specURL); server != "" {
			return server, false, nil
		}
	}
	return "", false, errors.New("the spec declares no absolute server URL; set openapi_config.base_url")
}

// defaultServerURL is the server OpenAPI 3 implies when servers is absent or
// empty: "/", resolved against $self or the URL the document came from. It is ""
// when that default does not apply or nothing resolves it.
//
// The default is an inference, so it stays narrower than a declared server. An
// operation's own server at another origin would be refused on every call, and
// such a document keeps asking for a base URL when it is saved instead. Userinfo
// is dropped: a credential for reading the document is not one for calling the
// API, and the resolved URL is shown in previews. For the same reason an operation
// server that carries userinfo, which a relative one inherits from the document's
// URL, keeps asking too: the executor would send it to the API.
func defaultServerURL(doc *Document, specURL string) string {
	if len(doc.Servers) > 0 || !strings.HasPrefix(doc.OpenAPIVersion, "3") {
		return ""
	}
	resolved := resolveRelativeURL("/", doc.Self, specURL)
	if !isAbsoluteHTTPURL(resolved) {
		return ""
	}
	parsed, err := url.Parse(resolved)
	if err != nil {
		return ""
	}
	parsed.User = nil
	server := strings.TrimSuffix(parsed.String(), "/")
	origin, ok := urlOrigin(server)
	if !ok {
		return ""
	}
	for i := range doc.Operations {
		servers := doc.Operations[i].Servers
		if len(servers) == 0 || !isAbsoluteHTTPURL(servers[0]) {
			continue
		}
		if opOrigin, ok := urlOrigin(servers[0]); !ok || opOrigin != origin || hasUserinfo(servers[0]) {
			return ""
		}
	}
	return server
}

func hasUserinfo(raw string) bool {
	parsed, err := url.Parse(raw)
	return err != nil || parsed.User != nil
}

// previewServers lists the document's servers. The default server is listed
// when it applies, so a form shows it as the document's own and saves no
// override, and a later change of spec_url keeps deciding the base.
func previewServers(doc *Document, specURL string) []string {
	servers := append([]string{}, doc.Servers...)
	if server := defaultServerURL(doc, specURL); server != "" {
		servers = append(servers, server)
	}
	return servers
}

// ResolveServingBaseURL is ResolveBaseURL for a spec about to be served.
// Operation-level servers never stand in for a missing base URL: the executor
// uses them only at the base URL's origin, so a spec that declares servers only
// on its operations needs an explicit openapi_config.base_url unless they share
// the origin of OpenAPI 3's default server.
func ResolveServingBaseURL(doc *Document, _ *Synthesis, override *string, specURL string) (base string, explicit bool, err error) {
	return ResolveBaseURL(doc, override, specURL)
}

// SpecHash is the stable identity of a spec's bytes.
func SpecHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Preview parses and synthesizes without building a server, for the UI's
// parse-first flow and for validating a create/update request.
func Preview(data []byte, parseOpts ParseOptions, synthOpts SynthesizeOptions, baseURLOverride *string) (*PreviewResult, *Synthesis, error) {
	doc, err := Parse(data, parseOpts)
	if err != nil {
		return nil, nil, err
	}
	syn, err := Synthesize(doc, synthOpts)
	if err != nil {
		return nil, nil, err
	}
	result := &PreviewResult{
		Title:           doc.Title,
		Version:         doc.Version,
		OpenAPIVersion:  doc.OpenAPIVersion,
		Description:     doc.Description,
		Servers:         previewServers(doc, parseOpts.SpecURL),
		SecuritySchemes: make([]SecurityScheme, 0, len(doc.SecuritySchemes)),
		Tools:           make([]ToolSummary, 0, len(syn.Tools)),
		Unsupported:     make([]UnsupportedOperation, 0, len(syn.Unsupported)),
		Warnings:        append([]string{}, syn.Warnings...),
		ToolCount:       len(syn.Tools),
		SpecSize:        len(data),
		SpecHash:        SpecHash(data),
	}
	if base, _, err := ResolveServingBaseURL(doc, syn, baseURLOverride, parseOpts.SpecURL); err == nil {
		result.BaseURL = base
	} else {
		result.Warnings = append(result.Warnings, err.Error())
	}
	for _, name := range sortedSchemeNames(doc.SecuritySchemes) {
		result.SecuritySchemes = append(result.SecuritySchemes, doc.SecuritySchemes[name])
	}
	// A path item reused by many paths hands the same text to every operation,
	// and the response carries a copy per entry, so the summary is capped here as
	// BuildDescription caps the description. Identifiers (operationId, path,
	// method) are bounded at parse instead, never truncated.
	for _, u := range syn.Unsupported {
		result.Unsupported = append(result.Unsupported, u)
	}
	for _, t := range syn.Tools {
		result.Tools = append(result.Tools, ToolSummary{
			Name:        t.Name,
			OperationID: t.Operation.ID,
			Method:      t.Operation.Method,
			Path:        t.Operation.Path,
			Summary:     capDescription(t.Operation.Summary),
			Description: t.Description,
			Deprecated:  t.Operation.Deprecated,
			InputSchema: t.InputSchema,
			Annotations: t.Annotations,
			Warnings:    t.Warnings,
		})
	}
	return result, syn, nil
}

// ToolNames lists the synthesized tool names, for validating tools_to_execute.
func (s *Synthesis) ToolNames() []string {
	if s == nil {
		return nil
	}
	names := make([]string, 0, len(s.Tools))
	for _, t := range s.Tools {
		names = append(names, t.Name)
	}
	return names
}

// ApplyMetadata fills the server-computed fields of an openapi_config from a
// parsed spec so stored rows describe themselves without re-parsing.
func ApplyMetadata(cfg *schemas.MCPOpenAPIConfig, data []byte, doc *Document, syn *Synthesis) {
	if cfg == nil {
		return
	}
	cfg.SpecSize = len(data)
	cfg.SpecHash = SpecHash(data)
	if doc != nil {
		cfg.SpecTitle = doc.Title
		cfg.OpenAPIVersion = doc.OpenAPIVersion
	}
	if syn != nil {
		cfg.OperationCount = len(syn.Tools)
	}
}

func sortedSchemeNames(m map[string]SecurityScheme) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
