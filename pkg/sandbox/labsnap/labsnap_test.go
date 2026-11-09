package labsnap

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// mkSnap writes a snapshot "s-1" with one file and returns its directory.
func mkSnap(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "s-1")
	if err := os.MkdirAll(filepath.Join(dir, HomeDir, "lab"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, HomeDir, "lab", "boot")
	if err := os.WriteFile(f, []byte("3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, n, _ := SumFile(f)
	b, _ := json.Marshal(Manifest{ID: "s-1", BinaryVersion: "v1", Files: []FileEntry{{Path: "lab/boot", SHA256: sum, Bytes: n}}})
	if err := os.WriteFile(filepath.Join(dir, ManifestName), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestVerify_RefusesWhatDoesNotMatch(t *testing.T) {
	dir := mkSnap(t)
	if _, err := Verify(dir, "v1"); err != nil {
		t.Fatalf("a good snapshot: %v", err)
	}
	if _, err := Verify(dir, "v2"); !errors.Is(err, ErrMismatch) {
		t.Errorf("another binary: %v, want ErrMismatch", err)
	}
	for name, tamper := range map[string]func(){
		"changed file":  func() { _ = os.WriteFile(filepath.Join(dir, HomeDir, "lab", "boot"), []byte("9\n"), 0o600) },
		"extra file":    func() { _ = os.WriteFile(filepath.Join(dir, HomeDir, "extra"), []byte("x"), 0o600) },
		"link":          func() { _ = os.Symlink("/etc/hosts", filepath.Join(dir, HomeDir, "lnk")) },
		"stray at root": func() { _ = os.WriteFile(filepath.Join(dir, "other"), []byte("x"), 0o600) },
	} {
		d := mkSnap(t)
		dir = d
		tamper()
		if _, err := Verify(d, "v1"); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
}

func TestVerify_RefusesNonLocalPaths(t *testing.T) {
	for _, p := range []string{"../x", "/etc/passwd", "a/../../b", "a//b", ""} {
		dir := mkSnap(t)
		b, _ := json.Marshal(Manifest{ID: "s-1", BinaryVersion: "v1", Files: []FileEntry{{Path: p}}})
		if err := os.WriteFile(filepath.Join(dir, ManifestName), b, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Verify(dir, "v1"); err == nil {
			t.Errorf("path %q verified", p)
		}
	}
}

func TestReplaceHome_WholePathsOnly(t *testing.T) {
	in := `{"w":"/t/kq-sbx-a/workspace","o":"/t/kq-sbx-ab/x","e":"/t/kq-sbx-a","p":"/x/t/kq-sbx-a/y"}`
	want := `{"w":"/n/kq-b/workspace","o":"/t/kq-sbx-ab/x","e":"/n/kq-b","p":"/x/t/kq-sbx-a/y"}`
	if got := string(ReplaceHome([]byte(in), "/t/kq-sbx-a", "/n/kq-b")); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestValidID(t *testing.T) {
	for id, ok := range map[string]bool{"default-20261020T0759-1": true, "a": true, "": false, "../a": false, "a/b": false, "a.b": false, "-a": false} {
		if (ValidID(id) == nil) != ok {
			t.Errorf("ValidID(%q) = %v, want %v", id, ValidID(id), ok)
		}
	}
}
