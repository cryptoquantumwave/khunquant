package agent

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// untypedSchemaPaths returns the path of every schema node (the root, each
// property, array items, nested properties, additionalProperties and
// oneOf/anyOf/allOf sub-schemas) that has no "type". The Gemini family of APIs
// rejects a tool schema containing one, and tool definitions go out on every
// request. (The same walker lives in pkg/tools' tests; test helpers do not
// cross packages.)
func untypedSchemaPaths(schema map[string]any) []string {
	var bad []string
	walkSchemaTypes("$", schema, &bad)
	sort.Strings(bad)
	return bad
}

func walkSchemaTypes(path string, node map[string]any, bad *[]string) {
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
			walkSchemaTypes(path+"."+name, child, bad)
		}
	}
	if items, ok := node["items"].(map[string]any); ok {
		walkSchemaTypes(path+"[]", items, bad)
	}
	if ap, ok := node["additionalProperties"].(map[string]any); ok {
		walkSchemaTypes(path+"{}", ap, bad)
	}
	for _, kw := range []string{"oneOf", "anyOf", "allOf"} {
		if list, ok := node[kw].([]any); ok {
			for i, v := range list {
				if m, ok := v.(map[string]any); ok {
					walkSchemaTypes(fmt.Sprintf("%s.%s[%d]", path, kw, i), m, bad)
				}
			}
		}
	}
}

// untypedSchemaSkipList names registered tools whose parameter schema already
// had an untyped node before MR-108. They are reported, not fixed here: the
// owner of those tools decides. A tool leaves this list by being fixed; a tool
// that is not on it must have a fully typed schema.
var untypedSchemaSkipList = map[string]string{}

// Every tool in the real per-agent registry (everything enabled, as in
// TestClassOf_AllRegisteredToolsClassified) must have a fully typed schema,
// except the ones listed above.
func TestRegisteredTools_SchemasFullyTyped(t *testing.T) {
	cfg := &config.Config{Agents: config.AgentsConfig{
		Defaults: config.AgentDefaults{
			Workspace: t.TempDir(), Model: "char-model", MaxTokens: 4096, MaxToolIterations: 10,
		},
	}}
	enableEveryTool(cfg)
	al := NewAgentLoop(cfg, bus.NewMessageBus(), &charScriptedProvider{})
	t.Cleanup(al.Close)

	reg := al.GetRegistry().GetDefaultAgent().Tools
	names := reg.List()
	if len(names) < 60 {
		t.Fatalf("only %d tools registered; the fixture is not enabling them", len(names))
	}
	for _, want := range []string{"get_profile", "update_profile"} {
		if _, ok := reg.Get(want); !ok {
			t.Fatalf("%s missing from the registry fixture", want)
		}
	}

	var skipped []string
	for _, name := range names {
		tool, _ := reg.Get(name)
		bad := untypedSchemaPaths(tool.Parameters())
		if len(bad) == 0 {
			if _, listed := untypedSchemaSkipList[name]; listed {
				t.Errorf("%s is now fully typed: remove it from untypedSchemaSkipList", name)
			}
			continue
		}
		if _, listed := untypedSchemaSkipList[name]; listed {
			skipped = append(skipped, fmt.Sprintf("%s: %s", name, strings.Join(bad, ", ")))
			continue
		}
		t.Errorf("tool %q has untyped schema nodes %v: give every property a \"type\" (Gemini rejects the request otherwise)", name, bad)
	}
	if len(skipped) > 0 {
		t.Logf("known untyped schemas (skip-listed, not fixed by MR-108):\n  %s", strings.Join(skipped, "\n  "))
	}
}
