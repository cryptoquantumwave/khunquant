package webull

import (
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
		{environment: "prod", region: "", wantHost: "api.webull.com"}, // empty region defaults to US
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

func TestNewClientRejectsUnknownRegion(t *testing.T) {
	cfg := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			APIKey: *config.NewSecureString("test-app-key"),
			Secret: *config.NewSecureString("test-app-secret"),
		},
		Region: "tz", // typo for "th"
	}
	if _, err := NewClient(cfg); err == nil {
		t.Fatal("expected NewClient to reject unknown region \"tz\"")
	}
}

func TestBaseURLForEnvironmentPrependsScheme(t *testing.T) {
	got := BaseURLForEnvironment("prod", "th")
	want := "https://api.webull.co.th"
	if got != want {
		t.Errorf("BaseURLForEnvironment(prod, th) = %q, want %q", got, want)
	}
}
