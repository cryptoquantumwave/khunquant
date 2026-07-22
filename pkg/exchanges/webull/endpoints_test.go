package webull

import (
	"slices"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// TestHostForEnvironmentByRegion is the regression test for the
// wrong-region-host bug: Webull operates entirely separate regional
// brokers, so a Thailand-registered app key is only valid against
// api.webull.co.th, never api.webull.com. Region was previously stored on
// the account config but silently ignored by host resolution.
func TestHostForEnvironmentByRegion(t *testing.T) {
	cases := []struct {
		environment string
		region      string
		wantHost    string
	}{
		{environment: "prod", region: "th", wantHost: "api.webull.co.th"},
		{environment: "prod", region: "TH", wantHost: "api.webull.co.th"},   // case-insensitive
		{environment: "prod", region: " th ", wantHost: "api.webull.co.th"}, // trims whitespace
		{environment: "prod", region: "us", wantHost: "api.webull.com"},
		{environment: "prod", region: "", wantHost: "api.webull.co.th"}, // empty region defaults to TH, the only supported region
		{environment: "prod", region: "hk", wantHost: "api.webull.hk"},
		{environment: "prod", region: "jp", wantHost: "api.webull.co.jp"},
		{environment: "prod", region: "sg", wantHost: "api.webull.com.sg"},
		{environment: "prod", region: "au", wantHost: "api.webull.com.au"},
		{environment: "prod", region: "my", wantHost: "api.webull.com.my"},
		{environment: "prod", region: "uk", wantHost: "api.webull-uk.com"},
		{environment: "prod", region: "eu", wantHost: "api.webull.eu"},
		{environment: "prod", region: "br", wantHost: "api.webull.com"},
		{environment: "prod", region: "mx", wantHost: "api.webull.com"},
		{environment: "prod", region: "za", wantHost: "api.webull.com.au"},
		{environment: "", region: "th", wantHost: "api.webull.co.th"},                  // empty environment behaves like prod
		{environment: "prod", region: "not-a-real-region", wantHost: "api.webull.com"}, // unknown region falls back to US, not an error
		// Sandbox/UAT: only verified for US today, so region is currently
		// ignored and every region shares the same sandbox host.
		{environment: "uat", region: "th", wantHost: uatHost},
		{environment: "sandbox", region: "us", wantHost: uatHost},
	}

	for _, tc := range cases {
		got := HostForEnvironment(tc.environment, tc.region)
		if got != tc.wantHost {
			t.Errorf("HostForEnvironment(%q, %q) = %q, want %q", tc.environment, tc.region, got, tc.wantHost)
		}
	}
}

func TestValidateRegion(t *testing.T) {
	valid := []string{"", "us", "th", "TH", " th ", "hk", "jp", "sg", "au", "my", "uk", "eu", "br", "mx", "za"}
	for _, region := range valid {
		if err := ValidateRegion(region); err != nil {
			t.Errorf("ValidateRegion(%q) = %v, want nil", region, err)
		}
	}

	invalid := []string{"tz", "thh", "usa", "thailand", "x"}
	for _, region := range invalid {
		if err := ValidateRegion(region); err == nil {
			t.Errorf("ValidateRegion(%q) = nil, want error", region)
		}
	}
}

// TestNormalizeRegion pins the availability gate: Thailand is the only
// region this integration is verified against, so everything else is
// rejected rather than silently attempted — except the legacy "us" default,
// which existing configs carry without the user ever having chosen it.
func TestNormalizeRegion(t *testing.T) {
	normalized := map[string]string{
		"":     "th",
		"th":   "th",
		"TH":   "th",
		" th ": "th",
		"us":   "th", // pre-Thailand default, rewritten rather than rejected
		"US":   "th",
	}
	for region, want := range normalized {
		got, err := NormalizeRegion(region)
		if err != nil {
			t.Errorf("NormalizeRegion(%q) returned error %v, want %q", region, err, want)
			continue
		}
		if got != want {
			t.Errorf("NormalizeRegion(%q) = %q, want %q", region, got, want)
		}
	}

	// Known regions that simply aren't wired up yet.
	for _, region := range []string{"hk", "jp", "sg", "au", "my", "uk", "eu", "br", "mx", "za"} {
		if _, err := NormalizeRegion(region); err == nil {
			t.Errorf("NormalizeRegion(%q) = nil error, want 'not available yet'", region)
		}
	}

	// Typos still get the unknown-region error, which lists valid regions.
	for _, region := range []string{"tz", "thh", "thailand"} {
		if _, err := NormalizeRegion(region); err == nil {
			t.Errorf("NormalizeRegion(%q) = nil error, want unknown-region error", region)
		}
	}
}

func TestSupportedAndKnownRegions(t *testing.T) {
	if got := SupportedRegions(); len(got) != 1 || got[0] != "th" {
		t.Errorf("SupportedRegions() = %v, want [th]", got)
	}
	// Every supported region must exist in the host table.
	known := KnownRegions()
	for _, r := range SupportedRegions() {
		if !slices.Contains(known, r) {
			t.Errorf("supported region %q missing from KnownRegions() %v", r, known)
		}
	}
	// The accessors must hand out copies, not the package state.
	KnownRegions()[0] = "mutated"
	if KnownRegions()[0] == "mutated" {
		t.Error("KnownRegions() leaks the underlying slice")
	}
}

func TestNewClientRejectsUnknownRegion(t *testing.T) {
	newAcc := func(region string) config.WebullExchangeAccount {
		return config.WebullExchangeAccount{
			ExchangeAccount: config.ExchangeAccount{
				APIKey: *config.NewSecureString("test-app-key"),
				Secret: *config.NewSecureString("test-app-secret"),
			},
			Region: region,
		}
	}

	if _, err := NewClient(newAcc("tz")); err == nil {
		t.Error("expected NewClient to reject unknown region \"tz\"")
	}
	if _, err := NewClient(newAcc("hk")); err == nil {
		t.Error("expected NewClient to reject not-yet-available region \"hk\"")
	}

	// Empty and legacy-"us" configs resolve to the Thailand host.
	for _, region := range []string{"", "us"} {
		c, err := NewClient(newAcc(region))
		if err != nil {
			t.Fatalf("NewClient(region=%q) = %v, want success", region, err)
		}
		if c.baseURL != "https://api.webull.co.th" {
			t.Errorf("NewClient(region=%q).baseURL = %q, want https://api.webull.co.th", region, c.baseURL)
		}
		if c.region != "th" {
			t.Errorf("NewClient(region=%q).region = %q, want th", region, c.region)
		}
	}
}

// TestNewClientTrimsCredentials guards the other opaque-401 cause: a key or
// secret pasted with a trailing newline signs a different HMAC than Webull
// computes, and the failure is indistinguishable from wrong credentials.
func TestNewClientTrimsCredentials(t *testing.T) {
	acc := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			APIKey: *config.NewSecureString(" test-app-key\n"),
			Secret: *config.NewSecureString("test-app-secret \n"),
		},
		Region: "th",
	}
	c, err := NewClient(acc)
	if err != nil {
		t.Fatalf("NewClient = %v", err)
	}
	if c.signer.appKey != "test-app-key" {
		t.Errorf("signer.appKey = %q, want %q", c.signer.appKey, "test-app-key")
	}
	if c.signer.appSecret != "test-app-secret" {
		t.Errorf("signer.appSecret = %q, want %q", c.signer.appSecret, "test-app-secret")
	}
}

func TestBaseURLForEnvironmentPrependsScheme(t *testing.T) {
	got := BaseURLForEnvironment("prod", "th")
	want := "https://api.webull.co.th"
	if got != want {
		t.Errorf("BaseURLForEnvironment(prod, th) = %q, want %q", got, want)
	}
}
