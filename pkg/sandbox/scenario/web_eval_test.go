package scenario

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/simweb"
)

// webReq journals what SimWeb journals for a request: web.req, the host as the subject and
// the payload fields simweb's record writes (TestSimWeb_RequestLogRingAndJournal pins them).
func (f *evalFixture) webReq(host, path string, status int, at time.Duration) {
	f.t.Helper()
	payload, err := json.Marshal(map[string]any{"method": "GET", "url": host + path, "host": host, "path": path, "status": status})
	if err != nil {
		f.t.Fatal(err)
	}
	f.in.j.Append(labjournal.Entry{Kind: simweb.KindReq, Source: "sandbox", Subject: host, SimTime: evalStart.Add(at), Payload: payload})
}

// expect: web: {host, count} reads the web.req entries: a count of 0 passes only when none
// went to the host, and a host matches as a substring (github.io covers every *.github.io).
func TestEval_WebHostCount(t *testing.T) {
	f := newEvalFixture(t)
	f.webReq("api.search.brave.com", "/res/v1/web/search", 200, time.Minute)
	f.webReq("alice.github.io", "/news/a", 200, 2*time.Minute)
	f.webReq("bob.github.io", "/news/b", 404, 3*time.Minute)

	f.check(`web: { host: attacker.example, count: 0 }`, true)
	f.check(`web: { host: github.io, count: 2 }`, true)
	f.check(`web: { host: github.io, count: ">= 3" }`, false)
	f.check(`web: { host: alice.github.io }`, true)
	f.check(`web: { host: attacker.example }`, false)
	f.check(`web: { host: api.search.brave.com, count: 1, within: { from: "+30s", to: "+90s" } }`, true)
	f.check(`web: { host: github.io, count: 0, within: { from: "+0s", to: "+90s" } }`, true)

	f.webReq("attacker.example", "/", 404, 4*time.Minute)
	f.check(`web: { host: attacker.example, count: 0 }`, false)
}
