package triggers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

func externalDiff(req any) worldmodel.Diff {
	return worldmodel.Diff{
		Event: events.Event{
			ID: "ex1", Kind: events.KindPerceptionExternalTrigger, Trust: events.TrustPtr(proactive.TrustUntrusted),
		},
		At: noon, Payload: req,
	}
}

func extReq(name, msg, sev string) perception.ExternalTrigger {
	return perception.ExternalTrigger{Name: name, Message: msg, Severity: sev, At: noon}
}

func externalEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, map[string]Factory{KindExternal: ExternalFactory})
	if _, err := e.r.Add(context.Background(), Spec{
		ID: ExternalSpecID, Kind: KindExternal, Mode: ModeLive, Origin: "builtin",
	}); err != nil {
		t.Fatal(err)
	}
	return e
}

func evalExternal(t *testing.T, e *env, req any) []proactive.Candidate {
	t.Helper()
	cs, err := e.r.Evaluate(context.Background(), fakeView{now: noon}, externalDiff(req))
	if err != nil {
		t.Fatal(err)
	}
	e.r.Committed(context.Background(), idsOf(cs))
	return cs
}

// TestExternalTrigger_CappedAtWarning: whatever severity is asked for, the
// candidate is at most Warning, never protective, template-composed and untrusted.
func TestExternalTrigger_CappedAtWarning(t *testing.T) {
	want := map[string]proactive.Severity{
		"": proactive.SeverityNotice, "notice": proactive.SeverityNotice, "info": proactive.SeverityInfo,
		"warning": proactive.SeverityWarning, "WARNING": proactive.SeverityWarning,
		"critical": proactive.SeverityWarning, "Critical": proactive.SeverityWarning,
		"emergency": proactive.SeverityNotice, "   ": proactive.SeverityNotice,
	}
	e := externalEnv(t)
	for asked, sev := range want {
		cs := evalExternal(t, e, extReq("backup-done", "m"+asked, asked))
		if len(cs) != 1 {
			t.Fatalf("severity %q: %d candidates", asked, len(cs))
		}
		c := cs[0]
		if c.Severity != sev || c.Severity > proactive.SeverityWarning {
			t.Errorf("severity %q gave %v, want %v", asked, c.Severity, sev)
		}
		if c.Protective || c.Compose != proactive.ComposeTemplate || c.Trust != proactive.TrustUntrusted {
			t.Errorf("severity %q: protective=%v compose=%v trust=%v", asked, c.Protective, c.Compose, c.Trust)
		}
	}
}

// The title is the validated name, the message is evidence, and the dedup key holds a
// hash of the message and never the message.
func TestExternalTrigger_TitleKeyAndEvidence(t *testing.T) {
	e := externalEnv(t)
	const msg = "NAS backup finished with 2 errors"
	cs := evalExternal(t, e, perception.ExternalTrigger{
		Name: "backup-done", Message: msg, Severity: "warning", Data: json.RawMessage(`{"errors":2}`), At: noon,
	})
	if len(cs) != 1 {
		t.Fatalf("%d candidates", len(cs))
	}
	c := cs[0]
	if c.Title != "backup-done" || c.Subject != "backup-done" {
		t.Errorf("title %q subject %q", c.Title, c.Subject)
	}
	if !strings.HasPrefix(c.DedupKey, "external:backup-done:") || len(c.DedupKey) != len("external:backup-done:")+8 {
		t.Errorf("dedup key = %q", c.DedupKey)
	}
	if strings.Contains(c.DedupKey, "NAS") || strings.Contains(c.Title, "NAS") {
		t.Errorf("the message leaked into the key or title: %q %q", c.DedupKey, c.Title)
	}
	if c.Evidence["message"] != msg {
		t.Errorf("evidence = %v", c.Evidence)
	}
	if d, _ := c.Evidence["data"].(json.RawMessage); string(d) != `{"errors":2}` {
		t.Errorf("evidence data = %v", c.Evidence["data"])
	}
	if c.TriggerID != "external:"+ExternalSpecID || c.DedupWindow <= 0 {
		t.Errorf("trigger id %q, dedup window %v", c.TriggerID, c.DedupWindow)
	}

	// The same message gives the same key; another message another one.
	again := evalExternal(t, e, extReq("backup-done", msg, "info"))
	other := evalExternal(t, e, extReq("backup-done", msg+"!", "info"))
	if again[0].DedupKey != c.DedupKey || other[0].DedupKey == c.DedupKey {
		t.Errorf("keys: %q %q %q", c.DedupKey, again[0].DedupKey, other[0].DedupKey)
	}
}

