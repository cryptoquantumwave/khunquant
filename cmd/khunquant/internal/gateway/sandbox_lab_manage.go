package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

// maxManageBody caps the response sandbox_manage hands back.
const maxManageBody = 1 << 20

const manageRoutePrefix = "/api/proactive/"

type manageArgs struct {
	Op     string          `json:"op"`
	Method string          `json:"method"`
	Route  string          `json:"route"`
	Body   json.RawMessage `json:"body"`
}

// manageGroup calls the gateway's own /api/proactive/* handlers in process, as the web
// launcher would: through the same guard chain (loopback, the management token, no
// browser headers), the same handlers and the same web-owner rules. The mux is the
// current generation's private one (registerSandboxLab); nothing leaves the process and
// the token never appears in an answer or the journal. Owner-level routes need
// proactive.owner.web_is_owner: they answer 403 web_owner_missing otherwise, as in
// production.
func manageGroup(shared *sharedResources) lab.Group {
	return lab.Group{
		Name: "manage",
		Description: "Call the gateway's /api/proactive/* management routes in process, as the web launcher " +
			"(state, autonomy, proposals, mandates, resume...). The real guards apply: a risk-raising route " +
			"answers 403 unless the web is the owner (proactive.owner.web_is_owner).",
		Ops: []lab.Op{{
			Description: "call a management route",
			Args: json.RawMessage(`{"method":{"type":"string","enum":["GET","POST","PUT","DELETE"]},` +
				`"route":{"type":"string","description":"must start with /api/proactive/, e.g. /api/proactive/state"},` +
				`"body":{"type":"object","description":"JSON request body"}}`),
			Required: []string{"route"},
			Run:      func(ctx context.Context, raw json.RawMessage) (any, error) { return manageCall(ctx, shared, raw) },
		}},
	}
}

// managePath validates a route and returns the request URL: no scheme, host or dot
// segments, a clean path under /api/proactive/, the query kept as given.
func managePath(route string) (*url.URL, error) {
	u, err := url.Parse(route)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" ||
		strings.Contains(route, "..") || strings.Contains(u.Path, "..") || strings.ContainsAny(route, "\\\x00") ||
		u.Path != path.Clean(u.Path) || !strings.HasPrefix(u.Path, manageRoutePrefix) {
		return nil, lab.Errorf(lab.CodeBadArgs, "route %q: only clean paths under %s are allowed", route, manageRoutePrefix)
	}
	u.Fragment = ""
	return u, nil
}

func manageCall(ctx context.Context, shared *sharedResources, raw json.RawMessage) (any, error) {
	var a manageArgs
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return nil, lab.Errorf(lab.CodeBadArgs, "%v", err)
	}
	if a.Method == "" {
		a.Method = http.MethodGet
	}
	if !slices.Contains([]string{"GET", "POST", "PUT", "DELETE"}, a.Method) {
		return nil, lab.Errorf(lab.CodeBadArgs, "method %q: want GET, POST, PUT or DELETE", a.Method)
	}
	u, err := managePath(a.Route)
	if err != nil {
		return nil, err
	}
	setup := labSetupOf(shared)
	if setup == nil || setup.mux == nil {
		return nil, lab.Errorf(lab.CodeUnavailable, "no generation of services is running (a reload is in progress?)")
	}

	var body *bytes.Reader
	if len(a.Body) > 0 && string(a.Body) != "null" {
		body = bytes.NewReader(a.Body)
	} else {
		body = bytes.NewReader(nil)
	}
	// NewRequestWithContext, not httptest: it returns an error where httptest panics.
	req, err := http.NewRequestWithContext(ctx, a.Method, u.String(), body)
	if err != nil {
		return nil, lab.Errorf(lab.CodeBadArgs, "%v", err)
	}
	req.RemoteAddr = "127.0.0.1:1" // what loopbackOnly reads; no Origin or Sec-Fetch-Site, as from local tooling
	req.Header.Set("Authorization", "Bearer "+setup.token)
	if body.Len() > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	w := &manageWriter{h: http.Header{}, status: http.StatusOK}
	setup.mux.ServeHTTP(w, req)

	if j := shared.lab.Journal; j != nil {
		payload, _ := json.Marshal(map[string]any{"method": a.Method, "route": u.Path, "status": w.status})
		j.Append(labjournal.Entry{Kind: "manage.call", Source: "lab", Subject: u.Path, Payload: payload})
	}
	resp := map[string]any{"status": w.status, "web_owner_active": false}
	if cfg := setup.al.GetConfig(); cfg != nil {
		resp["web_owner_active"] = cfg.WebOwnerActive()
	}
	out := bytes.ReplaceAll(w.buf.Bytes(), []byte(setup.token), []byte("[redacted]")) // no route echoes it; a guard, not a feature
	if json.Valid(out) {
		resp["body"] = json.RawMessage(out)
	} else {
		resp["body"] = strings.TrimSpace(string(out))
	}
	if w.truncated {
		resp["body_truncated"] = true
	}
	return resp, nil
}

// manageWriter is the small http.ResponseWriter the private mux answers into: the body
// is kept up to maxManageBody.
type manageWriter struct {
	h         http.Header
	status    int
	buf       bytes.Buffer
	truncated bool
}

func (w *manageWriter) Header() http.Header  { return w.h }
func (w *manageWriter) WriteHeader(code int) { w.status = code }
func (w *manageWriter) Write(b []byte) (int, error) {
	if room := maxManageBody - w.buf.Len(); len(b) > room {
		w.buf.Write(b[:max(room, 0)])
		w.truncated = true
	} else {
		w.buf.Write(b)
	}
	return len(b), nil
}
