package scenario

import "testing"

func TestMatrix_Substitution(t *testing.T) {
	f := parseFixture(t, "design_8_3.yaml")
	out, err := Expand(f)
	if err != nil || len(out) != 2 {
		t.Fatalf("Expand = %d files, %v; want 2", len(out), err)
	}
	for i, want := range []string{"template:v1", "template:v2"} {
		g := out[i]
		if g.Instance.Workspace["HEARTBEAT.md"] != want || g.MatrixRow["template"] != want || g.Matrix != nil {
			t.Errorf("file %d = workspace %q, row %v, matrix %v", i, g.Instance.Workspace, g.MatrixRow, g.Matrix)
		}
		if len(g.Timeline) != 2 || g.Runs != 5 {
			t.Errorf("file %d lost its body: %d steps, %d runs", i, len(g.Timeline), g.Runs)
		}
	}

	// A key is never substituted, and neither is a value that merely contains no scalar.
	g := parseString(t, base+"matrix: {n: [a, b]}\ninstance: {workspace: {\"${n}.md\": \"x-${n}\"}}\n")
	files, err := Expand(g)
	if err != nil || len(files) != 2 {
		t.Fatalf("Expand = %v, %v", files, err)
	}
	if got := files[1].Instance.Workspace; got["${n}.md"] != "x-b" {
		t.Errorf("workspace = %v: the key stays, the value is replaced", got)
	}
	if one, _ := Expand(parseString(t, base)); len(one) != 1 {
		t.Error("a file with no matrix expands to itself")
	}
}
