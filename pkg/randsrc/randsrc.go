// Package randsrc is the one source of the unguessable values the agent mints: approval
// codes and button nonces, prompt fence nonces and the entropy of proactive IDs. It draws
// from crypto/rand. Only a sandbox-locked process can change that: sandbox.Lock calls Arm
// and the lab wiring then calls Seed, after which Read is a deterministic stream, so two
// runs of one scenario mint the same codes. A seeded code is guessable by design; an
// unarmed process (every production process) can never reach that state, because Seed
// refuses until Arm ran and nothing else arms. Leaf package: stdlib imports only.
package randsrc

import (
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"sync"
	"testing"
)

var (
	mu     sync.Mutex
	armed  bool
	stream *rand.ChaCha8 // nil until Seed; ChaCha8 is not safe for concurrent use, so mu guards it
)

// Read fills b like crypto/rand.Read, unless Arm and Seed both ran: then from the seeded stream.
func Read(b []byte) (int, error) {
	mu.Lock()
	if stream != nil {
		n, _ := stream.Read(b)
		mu.Unlock()
		return n, nil
	}
	mu.Unlock()
	return crand.Read(b)
}

// Arm allows Seed. It cannot be undone outside a test binary and is called only by sandbox.Lock.
func Arm() {
	mu.Lock()
	armed = true
	mu.Unlock()
}

// Seed switches Read to a deterministic stream of seed. It is an error unless Arm ran, and
// can be called once per process. Only the lab wiring calls it.
func Seed(seed int64) error {
	mu.Lock()
	defer mu.Unlock()
	if !armed {
		return errors.New("randsrc: seeding is allowed only in a sandbox-locked process")
	}
	if stream != nil {
		return errors.New("randsrc: already seeded")
	}
	var key [32]byte
	binary.LittleEndian.PutUint64(key[:], uint64(seed))
	stream = rand.NewChaCha8(key)
	return nil
}

// ResetForTest disarms and unseeds. Test binaries only (sandbox.ResetLockForTest calls it).
func ResetForTest() {
	if !testing.Testing() {
		panic("randsrc: ResetForTest is a test seam and may only run in a test binary")
	}
	mu.Lock()
	armed, stream = false, nil
	mu.Unlock()
}
