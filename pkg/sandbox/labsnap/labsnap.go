// Package labsnap is the on-disk form of a Sandbox Lab snapshot: a directory holding
// manifest.json and a home/ tree, with a sha256 for every file. The lab writes it
// and the supervisor verifies it before it copies anything into a new home, so
// neither imports the other.
package labsnap

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	ManifestName = "manifest.json"
	HomeDir      = "home" // the snapshot's files, at their path relative to the instance home
	// MaxHomeBytes caps the home a snapshot will take (it fails with "too large").
	MaxHomeBytes = 500 << 20
)

// FileEntry is one file of the snapshot, by its path relative to the instance home.
type FileEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// Manifest describes a snapshot. Home is the home the instance had, so a restore can move
// every path that starts with it. LLM and Seed are the instance's own settings, which a
// restore repeats; the snapshot of a live or record instance is never taken.
type Manifest struct {
	ID            string      `json:"id"`
	Label         string      `json:"label,omitempty"`
	Instance      string      `json:"instance"`
	Home          string      `json:"home"`
	BinaryVersion string      `json:"binary_version"`
	SimTime       time.Time   `json:"sim_time"`
	JournalSeq    int64       `json:"journal_seq"`
	CreatedWall   time.Time   `json:"created_wall"`
	LLM           string      `json:"llm,omitempty"`
	Seed          string      `json:"seed,omitempty"`
	Parts         []string    `json:"parts"` // the saved world parts written for it (exchange, market, faults)
	Files         []FileEntry `json:"files"`
}

var idRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,79}$`)

// ValidID says whether id is one plain path element: no separator, no dot.
func ValidID(id string) error {
	if !idRE.MatchString(id) {
		return fmt.Errorf("snapshot id %q must be letters, digits and '-', at most 80 characters", id)
	}
	return nil
}

// ErrMismatch: the snapshot was taken by another binary, or a file does not match its sum.
var ErrMismatch = errors.New("snapshot: does not match its manifest")

// SumFile is the sha256 and size of the file at path.
func SumFile(path string) (sum string, n int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err = io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// Read loads the manifest of the snapshot in dir and checks it names the directory's own id.
func Read(dir string) (Manifest, error) {
	var m Manifest
	b, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("snapshot: %s is not valid: %w", ManifestName, err)
	}
	if err := ValidID(m.ID); err != nil || m.ID != filepath.Base(dir) {
		return m, fmt.Errorf("snapshot: manifest id %q is not the directory %q: %w", m.ID, filepath.Base(dir), ErrMismatch)
	}
	return m, nil
}

// Verify reads the manifest of dir and checks it against the files: every listed path is a
// plain relative path to a regular file with the recorded sum and size, nothing else is in
// home/ (no unlisted file, no link), and the binary version is version. It touches nothing.
func Verify(dir, version string) (Manifest, error) {
	m, err := Read(dir)
	if err != nil {
		return m, err
	}
	if m.BinaryVersion != version {
		return m, fmt.Errorf("snapshot: taken by %q, this binary is %q: %w", m.BinaryVersion, version, ErrMismatch)
	}
	listed := map[string]bool{}
	for _, f := range m.Files {
		if !filepath.IsLocal(f.Path) || listed[f.Path] || filepath.ToSlash(filepath.Clean(f.Path)) != f.Path {
			return m, fmt.Errorf("snapshot: file path %q is not a plain relative path: %w", f.Path, ErrMismatch)
		}
		listed[f.Path] = true
		p := filepath.Join(dir, HomeDir, filepath.FromSlash(f.Path))
		if st, err := os.Lstat(p); err != nil || !st.Mode().IsRegular() {
			return m, fmt.Errorf("snapshot: %s is missing or not a regular file: %w", f.Path, ErrMismatch)
		}
		if sum, n, err := SumFile(p); err != nil || sum != f.SHA256 || n != f.Bytes {
			return m, fmt.Errorf("snapshot: %s does not match its sum: %w", f.Path, ErrMismatch)
		}
	}
	root := filepath.Join(dir, HomeDir)
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if !d.IsDir() && (!d.Type().IsRegular() || !listed[filepath.ToSlash(rel)]) {
			return fmt.Errorf("snapshot: %s is not in the manifest: %w", rel, ErrMismatch)
		}
		return nil
	})
	if err != nil {
		return m, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return m, err
	}
	for _, e := range ents {
		if n := e.Name(); n != ManifestName && n != HomeDir {
			return m, fmt.Errorf("snapshot: unexpected %s in %s: %w", n, filepath.Base(dir), ErrMismatch)
		}
	}
	return m, nil
}

// Size is the bytes of every file the manifest lists.
func (m Manifest) Size() (n int64) {
	for _, f := range m.Files {
		n += f.Bytes
	}
	return n
}

// ReplaceHome rewrites every occurrence of the path from in b to to, where it stands as a
// whole path: after a delimiter (or the start) and before a separator, a delimiter or the
// end, so kq-sbx-a does not match inside kq-sbx-ab. It returns b itself when there is none.
func ReplaceHome(b []byte, from, to string) []byte {
	if from == "" || from == to {
		return b
	}
	const delim = "/\\\"'\n\r\t ,:;)]}=" // the file's own separators and JSON/YAML punctuation
	var out []byte
	last := 0
	for at := 0; ; {
		i := bytes.Index(b[at:], []byte(from))
		if i < 0 {
			break
		}
		i += at
		end := i + len(from)
		at = end
		if (i > 0 && !strings.ContainsRune(delim, rune(b[i-1]))) || (end < len(b) && !strings.ContainsRune(delim, rune(b[end]))) {
			continue
		}
		out = append(append(out, b[last:i]...), to...)
		last = end
	}
	if out == nil {
		return b
	}
	return append(out, b[last:]...)
}
