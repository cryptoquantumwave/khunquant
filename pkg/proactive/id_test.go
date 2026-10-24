package proactive

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewID_Monotonic(t *testing.T) {
	prev := NewID()
	for i := 0; i < 10000; i++ {
		next := NewID()
		if next <= prev {
			t.Fatalf("ID %d not strictly increasing: %s after %s", i, next, prev)
		}
		prev = next
	}

	const goroutines, per = 8, 1250
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen = make(map[string]struct{}, goroutines*per)
	)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]string, 0, per)
			for i := 0; i < per; i++ {
				local = append(local, NewID())
			}
			for i := 1; i < len(local); i++ {
				if local[i] <= local[i-1] {
					t.Errorf("per-goroutine order broken: %s after %s", local[i], local[i-1])
					return
				}
			}
			mu.Lock()
			defer mu.Unlock()
			for _, id := range local {
				if _, dup := seen[id]; dup {
					t.Errorf("duplicate ID %s", id)
				}
				seen[id] = struct{}{}
			}
		}()
	}
	wg.Wait()
	if len(seen) != goroutines*per {
		t.Errorf("got %d unique IDs, want %d", len(seen), goroutines*per)
	}
}

func TestNewID_Format(t *testing.T) {
	before := time.Now().Add(-time.Millisecond)
	id := NewID()
	after := time.Now().Add(time.Millisecond)

	if len(id) != 26 {
		t.Fatalf("len(%q) = %d, want 26", id, len(id))
	}
	for _, c := range id {
		if !strings.ContainsRune(crockford, c) {
			t.Fatalf("%q has %q outside the Crockford alphabet", id, c)
		}
	}
	if id[0] > '7' {
		t.Errorf("first digit %q overflows 128 bits", id[0])
	}
	ts, ok := IDTime(id)
	if !ok {
		t.Fatalf("IDTime(%q) not ok", id)
	}
	if ts.Before(before) || ts.After(after) {
		t.Errorf("IDTime = %v, want within [%v, %v]", ts, before, after)
	}
	// Lower case decodes too (Crockford is case-insensitive).
	if ts2, ok := IDTime(strings.ToLower(id)); !ok || !ts2.Equal(ts) {
		t.Errorf("lower-case IDTime = %v, %v", ts2, ok)
	}
}

func TestIDTime_Known(t *testing.T) {
	// The ULID spec's example: 01ARZ3NDEKTSV4RRFFQ69G5FAV is 2016-07-30T23:54:10.259Z.
	got, ok := IDTime("01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if !ok || got.UnixMilli() != 1469922850259 {
		t.Errorf("IDTime(spec example) = %v (%d), %v", got, got.UnixMilli(), ok)
	}
	if id := encodeID(1469922850259, [entropyLen]byte{}); id[:10] != "01ARZ3NDEK" {
		t.Errorf("encodeID timestamp part = %q, want 01ARZ3NDEK", id[:10])
	}
}

func TestIDTime_Rejects(t *testing.T) {
	for _, bad := range []string{
		"", "short", strings.Repeat("0", 27), strings.Repeat("0", 25),
		"01ARZ3NDEKTSV4RRFFQ69G5FAI", // I is not in the alphabet
		"01ARZ3NDEKTSV4RRFFQ69G5FAU", // nor is U
		"81ARZ3NDEKTSV4RRFFQ69G5FAV", // timestamp overflows 48 bits
		"01ARZ3NDEKTSV4RRFFQ69G5FA!",
	} {
		if _, ok := IDTime(bad); ok {
			t.Errorf("IDTime(%q) accepted", bad)
		}
	}
}

func TestEntropyIncrement(t *testing.T) {
	e := [entropyLen]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff}
	if !incrementEntropy(&e) || e[9] != 0 || e[8] != 1 {
		t.Errorf("carry failed: %v", e)
	}
	full := [entropyLen]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	if incrementEntropy(&full) {
		t.Error("overflow must be reported")
	}
}

// When the entropy would overflow within one millisecond the generator borrows
// the next millisecond, so order is preserved.
func TestNewID_EntropyOverflowBorrowsMillisecond(t *testing.T) {
	idGen.mu.Lock()
	savedMS, savedE := idGen.ms, idGen.entropy
	// Pretend the last ID was issued in the future so NewID takes the increment path.
	future := uint64(time.Now().Add(time.Hour).UnixMilli())
	idGen.ms = future
	for i := range idGen.entropy {
		idGen.entropy[i] = 0xff
	}
	idGen.mu.Unlock()
	t.Cleanup(func() {
		idGen.mu.Lock()
		idGen.ms, idGen.entropy = savedMS, savedE
		idGen.mu.Unlock()
	})

	id := NewID()
	ts, ok := IDTime(id)
	if !ok || ts.UnixMilli() != int64(future)+1 {
		t.Errorf("IDTime = %v, %v; want %d", ts, ok, future+1)
	}
}
