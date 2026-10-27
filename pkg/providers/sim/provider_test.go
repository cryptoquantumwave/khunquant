package sim

import (
	"context"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// lockForTest locks the process and undoes the lock, and any attached engine, after.
func lockForTest(t *testing.T) {
	t.Helper()
	sandbox.ResetLockForTest()
	t.Cleanup(sandbox.ResetLockForTest)
	if err := sandbox.Lock(sandbox.LockOptions{Token: "t-0123456789abcdef0123456789abcdef"}); err != nil {
		t.Fatal(err)
	}
}

func TestProviderWithoutEngineFails(t *testing.T) {
	lockForTest(t)
	attached.Store(nil)
	p, err := NewProvider(ModeScripted)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := p.Chat(context.Background(), nil, nil, "scripted", nil); err == nil || resp != nil {
		t.Fatalf("Chat before Attach = (%v, %v), want an error and no response", resp, err)
	}
	if p.Name() != "sim" || p.GetDefaultModel() != "sim/scripted" {
		t.Errorf("Name %q, default model %q", p.Name(), p.GetDefaultModel())
	}
}

func TestNewProviderRefusals(t *testing.T) {
	sandbox.ResetLockForTest()
	t.Cleanup(sandbox.ResetLockForTest)
	if _, err := NewProvider(ModeScripted); err == nil {
		t.Error("NewProvider must fail on an unlocked process")
	}
	lockForTest(t)
	for _, mode := range []string{"", "replay", "record", "live", "SCRIPTED"} {
		if _, err := NewProvider(mode); err == nil {
			t.Errorf("mode %q must not build", mode)
		}
	}
}

func TestAttachRules(t *testing.T) {
	sandbox.ResetLockForTest()
	t.Cleanup(sandbox.ResetLockForTest)
	attached.Store(nil)
	e := &Engine{}
	if err := Attach(e); err == nil {
		t.Fatal("Attach must refuse on an unlocked process")
	}
	lockForTest(t)
	if err := Attach(nil); err == nil {
		t.Error("Attach(nil) must fail")
	}
	if err := Attach(e); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Detach(e) })
	if err := Attach(&Engine{}); err == nil {
		t.Error("a second engine must be refused while one is attached")
	}
	Detach(&Engine{}) // not the attached one: no effect
	if attached.Load() != e {
		t.Fatal("Detach of another engine removed the attached one")
	}
	Detach(e)
	if err := Attach(&Engine{}); err != nil {
		t.Errorf("after Detach a new engine attaches: %v", err)
	}
}
