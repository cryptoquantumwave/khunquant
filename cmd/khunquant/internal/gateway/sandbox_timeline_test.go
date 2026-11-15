package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

// doGet sends a GET with the given bearer and extra headers.
func doGet(t *testing.T, url, token string, hdr map[string]string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := testHTTPClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

type timelineBody struct {
	Locked   bool   `json:"locked"`
	Instance string `json:"instance"`
	NextSeq  int64  `json:"next_seq"`
	Entries  []struct {
		Seq       int64           `json:"seq"`
		Kind      string          `json:"kind"`
		Subject   string          `json:"subject"`
		Summary   string          `json:"summary"`
		Payload   json.RawMessage `json:"payload"`
		Truncated bool            `json:"truncated"`
	} `json:"entries"`
}

func getTimeline(t *testing.T, f *lockedFixture, query string) timelineBody {
	t.Helper()
	code, body := doGet(t, f.url("/api/sandbox/timeline"+query), testMgmtToken, nil)
	if code != http.StatusOK {
		t.Fatalf("GET timeline%s = %d %s", query, code, body)
	}
	var out timelineBody
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return out
}

func TestTimelineNotRegisteredWhenUnlocked(t *testing.T) {
	f := newAssemblyFixture(t, nil)
	for _, tok := range []string{testMgmtToken, ""} {
		if code, body := doGet(t, f.url("/api/sandbox/timeline"), tok, nil); code != http.StatusNotFound {
			t.Errorf("unlocked gateway, token %q: GET timeline = %d %s, want 404", tok, code, body)
		}
	}
}

// The timeline is read with the config's management token (the view token), never with
// the instance token, and the view token opens no other route.
func TestTimelineGuard(t *testing.T) {
	f := newLockedFixture(t)
	url := f.url("/api/sandbox/timeline")
	for _, tc := range []struct {
		name, token string
		hdr         map[string]string
		want        int
	}{
		{"no token", "", nil, http.StatusUnauthorized},
		{"wrong token", testMgmtToken + "x", nil, http.StatusUnauthorized},
		{"the lab instance token", lockTestToken, nil, http.StatusUnauthorized},
		{"view token", testMgmtToken, nil, http.StatusOK},
		{"Origin header", testMgmtToken, map[string]string{"Origin": "http://evil.example"}, http.StatusForbidden},
		{"cross-site fetch", testMgmtToken, map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
	} {
		if code, body := doGet(t, url, tc.token, tc.hdr); code != tc.want {
			t.Errorf("%s: GET = %d %s, want %d", tc.name, code, body, tc.want)
		}
	}
	// Read-only: no write verb is registered.
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+testMgmtToken)
	if resp, err := testHTTPClient.Do(req); err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST timeline: %v %v, want 405", err, resp)
	}

	// The view token must not be a management token of the locked process.
	if code, _ := doGet(t, f.url("/api/proactive/state"), testMgmtToken, nil); code != http.StatusUnauthorized {
		t.Errorf("view token on GET /api/proactive/state = %d, want 401", code)
	}
	if code, _ := doGet(t, f.url("/api/proactive/state"), lockTestToken, nil); code != http.StatusOK && code != http.StatusServiceUnavailable {
		t.Errorf("instance token on GET /api/proactive/state = %d, want it accepted (the lock's management token)", code)
	}
	if code, _, _ := postJSON(f.url("/api/proactive/resume"), testMgmtToken, `{}`); code != http.StatusUnauthorized {
		t.Errorf("view token on POST /api/proactive/resume = %d, want 401", code)
	}

	// A caller that is not on loopback is refused before the token is looked at.
	mux := http.NewServeMux()
	registerSandboxTimeline(mux, f.lab.Journal, testMgmtToken)
	r := httptest.NewRequest(http.MethodGet, "/api/sandbox/timeline", nil)
	r.RemoteAddr = "203.0.113.7:4444"
	r.Header.Set("Authorization", "Bearer "+testMgmtToken)
	if rec := httptest.NewRecorder(); func() int { mux.ServeHTTP(rec, r); return rec.Code }() != http.StatusForbidden {
		t.Error("non-loopback caller was not refused")
	}
	// No view token configured: every request is refused, the bearer middleware lets all through.
	mux = http.NewServeMux()
	registerSandboxTimeline(mux, f.lab.Journal, "")
	r = httptest.NewRequest(http.MethodGet, "/api/sandbox/timeline", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	if rec := httptest.NewRecorder(); func() int { mux.ServeHTTP(rec, r); return rec.Code }() != http.StatusServiceUnavailable {
		t.Error("an empty view token was not refused with 503")
	}
}

// A reload rebuilds the mux and registers the route again, so it takes the token the
// reloaded file holds: the old one stops working, the new one reads.
func TestTimelineViewTokenFollowsReload(t *testing.T) {
	f := newLockedFixture(t)
	const next = "reloaded-view-token-0123456789abcdef"
	if err := f.reload(func(c *config.Config) { c.Gateway.ManagementToken = next }); err != nil {
		t.Fatal(err)
	}
	url := f.url("/api/sandbox/timeline")
	if code, _ := doGet(t, url, next, nil); code != http.StatusOK {
		t.Errorf("new view token after reload = %d, want 200", code)
	}
	if code, _ := doGet(t, url, testMgmtToken, nil); code != http.StatusUnauthorized {
		t.Errorf("old view token after reload = %d, want 401", code)
	}
}

// A file token equal to the instance token is never a view token.
func TestTimelineViewTokenNeverTheInstanceToken(t *testing.T) {
	t.Cleanup(func() { captureLabViewToken("", "") })
	captureLabViewToken(lockTestToken, lockTestToken)
	if got := labViewToken(); got != "" {
		t.Errorf("view token = %q, want none", got)
	}
	captureLabViewToken("file-token", lockTestToken)
	if got := labViewToken(); got != "file-token" {
		t.Errorf("view token = %q, want the file's", got)
	}
}

func TestTimelineCursorAndKinds(t *testing.T) {
	f := newLockedFixture(t)
	j := f.lab.Journal
	mk := func(kind, subj, text string) {
		p, _ := json.Marshal(map[string]any{"text": text, "delivered": true})
		j.Append(labjournal.Entry{Kind: kind, Subject: subj, Payload: p})
	}
	mk("chat.out", "dm:a", "one")
	mk("llm.call", "user", "two")
	mk("chat.in", "dm:a", "three")
	mk("chat.out", "dm:a", "four")

	all := getTimeline(t, f, "?kinds=chat.*&limit=500")
	var seqs []int64
	for _, e := range all.Entries {
		if !strings.HasPrefix(e.Kind, "chat.") {
			t.Errorf("kinds=chat.* returned %s", e.Kind)
		}
		if e.Subject == "dm:a" {
			seqs = append(seqs, e.Seq)
		}
	}
	if len(seqs) != 3 || !all.Locked || all.Instance == "" {
		t.Fatalf("chat entries for dm:a = %v locked=%v instance=%q, want 3", seqs, all.Locked, all.Instance)
	}
	if all.NextSeq != all.Entries[len(all.Entries)-1].Seq {
		t.Errorf("next_seq = %d, want the last seq", all.NextSeq)
	}
	if got := all.Entries[len(all.Entries)-1].Summary; !strings.Contains(got, "four") {
		t.Errorf("summary of the last chat.out = %q", got)
	}
	// Paging by the cursor returns the rest; an empty page keeps the cursor.
	rest := getTimeline(t, f, "?kinds=chat.*&after_seq="+seqStr(seqs[1]))
	if len(rest.Entries) != 1 || rest.Entries[0].Seq != seqs[2] || rest.NextSeq != seqs[2] {
		t.Errorf("after_seq page = %+v", rest)
	}
	none := getTimeline(t, f, "?kinds=chat.*&after_seq="+seqStr(seqs[2]))
	if len(none.Entries) != 0 || none.NextSeq != seqs[2] {
		t.Errorf("empty page: %d entries, next_seq %d, want 0 and %d", len(none.Entries), none.NextSeq, seqs[2])
	}
	if two := getTimeline(t, f, "?kinds=chat.*&limit=2"); len(two.Entries) > 2 {
		t.Errorf("limit=2 returned %d entries", len(two.Entries))
	}
	for _, q := range []string{"?limit=0", "?limit=501", "?limit=x", "?after_seq=-1", "?after_seq=1e3", "?kinds=Chat.*", "?kinds=a%20b"} {
		if code, _ := doGet(t, f.url("/api/sandbox/timeline"+q), testMgmtToken, nil); code != http.StatusBadRequest {
			t.Errorf("GET timeline%s = %d, want 400", q, code)
		}
	}
}

func seqStr(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestTimelinePayloadCapped(t *testing.T) {
	f := newLockedFixture(t, func(c *config.Config) {})
	big, _ := json.Marshal(map[string]any{"text": strings.Repeat("<é&", 4000)}) // ~ 10 KiB, grows when escaped
	f.lab.Journal.Append(labjournal.Entry{Kind: "chat.in", Subject: "dm:big", Payload: big})
	f.lab.Journal.Append(labjournal.Entry{Kind: "chat.in", Subject: "dm:small", Payload: json.RawMessage(`{"text":"hi"}`)})
	if len(big) <= timelinePayloadMax {
		t.Fatalf("test payload is %d bytes, want over the cap", len(big))
	}
	var sawBig, sawSmall bool
	for _, e := range getTimeline(t, f, "?kinds=chat.in").Entries {
		switch e.Subject {
		case "dm:big":
			sawBig = true
			if len(e.Payload) > timelinePayloadMax || !e.Truncated || !json.Valid(e.Payload) {
				t.Errorf("big payload: %d bytes truncated=%v valid=%v", len(e.Payload), e.Truncated, json.Valid(e.Payload))
			}
			if !strings.Contains(e.Summary, "<é&") {
				t.Errorf("summary was not taken from the whole payload: %q", e.Summary)
			}
		case "dm:small":
			sawSmall = true
			if e.Truncated || string(e.Payload) != `{"text":"hi"}` {
				t.Errorf("small payload = %s truncated=%v", e.Payload, e.Truncated)
			}
		}
	}
	if !sawBig || !sawSmall {
		t.Fatalf("entries missing: big=%v small=%v", sawBig, sawSmall)
	}
}

// The real path, as the launcher takes it: the profile `sandbox up` writes holds the view
// token, a re-executed locked gateway serves the timeline to it and refuses the instance
// token, and the chat the test sent is on the timeline.
func TestTimelineReadWithTheProfilesViewToken(t *testing.T) {
	home := newInstanceHome(t)
	t.Setenv("KHUNQUANT_HOME", home)
	t.Setenv("KHUNQUANT_CONFIG", filepath.Join(home, "config.json"))
	t.Setenv("KQ_SANDBOX_TOKEN", "")
	childHome, childTmp := t.TempDir(), t.TempDir()
	env := []string{
		"PATH=" + os.Getenv("PATH"), "TMPDIR=" + childTmp, "HOME=" + childHome,
		"KHUNQUANT_HOME=" + home, "KHUNQUANT_CONFIG=" + home + "/config.json",
		"KQ_SANDBOX_LOCKED=1", "KQ_SANDBOX_TOKEN=" + lockTestToken, "KQ_SANDBOX_INSTANCE=timeline-e2e",
	}
	p := startLocked(t, home, env, []string{home, childHome, childTmp}, func(port int) {
		if _, err := sandbox.WriteProfile(sandbox.ProfileOptions{Home: home, Port: port}); err != nil {
			t.Fatal(err)
		}
	})
	p.waitOutput("Channels enabled: [simchat]", 20*time.Second)

	fileCfg, err := config.LoadConfig(filepath.Join(home, "config.json"))
	if err != nil || len(fileCfg.Gateway.ManagementToken) < 32 {
		t.Fatalf("the profile's view token: %v (%d chars)", err, len(fileCfg.Gateway.ManagementToken))
	}
	view := fileCfg.Gateway.ManagementToken
	if code, body, err := postJSON(p.url("/api/sandbox/lab/chat"), lockTestToken, `{"op":"send","as":"stranger","text":"/whoami"}`); err != nil || code != http.StatusOK {
		t.Fatalf("POST chat send = %d (%v) %s", code, err, body)
	}
	if code, _ := doGet(t, p.url("/api/sandbox/timeline"), lockTestToken, nil); code != http.StatusUnauthorized {
		t.Errorf("the instance token on the timeline = %d, want 401", code)
	}
	if code, _ := doGet(t, p.url("/api/proactive/state"), view, nil); code != http.StatusUnauthorized {
		t.Errorf("the view token on a management read = %d, want 401", code)
	}
	var got timelineBody
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		code, body := doGet(t, p.url("/api/sandbox/timeline?kinds=chat.*"), view, nil)
		if code != http.StatusOK {
			t.Fatalf("timeline with the view token = %d %s", code, body)
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Entries) >= 2 { // chat.in and its chat.out
			break
		}
	}
	if !got.Locked || got.Instance != "timeline-e2e" || len(got.Entries) < 2 {
		t.Fatalf("timeline = locked %v instance %q, %d chat entries\n%s", got.Locked, got.Instance, len(got.Entries), p.out.String())
	}
	if !strings.Contains(got.Entries[0].Summary, "/whoami") {
		t.Errorf("first chat entry summary = %q", got.Entries[0].Summary)
	}
}

// The instance (lab) token and the view token are masked as *** in the response, in
// payloads and in summaries, as the supervisor masks the token in everything it relays.
func TestTimelineMasksBothTokens(t *testing.T) {
	f := newLockedFixture(t)
	j := f.lab.Journal
	for _, text := range []string{
		"debugging with " + lockTestToken + " pasted",
		"and " + testMgmtToken + " as well",
		lockTestToken + testMgmtToken,
	} {
		p, _ := json.Marshal(map[string]any{"text": text, "delivered": true})
		j.Append(labjournal.Entry{Kind: "chat.out", Subject: "dm:a", Payload: p})
	}
	code, body := doGet(t, f.url("/api/sandbox/timeline?kinds=chat.*&limit=500"), testMgmtToken, nil)
	if code != http.StatusOK {
		t.Fatalf("GET timeline = %d %s", code, body)
	}
	for name, tok := range map[string]string{"instance token": lockTestToken, "view token": testMgmtToken} {
		if tok == "" || strings.Contains(string(body), tok) {
			t.Errorf("the response holds the %s: %s", name, body)
		}
	}
	var out timelineBody
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	var masked int
	for _, e := range out.Entries {
		if strings.Contains(string(e.Payload), "***") && strings.Contains(e.Summary, "***") {
			masked++
		}
	}
	if masked != 3 {
		t.Errorf("%d entries carry *** in both payload and summary, want 3: %s", masked, body)
	}
}

// tokenFragment returns the first n consecutive characters of tok that body holds.
func tokenFragment(body, tok string, n int) (string, bool) {
	for i := 0; i+n <= len(tok); i++ {
		if strings.Contains(body, tok[i:i+n]) {
			return tok[i : i+n], true
		}
	}
	return "", false
}

// A token that a cut splits is masked too: the summary cut (PR6-1) and the payload cap
// leave no part of either token, where the whole-body mask alone would miss a prefix.
func TestTimelineMasksATokenAcrossACut(t *testing.T) {
	f := newLockedFixture(t)
	j := f.lab.Journal
	const (
		subject = "dm:cut"
		keep    = 8 // characters of the token the unmasked summary would keep
	)
	for _, tok := range []string{lockTestToken, testMgmtToken} {
		// "dm:cut: xxx…<token>", the token starting keep runes before the summary cut.
		text := strings.Repeat("x", labjournal.SummaryMax-1-keep-len(subject+": ")) + tok
		p, _ := json.Marshal(map[string]any{"text": text, "delivered": true})
		e := labjournal.Entry{Kind: "chat.out", Subject: subject, Payload: p}
		if s := labjournal.Summary(e); !strings.HasSuffix(s, tok[:keep]+"…") {
			t.Fatalf("the test line does not put the token across the summary cut: %q", s)
		}
		j.Append(e)
	}
	// Tokens all along a payload that is over the cap even when masked: one is at the cut.
	long, _ := json.Marshal(map[string]any{"text": strings.Repeat(lockTestToken+" "+testMgmtToken+" ", 1200)})
	j.Append(labjournal.Entry{Kind: "chat.in", Subject: "dm:long", Payload: long})

	code, body := doGet(t, f.url("/api/sandbox/timeline?kinds=chat.*&limit=500"), testMgmtToken, nil)
	if code != http.StatusOK {
		t.Fatalf("GET timeline = %d %s", code, body)
	}
	for name, tok := range map[string]string{"instance token": lockTestToken, "view token": testMgmtToken} {
		if frag, ok := tokenFragment(string(body), tok, keep); ok {
			t.Errorf("the response holds %q of the %s", frag, name)
		}
	}
	var out timelineBody
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	var cutSummaries int
	var sawLong bool
	for _, e := range out.Entries {
		switch e.Subject {
		case subject:
			if strings.HasSuffix(e.Summary, "x***") {
				cutSummaries++
			} else {
				t.Errorf("summary of a line with a token across the cut = %q, want it to end in ***", e.Summary)
			}
		case "dm:long":
			sawLong = true
			if !e.Truncated || len(e.Payload) > timelinePayloadMax || !strings.Contains(string(e.Payload), "***") {
				t.Errorf("long payload: %d bytes truncated=%v, want it cut and masked", len(e.Payload), e.Truncated)
			}
		}
	}
	if cutSummaries != 2 || !sawLong {
		t.Errorf("%d masked summaries (want 2), long payload seen=%v: %s", cutSummaries, sawLong, body)
	}
}
