// Package toolset compares tool definitions across profiles and builds the
// definitions exposed to clients.
package toolset

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Diff reports how other differs from base in tool names and input schemas.
// It returns nil when they match. Descriptions, output schemas and annotations are ignored.
func Diff(base, other []*mcp.Tool) ([]string, error) {
	baseSchemas, err := schemasByName(base)
	if err != nil {
		return nil, err
	}
	otherSchemas, err := schemasByName(other)
	if err != nil {
		return nil, err
	}

	var diffs []string
	for _, name := range sortedKeys(baseSchemas) {
		o, ok := otherSchemas[name]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("tool %q is missing", name))
			continue
		}
		if !bytes.Equal(baseSchemas[name], o) {
			diffs = append(diffs, fmt.Sprintf("tool %q has a different inputSchema", name))
		}
	}
	for _, name := range sortedKeys(otherSchemas) {
		if _, ok := baseSchemas[name]; !ok {
			diffs = append(diffs, fmt.Sprintf("tool %q is unexpected", name))
		}
	}
	return diffs, nil
}

func schemasByName(tools []*mcp.Tool) (map[string][]byte, error) {
	m := make(map[string][]byte, len(tools))
	for _, t := range tools {
		b, err := canonical(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("tool %q: %w", t.Name, err)
		}
		m[t.Name] = b
	}
	return m, nil
}

// canonical returns JSON with object keys sorted, so that key order does not affect comparison.
func canonical(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var generic any
	if err := json.Unmarshal(b, &generic); err != nil {
		return nil, err
	}
	// encoding/json sorts map keys on marshal.
	return json.Marshal(generic)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// ProfileArg describes the argument injected into every tool.
type ProfileArg struct {
	// Name is the argument name, such as "profile".
	Name string
	// Profiles lists the selectable profile names.
	Profiles []string
	// Default is the profile used when the argument is omitted. Empty makes the argument required.
	Default string
	// ListToolName is the name of the profile listing tool, if exposed.
	ListToolName string
}

// Schema returns the JSON Schema of the profile argument.
func (a ProfileArg) Schema() map[string]any {
	desc := "Target profile."
	if a.Default != "" {
		desc += fmt.Sprintf(" Defaults to %q.", a.Default)
	}
	if a.ListToolName != "" {
		desc += fmt.Sprintf(" See %s for details of each profile.", a.ListToolName)
	}
	return map[string]any{
		"type":        "string",
		"enum":        slices.Clone(a.Profiles),
		"description": desc,
	}
}

// WithProfileArg returns copies of tools whose input schemas accept the profile argument.
// It fails if any tool already has a property with the same name.
func WithProfileArg(tools []*mcp.Tool, arg ProfileArg) ([]*mcp.Tool, error) {
	out := make([]*mcp.Tool, 0, len(tools))
	for _, t := range tools {
		schema, err := toObject(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("tool %q: %w", t.Name, err)
		}
		props, _ := schema["properties"].(map[string]any)
		if props == nil {
			props = map[string]any{}
		}
		if _, exists := props[arg.Name]; exists {
			return nil, fmt.Errorf("tool %q already has an argument named %q; set profileArg to another name", t.Name, arg.Name)
		}
		props[arg.Name] = arg.Schema()
		schema["properties"] = props
		if arg.Default == "" {
			required, _ := schema["required"].([]any)
			schema["required"] = append(required, arg.Name)
		}

		c := *t
		c.InputSchema = schema
		out = append(out, &c)
	}
	return out, nil
}

// toObject deep-copies a schema into a JSON object.
func toObject(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("inputSchema is not a JSON object: %w", err)
	}
	if m == nil {
		m = map[string]any{"type": "object"}
	}
	return m, nil
}

// Names returns the tool names.
func Names(tools []*mcp.Tool) []string {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	return names
}

// FormatDiff joins diffs into one line for error messages.
func FormatDiff(diffs []string) string { return strings.Join(diffs, "; ") }
