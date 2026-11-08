package lockflag_test

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lockflag"
)

// The leaf agrees with sandbox.Locked through lock, reset and a refused second lock: it is
// set by sandbox.Lock and by nothing else.
func TestLockflagFollowsSandboxLock(t *testing.T) {
	sandbox.ResetLockForTest()
	t.Cleanup(sandbox.ResetLockForTest)
	if lockflag.Locked() || sandbox.Locked() {
		t.Fatal("a fresh process is locked")
	}
	if err := sandbox.Lock(sandbox.LockOptions{}); err == nil || lockflag.Locked() {
		t.Fatalf("a refused Lock set the flag (err %v)", err)
	}
	if err := sandbox.Lock(sandbox.LockOptions{Token: "t-0123456789abcdef0123456789abcdef"}); err != nil {
		t.Fatal(err)
	}
	if !lockflag.Locked() || !sandbox.Locked() {
		t.Fatalf("after Lock: flag %v, sandbox %v", lockflag.Locked(), sandbox.Locked())
	}
	sandbox.ResetLockForTest()
	if lockflag.Locked() || sandbox.Locked() {
		t.Fatalf("after reset: flag %v, sandbox %v", lockflag.Locked(), sandbox.Locked())
	}
}
