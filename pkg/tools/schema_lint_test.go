package tools

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// untypedSchemaPaths walks a tool's parameter schema and returns the path of
// every schema node (the root, each property, each array "items", nested
// properties, and any sub-schema under additionalProperties / oneOf / anyOf /
// allOf) that has no "type". The Gemini family of APIs rejects a request whose
// tool schema contains one, and tool definitions go out on every request.
//
// sanitizeSchemaForGemini (pkg/providers) is not called from here: it is
// unexported, and a test in package providers cannot import pkg/tools because
// pkg/tools imports pkg/providers. It does not add a missing property type, so
// a clean walk of the schema as the tool built it is the contract.
func untypedSchemaPaths(schema map[string]any) []string {
	var bad []string
	walkSchema("$", schema, &bad)
	sort.Strings(bad)
	return bad
}

func walkSchema(path string, node map[string]any, bad *[]string) {
	if node == nil {
		return
	}
	switch tv := node["type"].(type) {
	case string:
		if tv == "" {
			*bad = append(*bad, path)
		}
	case []string:
		if len(tv) == 0 {
			*bad = append(*bad, path)
		}
	case []any:
		if len(tv) == 0 {
			*bad = append(*bad, path)
		}
	default:
		*bad = append(*bad, path)
	}
	if props, ok := node["properties"].(map[string]any); ok {
		for name, v := range props {
			child, isMap := v.(map[string]any)
			if !isMap {
				*bad = append(*bad, fmt.Sprintf("%s.%s (not a schema map)", path, name))
				continue
			}
			walkSchema(path+"."+name, child, bad)
		}
	}
	if items, ok := node["items"].(map[string]any); ok {
		walkSchema(path+"[]", items, bad)
	}
	if ap, ok := node["additionalProperties"].(map[string]any); ok {
		walkSchema(path+"{}", ap, bad)
	}
	for _, kw := range []string{"oneOf", "anyOf", "allOf"} {
		if list, ok := node[kw].([]any); ok {
			for i, v := range list {
				if m, ok := v.(map[string]any); ok {
					walkSchema(fmt.Sprintf("%s.%s[%d]", path, kw, i), m, bad)
				}
			}
		}
	}
}

// The walker itself must catch each way a node can lack a type.
func TestUntypedSchemaPaths_Walker(t *testing.T) {
	good := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"a": map[string]any{"type": "string"},
			"b": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"c": map[string]any{"type": "object", "properties": map[string]any{"d": map[string]any{"type": "integer"}}},
		},
	}
	if bad := untypedSchemaPaths(good); len(bad) != 0 {
		t.Fatalf("false positive: %v", bad)
	}
	bad := untypedSchemaPaths(map[string]any{
		"properties": map[string]any{
			"a": map[string]any{"description": "no type"},
			"b": map[string]any{"type": "array", "items": map[string]any{"description": "untyped items"}},
			"c": map[string]any{"type": "object", "properties": map[string]any{"d": map[string]any{}}},
			"e": "not a schema",
		},
	})
	want := []string{"$", "$.a", "$.b[]", "$.c.d", "$.e (not a schema map)"}
	if strings.Join(bad, "|") != strings.Join(want, "|") {
		t.Fatalf("untyped = %v, want %v", bad, want)
	}
}
