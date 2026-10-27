package openapimcp

import (
	"fmt"
	"strings"
)

// Synthesize turns a Document into tools: one per supported operation, named,
// described, with a flattened input schema and the bindings the executor needs.
// Operations that cannot become tools are listed in Unsupported with a reason;
// the result is still usable as long as at least one tool was produced (the
// factory enforces that, the preview shows both lists).
func Synthesize(doc *Document, opts SynthesizeOptions) (*Synthesis, error) {
	if doc == nil {
		return nil, fmt.Errorf("document is nil")
	}
	budget := toolNameBudget(opts.ClientName)
	if budget < minToolNameBudget {
		return nil, fmt.Errorf("client name %q is too long for OpenAPI tool names: \"<client>-<tool>\" must fit %d characters, which leaves fewer than %d for the tool; use a name of at most %d characters", opts.ClientName, MaxToolNameLen, minToolNameBudget, MaxToolNameLen-1-minToolNameBudget)
	}
	syn := &Synthesis{Document: doc, Warnings: append([]string(nil), doc.Warnings...)}
	retained := 0

	for _, op := range doc.Operations {
		unsupported := func(reason string) {
			// The reason can quote document text; each entry keeps its own copy.
			reason = capDescription(reason)
			retained += len(reason)
			syn.Unsupported = append(syn.Unsupported, UnsupportedOperation{OperationID: op.ID, Method: op.Method, Path: op.Path, Reason: reason})
		}
		if op.Unsupported != "" {
			unsupported(op.Unsupported)
			continue
		}
		if op.Deprecated && !opts.IncludeDeprecated {
			unsupported("deprecated (set openapi_config.include_deprecated to expose it)")
			continue
		}
		schema, args, body, warnings, err := FlattenInputSchema(op)
		if err != nil {
			unsupported(err.Error())
			continue
		}
		tool := Tool{
			Name:        ToolName(op, budget),
			Operation:   op,
			Description: BuildDescription(op),
			InputSchema: schema,
			Args:        args,
			Body:        body,
			Annotations: annotationsFor(op.Method),
			Warnings:    warnings,
		}
		if w := securityWarning(op, doc); w != "" {
			tool.Warnings = append(tool.Warnings, w)
		}
		// Each tool retains its own text; cap every warning and bound the
		// document's total so per-tool copies cannot grow with the tool count.
		for i := range tool.Warnings {
			tool.Warnings[i] = capDescription(tool.Warnings[i])
		}
		retained += len(tool.Name) + len(tool.Description) + len(tool.InputSchema)
		for _, w := range tool.Warnings {
			retained += len(w)
		}
		if retained > maxSynthesizedTextBytes {
			return nil, fmt.Errorf("synthesized tool text exceeds %d bytes across the document", maxSynthesizedTextBytes)
		}
		syn.Tools = append(syn.Tools, tool)
	}

	if retained > maxSynthesizedTextBytes {
		return nil, fmt.Errorf("synthesized tool text exceeds %d bytes across the document", maxSynthesizedTextBytes)
	}
	if len(syn.Tools) > MaxToolCount {
		return nil, fmt.Errorf("spec yields %d tools, above the limit of %d", len(syn.Tools), MaxToolCount)
	}
	syn.Warnings = append(syn.Warnings, DedupeToolNames(syn.Tools, budget)...)
	if len(syn.Tools) > ToolCountWarnThreshold {
		syn.Warnings = append(syn.Warnings, fmt.Sprintf("%d tools exceed the recommended %d; restrict tools_to_execute so the model is not offered every operation", len(syn.Tools), ToolCountWarnThreshold))
	}
	if len(syn.Tools) == 0 {
		syn.Warnings = append(syn.Warnings, "no operation in the spec can be exposed as a tool")
	}
	return syn, nil
}

// securityWarning flags an operation whose every security requirement depends
// on an unsupported scheme: it will be called unauthenticated unless the admin
// supplies static headers.
func securityWarning(op Operation, doc *Document) string {
	reqs := doc.GlobalSecurity
	if op.HasSecurity {
		reqs = op.Security
	}
	if len(reqs) == 0 {
		return ""
	}
	// The warning names each unsupported scheme, and every tool keeps its own
	// copy, so names are shortened and the list stops at maxDescriptionLen.
	var unsupported []string
	size := 0
	for _, req := range reqs {
		if len(req) == 0 {
			return "" // an empty requirement means the operation may be called without credentials
		}
		satisfiable := true
		for name := range req {
			scheme, ok := doc.SecuritySchemes[name]
			if !ok || !scheme.Supported {
				satisfiable = false
				if size > maxDescriptionLen {
					continue
				}
				entry := capSchemeName(name) + " (undeclared)"
				if ok {
					entry = fmt.Sprintf("%s (%s)", capSchemeName(name), capSchemeName(scheme.Type))
				}
				unsupported = append(unsupported, entry)
				size += len(entry) + 2
			}
		}
		if satisfiable {
			return ""
		}
	}
	return capDescription(fmt.Sprintf("requires security scheme %s which is not supported; calls are sent with static headers only", strings.Join(unsupported, ", ")))
}

// capSchemeName shortens a security scheme name (or type) quoted in a warning.
func capSchemeName(name string) string {
	const maxSchemeNameLen = 128
	if len(name) <= maxSchemeNameLen {
		return name
	}
	return name[:maxSchemeNameLen] + "…"
}