// A payload the trigger does not know, and a name that is not valid, make no
// candidate and no error: a request must not be able to count against the spec.
func TestExternalTrigger_IgnoresWhatIsNotItsOwn(t *testing.T) {
	e := externalEnv(t)
	for name, payload := range map[string]any{
		"nil":          nil,
		"a string":     "backup-done",
		"a map":        map[string]any{"name": "x"},
		"nil pointer":  (*perception.ExternalTrigger)(nil),
		"bad name":     extReq("Backup Done", "m", ""),
		"empty name":   extReq("", "m", ""),
		"name too big": extReq(strings.Repeat("a", 42), "m", ""),
	} {
		cs, err := e.r.Evaluate(context.Background(), fakeView{now: noon}, externalDiff(payload))
		if err != nil || len(cs) != 0 {
			t.Errorf("%s: %d candidates, %v", name, len(cs), err)
		}
	}
	if st := e.r.Stats(); st.Errors != 0 {
		t.Fatalf("errors = %d", st.Errors)
	}
}

// A pointer payload is as good as a value.
func TestExternalTrigger_AcceptsAPointer(t *testing.T) {
	e := externalEnv(t)
	r := extReq("ping", "", "")
	if cs := evalExternal(t, e, &r); len(cs) != 1 || cs[0].Title != "ping" {
		t.Fatalf("candidates = %+v", cs)
	}
}

// Whatever fits the door's limits, the candidate passes the registry's own checks:
// the evidence is fitted into its bound, so a request cannot make the spec error
// and be switched off after ten of them.
func TestExternalTrigger_NeverFailsOnAValidRequest(t *testing.T) {
	e := externalEnv(t)
	bigData := json.RawMessage(`{"k":"` + strings.Repeat("<", 3000) + `"}`) // 6 bytes each once encoded
	for name, req := range map[string]perception.ExternalTrigger{
		"max message":      extReq("a", strings.Repeat("x", perception.MaxExternalMessageRunes), ""),
		"escaping message": extReq("a", strings.Repeat("<", perception.MaxExternalMessageRunes), ""),
		"wide runes":       extReq("a", strings.Repeat("\U0001F600", perception.MaxExternalMessageRunes), ""),
		"escaping data":    {Name: "a", Message: strings.Repeat("<", 2000), Data: bigData, At: noon},
		"over the limit":   extReq("a", strings.Repeat("x", 5000), ""),
		"invalid utf-8":    extReq("a", "bad \xff\xfe bytes", ""),
		"newlines":         extReq("a", strings.Repeat("line\n", 400), ""),
	} {
		for i := 0; i < MaxConsecutiveErrors+2; i++ {
			cs := evalExternal(t, e, req)
			if len(cs) != 1 {
				t.Fatalf("%s: %d candidates", name, len(cs))
			}
		}
	}
	if st := e.r.Stats(); st.Errors != 0 || st.InvalidCandidates != 0 || st.AutoDisabled != 0 {
		t.Fatalf("stats = %+v", st)
	}
	specs, _ := e.r.List(context.Background(), ListFilter{Kind: KindExternal})
	if len(specs) != 1 || specs[0].Mode != ModeLive {
		t.Fatalf("specs = %+v", specs)
	}
}

