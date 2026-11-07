package gateway

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/scenario"
)

// A scenario that names the web group, its put step or the web.req journal kind is accepted
// by a lab that has SimWeb, and refused by name (SBX-15) by one that has not.
func TestScenarioWebFeatures_AcceptedWithSimWebRefusedWithout(t *testing.T) {
	f := newLockedFixture(t)
	caps := &scenario.Caps{Groups: map[string][]string{}, Kinds: f.lab.Kinds()}
	for _, g := range f.lab.Groups() {
		for _, o := range g.Ops {
			caps.Groups[g.Name] = append(caps.Groups[g.Name], o.Name)
		}
	}
	if !slices.Equal(caps.Groups["web"], []string{"put", "list", "requests"}) {
		t.Fatalf("the web group's ops are %v", caps.Groups["web"])
	}
	if !scenario.KindDeclared(caps.Kinds, "web.req") {
		t.Fatalf("web.req is not a declared journal kind: %v", caps.Kinds)
	}

	dir := filepath.Join(repoRootDir(t), filepath.FromSlash(scenario.DefaultDir), "web")
	files, _ := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if len(files) < 4 {
		t.Fatalf("found %d web scenarios under %s, want at least 4", len(files), dir)
	}
	without := &scenario.Caps{Groups: map[string][]string{}, Kinds: slices.DeleteFunc(slices.Clone(caps.Kinds), func(k string) bool { return k == "web.req" })}
	for g, ops := range caps.Groups {
		if g != "web" {
			without.Groups[g] = ops
		}
	}
	refused := 0
	for _, name := range files {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := scenario.Parse(strings.NewReader(string(data)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, issue := range scenario.Validate(file, caps) {
			t.Errorf("%s: refused by a lab with SimWeb: %s", filepath.Base(name), issue)
		}
		for _, issue := range scenario.Validate(file, without) {
			if strings.Contains(issue.Msg, "SBX-15") {
				refused++
			}
		}
	}
	if refused == 0 {
		t.Error("no web scenario was refused as needing SBX-15 by a lab without SimWeb: the check proves nothing")
	}
}
