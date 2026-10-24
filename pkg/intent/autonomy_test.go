package intent

import (
	"context"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func TestNormalizeAutonomy(t *testing.T) {
	got, err := NormalizeAutonomy(map[string]string{"T1": "default", "T2": "ask", "T3": "off"})
	if err != nil || len(got) != 2 || got["T2"] != "ask" || got["T3"] != "off" {
		t.Fatalf("got %v, %v; want T2 and T3 only (default dropped)", got, err)
	}
	if got, err := NormalizeAutonomy(map[string]string{"T2": "default"}); err != nil || got != nil {
		t.Fatalf("all default = %v, %v; want nil", got, err)
	}
	for _, bad := range []map[string]string{
		{"T0": "ask"}, {"T4": "off"}, {"t3": "ask"}, {"": "ask"},
		{"T3": "act"}, {"T3": "ASK"}, {"T3": ""}, {"T2": "allow"},
	} {
		if _, err := NormalizeAutonomy(bad); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
}

func TestAutonomyRank_OrdersByRestriction(t *testing.T) {
	prev := -1
	for _, s := range []string{AutonomyDefault, AutonomyAsk, AutonomyNotify, AutonomyOff} {
		r, ok := AutonomyRank(s)
		if !ok || r != prev+1 {
			t.Fatalf("%s: rank %d ok=%v, want %d", s, r, ok, prev+1)
		}
		prev = r
	}
	if _, ok := AutonomyRank("act"); ok {
		t.Fatal(`"act" is a setting`)
	}
}

func TestSetAutonomy_IsVersionedWithAuthorAndKeepsTheRest(t *testing.T) {
	needZone(t, "Asia/Bangkok")
	st := newTestStore(t)
	ctx := context.Background()
	if _, err := st.PutProfile(ctx, Profile{Language: "th", NotificationBudgetPerDay: 5}, "owner", proactive.TrustUserAuthored); err != nil {
		t.Fatal(err)
	}
	v, err := st.SetAutonomy(ctx, map[string]string{"T3": "ask"}, "web")
	if err != nil {
		t.Fatal(err)
	}
	latest, ok, _ := st.Latest(ctx)
	if !ok || latest.Version != v || latest.UpdatedBy != "web" || latest.Provenance != proactive.TrustUserAuthored {
		t.Fatalf("latest = %+v, want version %d by web", latest, v)
	}
	if latest.Profile.AutonomyOf("T3") != "ask" || latest.Profile.AutonomyOf("T2") != "default" ||
		latest.Profile.Language != "th" || latest.Profile.NotificationBudgetPerDay != 5 {
		t.Fatalf("profile = %+v", latest.Profile)
	}
	if _, err := st.SetAutonomy(ctx, map[string]string{"T3": "act"}, "web"); err == nil {
		t.Fatal(`SetAutonomy accepted "act"`)
	}
}

// A writer that read the profile before the owner moved the dial cannot put the old
// dial back: PutProfile carries the stored dial over and ignores the one it is given.
func TestPutProfile_CannotRevertOrSetTheDial(t *testing.T) {
	needZone(t, "Asia/Bangkok")
	st := newTestStore(t)
	ctx := context.Background()
	stale, err := st.Profile(ctx) // read before the dial moves
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetAutonomy(ctx, map[string]string{"T2": "off", "T3": "notify"}, "web"); err != nil {
		t.Fatal(err)
	}
	stale.Language = "en"
	if _, err := st.PutProfile(ctx, stale, "someone", proactive.TrustUserAuthored); err != nil {
		t.Fatal(err)
	}
	p, _ := st.Profile(ctx)
	if p.Language != "en" || p.AutonomyOf("T2") != "off" || p.AutonomyOf("T3") != "notify" {
		t.Fatalf("after a stale write: %+v", p)
	}
	// And a profile that carries a dial of its own does not set one.
	p.Autonomy = map[string]string{"T2": "default", "T3": "default"}
	if _, err := st.PutProfile(ctx, p, "someone", proactive.TrustUserAuthored); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Profile(ctx); got.AutonomyOf("T2") != "off" {
		t.Fatalf("PutProfile loosened the dial: %+v", got.Autonomy)
	}
}

// ApplyPatch is how update_profile and the launcher's profile PUT change the profile:
// "autonomy" is not one of its fields, in a patch or in a reset.
func TestApplyPatch_AutonomyIsNotAField(t *testing.T) {
	p := DefaultProfile()
	p.Autonomy = map[string]string{"T3": "off"}
	for _, patch := range []string{`{"autonomy":{"T3":"default"}}`, `{"autonomy":null}`} {
		if _, err := p.ApplyPatch([]byte(patch)); err == nil || !strings.Contains(err.Error(), "unknown profile field(s): autonomy") {
			t.Errorf("%s: err = %v", patch, err)
		}
	}
	got, err := p.ApplyPatch([]byte(`{"language":"en"}`))
	if err != nil || got.AutonomyOf("T3") != "off" {
		t.Fatalf("an unrelated patch lost the dial: %+v, %v", got.Autonomy, err)
	}
	for _, f := range FieldNames() {
		if f == "autonomy" {
			t.Fatal("autonomy is in FieldNames")
		}
	}
}

// A stored dial that does not validate (hand-edited) reads as off, not as default.
func TestDecodeProfile_BadStoredDialReadsAsOff(t *testing.T) {
	p, err := decodeProfile(`{"autonomy":{"T3":"act"}}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, tier := range AutonomyTiers {
		if p.AutonomyOf(tier) != AutonomyOff {
			t.Errorf("%s = %s, want off", tier, p.AutonomyOf(tier))
		}
	}
}