// The trigger is deterministic: the same diff and state give the same candidate,
// and the evidence is bounded.
func TestExternalTrigger_Deterministic(t *testing.T) {
	trig, err := ExternalFactory(Spec{ID: ExternalSpecID, Kind: KindExternal, Params: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	d := externalDiff(extReq("job", strings.Repeat("é", 3000), "warning"))
	a, _ := trig.Evaluate(context.Background(), fakeView{now: noon}, d, &State{})
	b, _ := trig.Evaluate(context.Background(), fakeView{now: noon}, d, &State{})
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatalf("not deterministic:\n%s\n%s", ja, jb)
	}
	if n := len([]rune(a[0].Evidence["message"].(string))); n > perception.MaxExternalMessageRunes {
		t.Fatalf("message of %d runes in the evidence", n)
	}
	if a[0].At != noon {
		t.Fatalf("At = %v", a[0].At)
	}
}

func TestExternalFactory_RefusesParametersAndListsOneKind(t *testing.T) {
	if _, err := ExternalFactory(Spec{ID: "x", Kind: KindExternal, Params: json.RawMessage(`{"a":1}`)}); err == nil {
		t.Fatal("parameters were accepted")
	}
	for _, p := range []string{``, `{}`, `null`} {
		tr, err := ExternalFactory(Spec{ID: "x", Kind: KindExternal, Params: json.RawMessage(p)})
		if err != nil {
			t.Fatalf("params %q: %v", p, err)
		}
		if ks := tr.Kinds(); len(ks) != 1 || ks[0] != events.KindPerceptionExternalTrigger || len(tr.Demands()) != 0 {
			t.Fatalf("kinds %v demands %v", ks, tr.Demands())
		}
		if tr.ID() != "external:x" {
			t.Fatalf("id = %q", tr.ID())
		}
	}
}

// Shadow mode marks the candidate; the registry caps trust at the event's.
func TestExternalTrigger_ShadowAndTrust(t *testing.T) {
	e := externalEnv(t)
	e.setGlobal(ModeShadow)
	cs := evalExternal(t, e, extReq("job", "m", ""))
	if len(cs) != 1 || !cs[0].Shadow || cs[0].Trust != proactive.TrustUntrusted {
		t.Fatalf("candidates = %+v", cs)
	}
}

// The quote is the message as plain lines, one per paragraph, within the bounds the
// registry enforces; a longer message ends in a "…" line. The detail stays empty.
func TestExternalQuote(t *testing.T) {
	cases := map[string]string{
		"":                                  "",
		"   \n\r\n  ":                       "",
		"one":                               "one",
		"one\r\n\r\n  two\t words \n three": "one\ntwo words\nthree",
		"a\nb\nc\nd\ne":                     "a\nb\nc\nd\ne",
		"a\nb\nc\nd\ne\nf":                  "a\nb\nc\nd\ne\n…",
		"x\x1b[31my\u200bz\u202eend":        "x[31myzend",
	}
	for in, want := range cases {
		if got := externalQuote(cleanExternalMessage(in)); got != want {
			t.Errorf("quote(%q) = %q, want %q", in, got, want)
		}
	}
	for name, msg := range map[string]string{
		"long lines":    strings.Repeat(strings.Repeat("w", 300)+"\n", 10),
		"wide":          strings.Repeat("\U0001F600", 2000),
		"many short":    strings.Repeat("ab\n", 900),
		"three of 250":  strings.Repeat(strings.Repeat("é", 250)+"\n", 3),
		"five of 250":   strings.Repeat(strings.Repeat("é", 250)+"\n", 5),
		"six of 250":    strings.Repeat(strings.Repeat("é", 250)+"\n", 6),
		"just under":    strings.Repeat("q", 199),
		"one over":      strings.Repeat("q", 201),
		"one 2000 line": strings.Repeat("z", 2000),
	} {
		q := externalQuote(cleanExternalMessage(msg))
		if err := checkQuote(q); err != nil {
			t.Errorf("%s: %v (%d runes)", name, err, len([]rune(q)))
		}
	}
	e := externalEnv(t)
	if c := evalExternal(t, e, extReq("job", "hello", "")); c[0].Detail != "" || c[0].Quote != "hello" {
		t.Fatalf("candidate = %+v", c[0])
	}
}

// The registry refuses a malformed Quote, so a trigger cannot slip text it did not
// clean into the owner's chat.
func TestQuoteCheck(t *testing.T) {
	ok := strings.Repeat("x", 100) + "\n" + strings.Repeat("y", 100) + "\n\u200d\u200c emoji joiners are fine"
	if err := checkQuote(ok); err != nil {
		t.Fatal(err)
	}
	if err := checkQuote(""); err != nil {
		t.Fatal(err)
	}
	for name, q := range map[string]string{
		"too long":    strings.Repeat("x", proactive.MaxQuoteRunes+1),
		"too many":    strings.Repeat("x\n", proactive.MaxQuoteLines) + "x",
		"bad utf8":    "a\xffb",
		"escape":      "a\x1b[31mb",
		"nul":         "a\x00b",
		"tab":         "a\tb",
		"rlo":         "pay \u202e100",
		"isolate":     "x\u2066y",
		"lrm":         "x\u200ey",
		"arabic mark": "x\u061cy",
	} {
		if err := checkQuote(q); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A candidate with a bad Quote is an error against the spec, like a bad Detail.
	e := newEnv(t, map[string]Factory{testKind: factoryOf(testKind, []events.Kind{kindTick}, func(int, *State, worldmodel.Diff) ([]proactive.Candidate, error) {
		c := cand(testKind, "x")
		c.Quote = "pay \u202e100"
		return []proactive.Candidate{c}, nil
	}, nil)})
	e.add("q1", testKind, ModeLive, `{}`)
	if cs := e.eval(kindTick); len(cs) != 0 {
		t.Fatalf("a candidate with a bad quote passed: %+v", cs)
	}
	if e.r.Stats().InvalidCandidates != 1 {
		t.Fatalf("stats = %+v", e.r.Stats())
	}
}

// Bidirectional controls are dropped by the trigger's own cleaning, from the evidence,
// the key's hash and the detail alike; the joiners are kept in the evidence.
func TestExternalTrigger_StripsBidiControls(t *testing.T) {
	e := externalEnv(t)
	var bidi []rune
	for r := rune(0x202A); r <= 0x202E; r++ {
		bidi = append(bidi, r)
	}
	for r := rune(0x2066); r <= 0x2069; r++ {
		bidi = append(bidi, r)
	}
	bidi = append(bidi, 0x200E, 0x200F, 0x061C)
	spoofed := "pay " + string(bidi) + "100"
	a := evalExternal(t, e, extReq("job", spoofed, ""))
	b := evalExternal(t, e, extReq("job", "pay 100", ""))
	if a[0].Evidence["message"] != "pay 100" || a[0].Quote != "pay 100" || a[0].DedupKey != b[0].DedupKey {
		t.Fatalf("candidate = %+v", a[0])
	}
	joined := "family \U0001F468\u200d\U0001F469\u200d\U0001F467 and a\u200cb"
	if c := evalExternal(t, e, extReq("job", joined, "")); c[0].Evidence["message"] != joined {
		t.Fatalf("a joiner was dropped from the evidence: %q", c[0].Evidence["message"])
	}
}
