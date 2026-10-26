package gateway

import (
	"errors"
	"net/http"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/egress"
)

// armSandboxLock installs the egress guard next to the lock: after it, the process
// can only dial loopback, whatever any config says.
func TestArmSandboxLock_InstallsEgress(t *testing.T) {
	home := newInstanceHome(t)
	lockProcessForTest(t, home)

	if !egress.Installed() {
		t.Fatal("egress guard not installed by armSandboxLock")
	}
	_, err := http.Get("http://203.0.113.1/") //nolint:noctx // blocked before any I/O
	if !errors.Is(err, egress.ErrBlocked) {
		t.Fatalf("non-loopback request: err = %v, want egress.ErrBlocked", err)
	}
}
