package toolset

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func tool(t *testing.T, name, schema string) *mcp.Tool {
	t.Helper()
	var s map[string]any
	if err := json.Unmarshal([]byte(schema), &s); err != nil {
		t.Fatal(err)
	}
	return &mcp.Tool{Name: name, Description: "desc of " + name, InputSchema: s}
}

func TestDiff(t *testing.T) {
	base := []*mcp.Tool{
		tool(t, "a", `{"type": "object", "properties": {"x": {"type": "string"}}}`),
		tool(t, "b", `{"type": "object"}`),
	}

	t.Run("same with different key order and description", func(t *testing.T) {
		other := []*mcp.Tool{
			tool(t, "b", `{"type": "object"}`),
			tool(t, "a", `{"properties": {"x": {"type": "string"}}, "type": "object"}`),
		}
		other[0].Description = "changed"
		diffs, err := Diff(base, other)
		if err != nil || diffs != nil {
			t.Errorf("Diff = %v, %v; want no diff", diffs, err)
		}
	})

	t.Run("mismatch", func(t *testing.T) {
		other := []*mcp.Tool{
			tool(t, "a", `{"type": "object", "properties": {"x": {"type": "number"}}}`),
			tool(t, "c", `{"type": "object"}`),
		}
		diffs, err := Diff(base, other)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{
			`tool "a" has a different inputSchema`,
			`tool "b" is missing`,
			`tool "c" is unexpected`,
		}
		if !reflect.DeepEqual(diffs, want) {
			t.Errorf("Diff = %v, want %v", diffs, want)
		}
	})
}

func TestWithProfileArg(t *testing.T) {
	base := []*mcp.Tool{
		tool(t, "a", `{"type": "object", "properties": {"x": {"type": "string"}}, "required": ["x"]}`),
		tool(t, "b", `{"type": "object"}`),
	}

	t.Run("required without default", func(t *testing.T) {
		got, err := WithProfileArg(base, ProfileArg{Name: "profile", Profiles: []string{"dev", "prod"}})
		if err != nil {
			t.Fatal(err)
		}
		a := got[0].InputSchema.(map[string]any)
		if !reflect.DeepEqual(a["required"], []any{"x", "profile"}) {
			t.Errorf("required = %v", a["required"])
		}
		prop := a["properties"].(map[string]any)["profile"].(map[string]any)
		if !reflect.DeepEqual(prop["enum"], []string{"dev", "prod"}) {
			t.Errorf("enum = %v", prop["enum"])
		}
		b := got[1].InputSchema.(map[string]any)
		if _, ok := b["properties"].(map[string]any)["profile"]; !ok {
			t.Errorf("properties not created: %v", b)
		}
		// The input must not be modified.
		if _, ok := base[0].InputSchema.(map[string]any)["properties"].(map[string]any)["profile"]; ok {
			t.Error("base tool was modified")
		}
	})

	t.Run("optional with default", func(t *testing.T) {
		got, err := WithProfileArg(base, ProfileArg{Name: "profile", Profiles: []string{"dev"}, Default: "dev", ListToolName: "list_profiles"})
		if err != nil {
			t.Fatal(err)
		}
		b := got[1].InputSchema.(map[string]any)
		if _, ok := b["required"]; ok {
			t.Errorf("required should not be set: %v", b["required"])
		}
		desc := b["properties"].(map[string]any)["profile"].(map[string]any)["description"].(string)
		if !strings.Contains(desc, `Defaults to "dev"`) || !strings.Contains(desc, "list_profiles") {
			t.Errorf("description = %q", desc)
		}
	})

	t.Run("conflict", func(t *testing.T) {
		_, err := WithProfileArg(base, ProfileArg{Name: "x", Profiles: []string{"dev"}})
		if err == nil || !strings.Contains(err.Error(), `already has an argument named "x"`) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestMismatched(t *testing.T) {
	base := []*mcp.Tool{
		tool(t, "same", `{"type": "object"}`),
		tool(t, "changed", `{"type": "object", "properties": {"x": {"type": "string"}}}`),
		tool(t, "missing", `{"type": "object"}`),
	}
	other := []*mcp.Tool{
		tool(t, "same", `{"type": "object"}`),
		tool(t, "changed", `{"type": "object", "properties": {"x": {"type": "number"}}}`),
		tool(t, "extra", `{"type": "object"}`),
	}
	got, err := Mismatched(base, other)
	if err != nil {
		t.Fatal(err)
	}
	want := []Mismatch{{Tool: "changed", Reason: ReasonInputSchema}, {Tool: "missing", Reason: ReasonMissing}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Mismatched = %v, want %v", got, want)
	}
}
