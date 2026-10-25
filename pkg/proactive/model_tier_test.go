package proactive

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseModelTier(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want ModelTier
	}{
		{"primary", ModelTierPrimary}, {"light", ModelTierLight},
		{"", ModelTierPrimary}, {"  \n", ModelTierPrimary},
		{"LIGHT", ModelTierLight}, {" Primary ", ModelTierPrimary},
	} {
		got, err := ParseModelTier(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseModelTier(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"fast", "main", "normal", "light2", "model_tier(\"x\")"} {
		if got, err := ParseModelTier(bad); err == nil {
			t.Errorf("ParseModelTier(%q) = %q, want an error", bad, got)
		}
	}
}

func TestModelTierString(t *testing.T) {
	if ModelTierPrimary != "primary" || ModelTierLight != "light" {
		t.Fatalf("the stored names changed: %q, %q", ModelTierPrimary, ModelTierLight)
	}
	var zero ModelTier
	if zero.String() != "primary" || ModelTierPrimary.String() != "primary" || ModelTierLight.String() != "light" {
		t.Errorf("String: %q, %q, %q", zero, ModelTierPrimary, ModelTierLight)
	}
	got := ModelTier("turbo").String()
	if got != `model_tier("turbo")` {
		t.Errorf("unknown tier String() = %q", got)
	}
	if _, err := ParseModelTier(got); err == nil {
		t.Error("the placeholder for an unknown tier parses back")
	}
	if !zero.Valid() || !ModelTierPrimary.Valid() || !ModelTierLight.Valid() || ModelTier("turbo").Valid() {
		t.Error("Valid: the zero value and the two tiers are valid, anything else is not")
	}
}

func TestModelTierTextAndJSONRoundTrip(t *testing.T) {
	type holder struct{ M ModelTier }
	for _, tier := range []ModelTier{ModelTierPrimary, ModelTierLight} {
		b, err := json.Marshal(holder{tier})
		if err != nil {
			t.Fatal(err)
		}
		if want := `{"M":"` + string(tier) + `"}`; string(b) != want {
			t.Errorf("Marshal = %s, want %s", b, want)
		}
		var out holder
		if err := json.Unmarshal(b, &out); err != nil || out.M != tier {
			t.Errorf("round trip of %q = %+v, %v", tier, out, err)
		}
		txt, err := tier.MarshalText()
		if err != nil || string(txt) != string(tier) {
			t.Errorf("MarshalText(%q) = %q, %v", tier, txt, err)
		}
	}

	// The zero value is primary and is written that way.
	b, err := json.Marshal(holder{})
	if err != nil || string(b) != `{"M":"primary"}` {
		t.Errorf("zero value marshals as %s, %v", b, err)
	}

	// Upper case and padding are accepted on the way in; empty means primary.
	var out holder
	if err := json.Unmarshal([]byte(`{"M":" Light "}`), &out); err != nil || out.M != ModelTierLight {
		t.Errorf("padded input: %+v, %v", out, err)
	}
	if err := json.Unmarshal([]byte(`{"M":""}`), &out); err != nil || out.M != ModelTierPrimary {
		t.Errorf("empty input: %+v, %v", out, err)
	}
	// null leaves the receiver unchanged, as for the other enums.
	out = holder{ModelTierLight}
	if err := json.Unmarshal([]byte(`{"M":null}`), &out); err != nil || out.M != ModelTierLight {
		t.Errorf("null input: %+v, %v", out, err)
	}
}

func TestModelTierRejectsUnknown(t *testing.T) {
	var out struct{ M ModelTier }
	if err := json.Unmarshal([]byte(`{"M":"turbo"}`), &out); err == nil ||
		!strings.Contains(err.Error(), "turbo") {
		t.Errorf("unknown tier accepted: %+v, %v", out, err)
	}
	if err := json.Unmarshal([]byte(`{"M":1}`), &out); err == nil {
		t.Error("an integer was accepted as a tier")
	}
	if _, err := json.Marshal(struct{ M ModelTier }{"turbo"}); err == nil {
		t.Error("an unknown tier marshalled")
	}
	var tier ModelTier
	if err := tier.UnmarshalText([]byte("turbo")); err == nil || tier != "" {
		t.Errorf("UnmarshalText accepted turbo: %q, %v", tier, err)
	}
}
