package supervisor

import (
	"fmt"
	"net"
	"net/url"
	"path"
	"strings"

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
// exits the instance with code 3, and a real chat channel is not built.)
var deniedPaths = [][]string{{"gateway"}, {"debug", "sandbox"}, {"agents", "defaults", "workspace"}, {"providers"}}

// CheckConfigPatch reports what a scenario's config overlay (instance.config,
// config.patch) may not carry: a key the lock owns, a credential field, or a URL that
// does not point at this machine. Scenarios are data; none may name a real service.
func CheckConfigPatch(patch map[string]any) []PatchIssue {
	var out []PatchIssue
	var walk func(v any, p []string)
	walk = func(v any, p []string) {
		for _, d := range deniedPaths {
			if len(p) == len(d) && strings.Join(p, "\x00") == strings.Join(d, "\x00") {
				out = append(out, PatchIssue{p, "this key belongs to the sandbox lock and cannot be patched"})
				return
			}
		}
		switch t := v.(type) {
		case map[string]any:
			for k, e := range t {
				if config.CredentialFieldNames[k] {
					out = append(out, PatchIssue{append(append([]string{}, p...), k), "a scenario cannot carry a credential"})
					continue
				}
				walk(e, append(append([]string{}, p...), k))
			}
		case []any:
			for i, e := range t {
				walk(e, append(append([]string{}, p...), fmt.Sprint(i)))
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
	walk(patch, nil)
	return out
}

func isLoopbackHost(h string) bool {
	ip := net.ParseIP(h)
	return h == "localhost" || (ip != nil && ip.IsLoopback())
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
