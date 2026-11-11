package scenario

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"gopkg.in/yaml.v3"
)

const schemaRel = "testdata/scenarios/schema.v1.json"

// repoRoot walks up to go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

func resolvedSchema(t *testing.T) *jsonschema.Resolved {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(schemaRel)))
	if err != nil {
		t.Fatal(err)
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("%s is not a schema: %v", schemaRel, err)
	}
	r, err := s.Resolve(nil)
	if err != nil {
		t.Fatalf("resolving %s: %v", schemaRel, err)
	}
	return r
}

// yamlInstance reads YAML the way a JSON Schema validator sees it.
func yamlInstance(t *testing.T, data string) any {
	t.Helper()
	var v any
	if err := yaml.Unmarshal([]byte(data), &v); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The committed schema is what go generate writes; CI's go generate cannot drift silently.
func TestScenarioSchema_IsFresh(t *testing.T) {
	want, err := SchemaJSON()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(schemaRel)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is stale: run go generate ./pkg/sandbox/scenario/", schemaRel)
	}
}

// Every committed scenario, known_bug and skipped ones included, validates against the
// schema: it must never reject what the loader accepts (TestScenarioFiles_Validate).
func TestScenarioSchema_ValidatesAllCommittedScenarios(t *testing.T) {
	rs := resolvedSchema(t)
	root := filepath.Join(repoRoot(t), filepath.FromSlash(DefaultDir))
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".yaml") {
			files = append(files, path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no scenario files under %s", root)
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := rs.Validate(yamlInstance(t, string(data))); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
}

const minimalScenario = `
version: 1
scenario: minimal
clock: { start: "2026-10-20T09:00:00+07:00" }
timeline:
  - at: "+5m"
    market.shock: { symbol: BTC/USDT, pct: -5 }
  - advance: 15m
expect:
  - message: { to: owner, count: 0 }
`

// The schema accepts forms the loader accepts that a naive schema would not, and rejects
// the mistakes it can see; the loader stays the authority for the rest.
func TestScenarioSchema_AcceptsAndRejects(t *testing.T) {
	rs := resolvedSchema(t)
	cases := []struct {
		name, yaml string
		ok         bool
	}{
		{"minimal", minimalScenario, true},
		{"count as a number, at as a number", strings.Replace(strings.Replace(minimalScenario, `count: 0`, `count: 1`, 1), `"+5m"`, `0`, 1), true},
		{"count beside the kind", strings.Replace(minimalScenario, `message: { to: owner, count: 0 }`, "message: { to: owner }\n    count: \">= 1\"\n    within: { from: 0, to: \"+1h\" }", 1), true},
		{"kind with no mapping", strings.Replace(minimalScenario, `message: { to: owner, count: 0 }`, `logs: {}`, 1), true},
		{"status given as null", minimalScenario + "status:\n", true},
		{"llm and known_bug given as null", minimalScenario + "llm:\nknown_bug:\n", true},
		{"clock mode empty", strings.Replace(minimalScenario, `clock: { start:`, `clock: { mode: "", start:`, 1), true},
		{"instance null", minimalScenario + "instance:\n", true},
		{"unknown top-level key", minimalScenario + "bogus: 1\n", false},
		{"unknown step kind", strings.Replace(minimalScenario, `market.shock`, `market.explode`, 1), false},
		{"step without its required argument", strings.Replace(minimalScenario, `pct: -5`, ``, 1), false},
		{"unknown expectation field", strings.Replace(minimalScenario, `to: owner`, `colour: red`, 1), false},
		{"unknown status", minimalScenario + "status: maybe\n", false},
		{"missing expect", strings.Replace(minimalScenario, "expect:\n  - message: { to: owner, count: 0 }\n", "", 1), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := rs.Validate(yamlInstance(t, c.yaml))
			if (err == nil) != c.ok {
				t.Fatalf("schema verdict = %v, want ok=%v", err, c.ok)
			}
			if c.ok { // what the schema accepts here, the loader accepts too
				f, perr := Parse(strings.NewReader(c.yaml))
				if perr != nil {
					t.Fatal(perr)
				}
				for _, is := range Validate(f, nil) {
					t.Errorf("loader: %s", is)
				}
			}
		})
	}
}
