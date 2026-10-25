package proactive

import (
	"crypto/rand"
	"sync"
	"time"
)

// crockford is the Crockford base32 alphabet used by ULIDs: no I, L, O or U,
// and in ascending ASCII order so that string order equals numeric order.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

const (
	idLen      = 26 // 10 time chars + 16 entropy chars
	entropyLen = 10 // 80 bits
	maxMillis  = 1<<48 - 1
)

// idGen holds the monotonic state. ULIDs minted in the same millisecond must
// still sort in creation order, so the entropy is incremented rather than
// redrawn.
var idGen struct {
	mu      sync.Mutex
	ms      uint64
	entropy [entropyLen]byte
}

// NewID returns a new ULID: 26 characters of Crockford base32 made of a 48-bit
// millisecond timestamp followed by 80 bits from crypto/rand. IDs sort
// lexicographically by creation time and are strictly increasing within one
// process, even inside a single millisecond and even if the wall clock steps
// backwards (the generator then keeps the last timestamp it issued).
func NewID() string {
	idGen.mu.Lock()
	defer idGen.mu.Unlock()

	now := uint64(time.Now().UnixMilli())
	if now > idGen.ms {
		idGen.ms = now
		fillEntropy(&idGen.entropy)
	} else if !incrementEntropy(&idGen.entropy) {
		// 2^80 IDs in one millisecond: borrow the next millisecond instead of
		// wrapping, which would break ordering.
		idGen.ms++
		fillEntropy(&idGen.entropy)
	}
	return encodeID(idGen.ms&maxMillis, idGen.entropy)
}

// fillEntropy draws 80 fresh random bits. crypto/rand.Read does not return an
// error (it aborts the process if the OS source is unusable), so there is no
// weaker fallback to get wrong here.
func fillEntropy(e *[entropyLen]byte) {
	_, _ = rand.Read(e[:])
}

// incrementEntropy adds one to the big-endian 80-bit value and reports false
// on overflow.
func incrementEntropy(e *[entropyLen]byte) bool {
	for i := entropyLen - 1; i >= 0; i-- {
		e[i]++
		if e[i] != 0 {
			return true
		}
	}
	return false
}

// encodeID renders the 128-bit value (ms << 80 | entropy) as 26 base32 digits,
// most significant first. The top digit carries only 3 bits, so it is at most
// '7'.
func encodeID(ms uint64, e [entropyLen]byte) string {
	var raw [16]byte
	for i := 0; i < 6; i++ {
		raw[i] = byte(ms >> (8 * (5 - i)))
	}
	copy(raw[6:], e[:])

	var out [idLen]byte
	for k := 0; k < idLen; k++ { // k-th digit from the least significant end
		var d byte
		for j := 0; j < 5; j++ {
			bit := 5*k + j
			if bit >= 128 {
				break
			}
			d |= ((raw[15-bit/8] >> (bit % 8)) & 1) << j
		}
		out[idLen-1-k] = crockford[d]
	}
	return string(out[:])
}

// IDTime returns the timestamp embedded in a ULID produced by NewID. ok is
// false when id is not a well-formed ULID (wrong length, a character outside
// the alphabet, or a timestamp that overflows 48 bits).
func IDTime(id string) (time.Time, bool) {
	if len(id) != idLen {
		return time.Time{}, false
	}
	var ms uint64
	for i := 0; i < idLen; i++ {
		c := id[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		v := indexOf(c)
		if v < 0 {
			return time.Time{}, false
		}
		if i < 10 {
			if i == 0 && v > 7 {
				return time.Time{}, false
			}
			ms = ms<<5 | uint64(v)
		}
	}
	return time.UnixMilli(int64(ms)), true
}

func indexOf(c byte) int {
	for i := 0; i < len(crockford); i++ {
		if crockford[i] == c {
			return i
		}
	}
	return -1
}
