package proactive

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSeverityOrdering(t *testing.T) {
	ordered := []Severity{SeverityInfo, SeverityNotice, SeverityWarning, SeverityCritical}
	for i := 1; i < len(ordered); i++ {
		if ordered[i-1] >= ordered[i] {
			t.Errorf("%v must rank below %v", ordered[i-1], ordered[i])
		}
	}
	if SeverityInfo != 0 {
		t.Errorf("SeverityInfo must be the zero value, got %d", SeverityInfo)
	}
	wantNames := []string{"info", "notice", "warning", "critical"}
	for i, s := range ordered {
		if s.String() != wantNames[i] {
			t.Errorf("Severity(%d).String() = %q, want %q", int(s), s.String(), wantNames[i])
		}
	}
	if got := Severity(9).String(); got != "severity(9)" {
		t.Errorf("out-of-range String() = %q", got)
	}
}

func TestParseSeverity(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Severity
	}{
		{"info", SeverityInfo}, {"notice", SeverityNotice}, {"warning", SeverityWarning},
		{"critical", SeverityCritical}, {"CRITICAL", SeverityCritical}, {"  Warning \n", SeverityWarning},
	} {
		got, err := ParseSeverity(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseSeverity(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "warn", "severe", "2", "severity(2)"} {
		if got, err := ParseSeverity(bad); err == nil {
			t.Errorf("ParseSeverity(%q) = %v, want an error", bad, got)
		}
	}
}

func TestEnumJSONRoundTrip(t *testing.T) {
	type holder struct {
		S Severity
		T Trust
		C ComposeMode
	}
	for s := SeverityInfo; s <= SeverityCritical; s++ {
		for tr := TrustUntrusted; tr <= TrustUserAuthored; tr++ {
			for c := ComposeTemplate; c <= ComposeLLM; c++ {
				in := holder{s, tr, c}
				raw, err := json.Marshal(in)
				if err != nil {
					t.Fatal(err)
				}
				var out holder
				if err := json.Unmarshal(raw, &out); err != nil || out != in {
					t.Fatalf("round trip of %s: got %+v, %v", raw, out, err)
				}
			}
		}
	}

	raw, _ := json.Marshal(holder{SeverityWarning, TrustUserAuthored, ComposeLLM})
	if want := `{"S":"warning","T":"user_authored","C":"llm"}`; string(raw) != want {
		t.Errorf("encoded = %s, want %s", raw, want)
	}
}

func TestEnumJSONAcceptsIntegers(t *testing.T) {
	var h struct {
		S Severity
		T Trust
		C ComposeMode
	}
	if err := json.Unmarshal([]byte(`{"S":3,"T":2,"C":1}`), &h); err != nil {
		t.Fatal(err)
	}
	if h.S != SeverityCritical || h.T != TrustSystem || h.C != ComposeLLM {
		t.Errorf("integer form decoded to %+v", h)
	}
	// null leaves the value untouched.
	h.S = SeverityNotice
	if err := json.Unmarshal([]byte(`{"S":null}`), &h); err != nil || h.S != SeverityNotice {
		t.Errorf("null changed the value: %v, %v", h.S, err)
	}
}

func TestEnumJSONRejectsUnknown(t *testing.T) {
	for _, in := range []string{
		`"loud"`, `""`, `4`, `-1`, `1.5`, `true`, `[]`, `{}`, `"3"`,
	} {
		var s Severity
		if err := json.Unmarshal([]byte(in), &s); err == nil {
			t.Errorf("Severity accepted %s as %v", in, s)
		}
		var tr Trust
		if err := json.Unmarshal([]byte(in), &tr); err == nil {
			t.Errorf("Trust accepted %s as %v", in, tr)
		}
		var c ComposeMode
		if err := json.Unmarshal([]byte(in), &c); err == nil {
			t.Errorf("ComposeMode accepted %s as %v", in, c)
		}
	}
	if _, err := json.Marshal(Severity(9)); err == nil {
		t.Error("marshalling an out-of-range Severity must fail")
	}
	if _, err := json.Marshal(Trust(-1)); err == nil {
		t.Error("marshalling an out-of-range Trust must fail")
	}
	if _, err := json.Marshal(ComposeMode(5)); err == nil {
		t.Error("marshalling an out-of-range ComposeMode must fail")
	}
}

func TestEnumText(t *testing.T) {
	b, err := SeverityNotice.MarshalText()
	if err != nil || string(b) != "notice" {
		t.Fatalf("MarshalText = %q, %v", b, err)
	}
	var s Severity
	if err := s.UnmarshalText([]byte("Critical")); err != nil || s != SeverityCritical {
		t.Errorf("UnmarshalText = %v, %v", s, err)
	}
	if err := s.UnmarshalText([]byte("nope")); err == nil {
		t.Error("UnmarshalText accepted an unknown name")
	}
	if _, err := Severity(7).MarshalText(); err == nil {
		t.Error("MarshalText accepted an out-of-range value")
	}

	// As a map key, the text form is used.
	raw, err := json.Marshal(map[Severity]int{SeverityWarning: 2})
	if err != nil || string(raw) != `{"warning":2}` {
		t.Errorf("map key encoding = %s, %v", raw, err)
	}
	var back map[Severity]int
	if err := json.Unmarshal(raw, &back); err != nil || back[SeverityWarning] != 2 {
		t.Errorf("map key decoding = %v, %v", back, err)
	}

	for _, tc := range []struct {
		name string
		fn   func() error
	}{
		{"trust", func() error { var v Trust; return v.UnmarshalText([]byte("exchange")) }},
		{"compose", func() error { var v ComposeMode; return v.UnmarshalText([]byte("llm")) }},
	} {
		if err := tc.fn(); err != nil {
			t.Errorf("%s UnmarshalText: %v", tc.name, err)
		}
	}
	if _, err := Trust(8).MarshalText(); err == nil {
		t.Error("Trust.MarshalText accepted an out-of-range value")
	}
	if _, err := ComposeMode(8).MarshalText(); err == nil {
		t.Error("ComposeMode.MarshalText accepted an out-of-range value")
	}
	var tr Trust
	if err := tr.UnmarshalText([]byte("bogus")); err == nil {
		t.Error("Trust.UnmarshalText accepted an unknown name")
	}
	var cm ComposeMode
	if err := cm.UnmarshalText([]byte("bogus")); err == nil {
		t.Error("ComposeMode.UnmarshalText accepted an unknown name")
	}
}

func TestTrustAndComposeStrings(t *testing.T) {
	for want, v := range map[string]Trust{
		"untrusted": TrustUntrusted, "exchange": TrustExchange, "system": TrustSystem, "user_authored": TrustUserAuthored,
	} {
		if v.String() != want {
			t.Errorf("%d.String() = %q, want %q", int(v), v.String(), want)
		}
		if got, err := ParseTrust(strings.ToUpper(want)); err != nil || got != v {
			t.Errorf("ParseTrust(%q) = %v, %v", want, got, err)
		}
	}
	if _, err := ParseTrust("trusted"); err == nil {
		t.Error("ParseTrust accepted an unknown name")
	}
	if ComposeTemplate.String() != "template" || ComposeLLM.String() != "llm" {
		t.Errorf("compose names: %q %q", ComposeTemplate, ComposeLLM)
	}
	if _, err := ParseComposeMode("prose"); err == nil {
		t.Error("ParseComposeMode accepted an unknown name")
	}
	if got := Trust(9).String(); got != "trust(9)" {
		t.Errorf("out-of-range Trust.String() = %q", got)
	}
}

func TestMinTrust(t *testing.T) {
	for _, tc := range []struct {
		in   []Trust
		want Trust
	}{
		{nil, TrustUntrusted},
		{[]Trust{TrustSystem}, TrustSystem},
		{[]Trust{TrustUserAuthored, TrustSystem}, TrustSystem},
		{[]Trust{TrustUserAuthored, TrustExchange, TrustUntrusted, TrustSystem}, TrustUntrusted},
		{[]Trust{TrustSystem, TrustUserAuthored, TrustExchange}, TrustExchange},
	} {
		if got := MinTrust(tc.in...); got != tc.want {
			t.Errorf("MinTrust(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// A Candidate must survive JSON storage, with the enums readable on disk.
func TestCandidateJSONRoundTrip(t *testing.T) {
	in := Candidate{
		ID: NewID(), TriggerID: "price_cross:7", DedupKey: "k", Severity: SeverityWarning, Protective: true,
		Salience: 0.5, Subject: "BTC/USDT", Title: "t", Evidence: map[string]any{"price": 1.5},
		Trust: TrustExchange, Compose: ComposeLLM, DedupWindow: 90 * time.Minute,
		At: time.Date(2026, 10, 11, 1, 2, 3, 0, time.UTC), Shadow: true,
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"Severity":"warning"`, `"Trust":"exchange"`, `"Compose":"llm"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("%s missing from %s", want, raw)
		}
	}
	var out Candidate
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != in.ID || out.Severity != in.Severity || out.Trust != in.Trust || out.Compose != in.Compose ||
		out.DedupWindow != in.DedupWindow || !out.At.Equal(in.At) || out.Evidence["price"] != 1.5 {
		t.Errorf("round trip lost data: %+v", out)
	}
}
