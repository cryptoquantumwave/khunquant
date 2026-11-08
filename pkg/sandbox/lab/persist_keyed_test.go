package lab

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// keyedPart is a part with a real field and a bookkeeping field that moves on every step.
type keyedPart struct {
	real, tick int
}

func (k *keyedPart) StateKey() string { return "keyed_test" }
func (k *keyedPart) MarshalState() ([]byte, error) {
	return []byte(fmt.Sprintf(`{"real":%d,"tick":%d}`, k.real, k.tick)), nil
}

func (k *keyedPart) MarshalStateKeyed() (state, key []byte, err error) {
	state, _ = k.MarshalState()
	return state, []byte(fmt.Sprintf(`{"real":%d}`, k.real)), nil
}

func (k *keyedPart) RestoreState(data []byte) error {
	_, err := fmt.Sscanf(string(data), `{"real":%d,"tick":%d}`, &k.real, &k.tick)
	return err
}

func fileState(t *testing.T, path string) (os.FileInfo, string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi, string(b)
}

// A part whose only change is its bookkeeping is not written again; a real change is, and
// is on disk by the time Persist returns. After a restore the baseline is what the part
// would save, so the first bookkeeping-only step after it writes nothing either.
func TestPersist_KeyedPartSkipsBookkeepingOnlyChanges(t *testing.T) {
	home := t.TempDir()
	fixtures := filepath.Join(home, "workspace", "sandbox")
	copyFixtures(t, fixtures)
	open := func() (*Lab, *keyedPart) {
		l, err := New(context.Background(), Options{Home: home, FixturesDir: fixtures, Token: testToken})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
		p := &keyedPart{}
		l.Register(p)
		if err := l.Restore(); err != nil {
			t.Fatal(err)
		}
		return l, p
	}
	l, p := open()
	file := l.worldFile("keyed_test")

	p.real = 1
	if err := l.Persist("keyed_test"); err != nil {
		t.Fatal(err)
	}
	fi1, body1 := fileState(t, file)
	if body1 != `{"real":1,"tick":0}` {
		t.Fatalf("file = %s", body1)
	}

	time.Sleep(20 * time.Millisecond)
	p.tick = 7 // bookkeeping only
	if err := l.Persist("keyed_test"); err != nil {
		t.Fatal(err)
	}
	fi2, body2 := fileState(t, file)
	if !fi2.ModTime().Equal(fi1.ModTime()) || body2 != body1 {
		t.Fatalf("a bookkeeping-only step rewrote the file: %s", body2)
	}

	p.real = 2 // a real change carries the bookkeeping along
	if err := l.Persist("keyed_test"); err != nil {
		t.Fatal(err)
	}
	if _, body3 := fileState(t, file); body3 != `{"real":2,"tick":7}` {
		t.Fatalf("file = %s", body3)
	}

	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l2, p2 := open()
	fi4, _ := fileState(t, file)
	time.Sleep(20 * time.Millisecond)
	p2.tick = 99
	if err := l2.Persist("keyed_test"); err != nil {
		t.Fatal(err)
	}
	if fi5, _ := fileState(t, file); !fi5.ModTime().Equal(fi4.ModTime()) {
		t.Fatal("the first bookkeeping-only step after a restore rewrote the file")
	}
}
