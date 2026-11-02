package dca

import (
	"strings"
	"testing"
	"time"
)

// The expiry notice is a template in the owner's language, keyed by the mandate so that
// a later mandate of the same plan is told about too.
func TestMandateExpiredNotice(t *testing.T) {
	ended := time.Date(2027, 4, 12, 9, 0, 0, 0, time.FixedZone("ICT", 7*3600))
	en := MandateExpiredNotice(false, 7, "BTC/THB", "mnd_a", ended, "Asia/Bangkok")
	th := MandateExpiredNotice(true, 7, "BTC/THB", "mnd_a", ended, "Asia/Bangkok")
	for _, n := range []Notice{en, th} {
		for _, want := range []string{"7", "BTC/THB", "2027-04-12 09:00 (Asia/Bangkok)"} {
			if !strings.Contains(n.Body, want) {
				t.Errorf("body %q does not say %q", n.Body, want)
			}
		}
		if n.DedupKey != "dca-mandate-expired:mnd_a" || n.Severity != "notice" || n.Silent {
			t.Errorf("notice = %+v", n)
		}
	}
	if strings.Contains(th.Body, "scheduled job") || !strings.Contains(en.Body, "scheduled job") {
		t.Errorf("language: en %q, th %q", en.Body, th.Body)
	}
}

// Expired is about a mandate running out: revoked is a different ending, and the date
// decides before ExpireMandates has marked the row.
func TestMandateExpired(t *testing.T) {
	now := time.Date(2026, 10, 12, 2, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		status string
		ends   time.Time
		want   bool
	}{
		{"before its end", MandateActive, now.Add(time.Second), false},
		{"at its end, not marked yet", MandateActive, now, true},
		{"marked expired", MandateExpired, now.Add(-time.Hour), true},
		{"revoked after its end", MandateRevoked, now.Add(-time.Hour), false},
	}
	for _, c := range cases {
		if got := (Mandate{Status: c.status, ExpiresAt: c.ends}).Expired(now); got != c.want {
			t.Errorf("%s: Expired = %v, want %v", c.name, got, c.want)
		}
	}
}
