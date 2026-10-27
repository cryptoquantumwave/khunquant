package sim

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScriptPersistsAcrossEngines(t *testing.T) {
	store := filepath.Join(t.TempDir(), "lab", "llm_script.json")
	open := func() *Engine {
		e, err := NewEngine(EngineOptions{Store: store})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	e1 := open()
	then := "done"
	err := e1.Put([]Rule{
		{ID: "a", Match: Match{Turn: "user"}, Respond: Respond{Tool: "message", Args: map[string]any{"content": "c"}, ThenText: &then}, Times: 2},
	}, true, "error")
	if err != nil {
		t.Fatal(err)
	}

	e2 := open() // a new Engine on the same store, as after a restart of the instance
	s := e2.Script()
	if len(s.Rules) != 1 || s.Fallback != "error" || s.Rules[0].Times != 2 || *s.Rules[0].Respond.ThenText != "done" ||
		s.Rules[0].Respond.Args["content"] != "c" || s.Rules[0].Hits != 0 {
		t.Fatalf("the restarted engine has %+v", s)
	}
	if info, err := os.Stat(store); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("store %v, %v", info, err)
	}

	// A script that fails validation is not saved, so a restart cannot be poisoned by it.
	if err := e2.Put([]Rule{{ID: "x"}}, true, ""); err == nil {
		t.Fatal("invalid script accepted")
	}
	if got := open().Script(); len(got.Rules) != 1 || got.Rules[0].ID != "a" {
		t.Errorf("an invalid put reached the store: %+v", got)
	}

	// A store that is not valid fails loudly instead of starting with no script.
	if err := os.WriteFile(store, []byte(`{"rules":[{"id":"a"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewEngine(EngineOptions{Store: store}); err == nil {
		t.Error("an invalid saved script was accepted")
	}
	if err := os.WriteFile(store, []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewEngine(EngineOptions{Store: store}); err == nil {
		t.Error("a corrupt store was accepted")
	}
}
