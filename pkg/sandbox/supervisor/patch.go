package supervisor

import (
	"fmt"
	"net"
	"net/url"
	"path"
	"strings"
	"unicode"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// PatchIssue is one thing a config patch may not do. Path is the keys down to it.
type PatchIssue struct {
	Path []string
	Msg  string
}

// deniedPaths are config keys the supervisor and the lock own. A patch that names one
// is refused before it is written: the gateway port or token would strand the supervisor,
// and the sandbox switch and fixtures directory belong to the lock. (The gateway
// re-enforces the rest on every reload: a non-sim model or a workspace outside the home
// exits the instance with code 3. A real chat channel is only not built there, so
// CheckConfigPatch refuses it itself.)
var deniedPaths = [][]string{{"gateway"}, {"debug", "sandbox"}, {"agents", "defaults", "workspace"}, {"providers"}}

// deniedKeys is deniedPaths as joined keys, for lookup.
var deniedKeys = func() map[string]bool {
	m := map[string]bool{}
	for _, d := range deniedPaths {
		m[foldedKey(d)] = true
	}
	return m
}()

// foldedKey joins the folded segments of a path.
func foldedKey(parts []string) string {
	f := make([]string, len(parts))
	for i, s := range parts {
		f[i] = fold(s)
	}
	return strings.Join(f, "\x00")
}

// objectOnly are the paths that must stay objects when a patch names them: the ancestors
// of a denied path, channels, and the one channel that exists. A null or a scalar there
// would delete or replace the protected subtree under it without naming it.
var objectOnly = func() map[string]bool {
	m := map[string]bool{foldedKey([]string{"channels"}): true, foldedKey([]string{"channels", "simchat"}): true}
	for _, d := range deniedPaths {
		for n := 1; n < len(d); n++ {
			m[foldedKey(d[:n])] = true
		}
	}
	return m
}()

// fold is the case folding encoding/json matches keys with (the config is decoded by it,
// so {"GATEWAY": ...} sets gateway): the smallest rune of each rune's folding orbit.
func fold(s string) string {
	return strings.Map(func(r rune) rune {
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			least = min(least, f)
		}
		return least
	}, s)
}

// The folded spellings the checks compare against.
var foldChannels, foldSimchat, foldEnabled = fold("channels"), fold("simchat"), fold("enabled")

// credentialKeys is config.CredentialFieldNames, folded once.
var credentialKeys = func() map[string]bool {
	m := map[string]bool{}
	for k := range config.CredentialFieldNames {
		m[fold(k)] = true
	}
	return m
}()

// CheckConfigPatch reports what a scenario's config overlay (instance.config,
// config.patch) may not carry: a key the lock owns, a credential field, or a URL that
// does not point at this machine. Keys are compared as encoding/json compares them, so
// without regard to case. Scenarios are data; none may name a real service.
func CheckConfigPatch(patch map[string]any) []PatchIssue {
	var out []PatchIssue
	var walk func(v any, p, fp []string)
	walk = func(v any, p, fp []string) {
		key := strings.Join(fp, "\x00")
		if deniedKeys[key] {
			out = append(out, PatchIssue{p, "this key belongs to the sandbox lock and cannot be patched"})
			return
		}
		if _, isObj := v.(map[string]any); !isObj && objectOnly[key] {
			out = append(out, PatchIssue{p, "must be an object: a null or a value here would replace keys the sandbox lock owns"})
			return
		}
		switch t := v.(type) {
		case map[string]any:
			for k, e := range t {
				fk := fold(k)
				// The gateway does not refuse a real chat channel under the lock, it just
				// does not build it, which a scenario would take for a working setup.
				if len(fp) == 1 && fp[0] == foldChannels && fk != foldSimchat && !channelOff(e) {
					out = append(out, PatchIssue{append(append([]string{}, p...), k), "only the simchat channel exists under the sandbox lock"})
					continue
				}
				if credentialKeys[fk] {
					out = append(out, PatchIssue{append(append([]string{}, p...), k), "a scenario cannot carry a credential"})
					continue
				}
				walk(e, append(append([]string{}, p...), k), append(append([]string{}, fp...), fk))
			}
		case []any:
			for i, e := range t {
				i := fmt.Sprint(i)
				walk(e, append(append([]string{}, p...), i), append(append([]string{}, fp...), i))
			}
		case string:
			if !strings.Contains(t, "://") {
				return
			}
			if u, err := url.Parse(t); err != nil || !isLoopbackHost(u.Hostname()) {
				out = append(out, PatchIssue{p, "a scenario cannot name a URL that is not on this machine"})
			}
		}
	}
	walk(patch, nil, nil)
	return out
}

// channelOff says whether a channel entry switches the channel off explicitly.
func channelOff(v any) bool {
	m, _ := v.(map[string]any)
	off := false
	for k, e := range m {
		if fold(k) == foldEnabled {
			if e != false { // a duplicate spelling that says otherwise may be the one that wins
				return false
			}
			off = true
		}
	}
	return off
}

func isLoopbackHost(h string) bool {
	ip := net.ParseIP(h)
	return strings.EqualFold(h, "localhost") || (ip != nil && ip.IsLoopback())
}

// CheckWorkspacePath says whether p is a file path that stays under <home>/workspace:
// relative, no "..", not empty.
func CheckWorkspacePath(p string) error {
	switch {
	case p == "" || strings.ContainsAny(p, "\x00\\"):
		return fmt.Errorf("workspace path %q is empty or has a NUL or backslash", p)
	case strings.HasPrefix(p, "/") || strings.Contains(p, ":"):
		return fmt.Errorf("workspace path %q must be relative", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("workspace path %q must not contain ..", p)
		}
	}
	if c := path.Clean(p); c == "." || strings.HasPrefix(c, "../") {
		return fmt.Errorf("workspace path %q is not a file under the workspace", p)
	}
	return nil
}
