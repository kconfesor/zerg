package store

import (
	"crypto/rand"
	"encoding/binary"
	"sync"
	"time"
)

// crockford is Crockford base32: no I, L, O or U, so an id read aloud or
// retyped from a screenshot cannot turn into a different id.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// Ids are monotonic, which takes a little shared state. Within one millisecond
// the timestamp prefix is identical, so plain random suffixes would sort
// arbitrarily — and events written 0.2ms apart would replay out of order. The
// monotonic ULID rule fixes that: same millisecond, increment the previous
// suffix instead of drawing a new one.
var (
	idMu     sync.Mutex
	idLastMS uint64
	idLastRV [10]byte
)

// NewID returns a 26-character lexicographically sortable identifier: 48 bits
// of millisecond timestamp followed by 80 bits of monotonic randomness.
//
// Sortable matters here. Events, turns and messages are read in insertion order
// constantly, so an id that already sorts by creation saves both a secondary
// index and a clock-skew tiebreaker — but only if the ordering holds inside a
// millisecond too, which is where most bursts land.
func NewID() string { return newIDAt(time.Now()) }

func newIDAt(t time.Time) string {
	ms := uint64(t.UTC().UnixMilli())

	idMu.Lock()
	switch {
	case ms > idLastMS:
		idLastMS = ms
		if _, err := rand.Read(idLastRV[:]); err != nil {
			idMu.Unlock()
			// crypto/rand does not fail on any platform zerg supports. If it
			// ever did, a duplicated id would corrupt state quietly — so fail
			// loudly instead.
			panic("zerg: crypto/rand unavailable: " + err.Error())
		}
	default:
		// Same millisecond, or a clock that went backwards. Either way, stay
		// ahead of the last id rather than emitting one that sorts before it.
		ms = idLastMS
		if carry := increment(&idLastRV); carry {
			// 2^80 ids in one millisecond is not reachable, but silently
			// wrapping would break ordering, so borrow from the clock instead.
			idLastMS++
			ms = idLastMS
		}
	}
	rv := idLastRV
	idMu.Unlock()

	var raw [16]byte
	var stamp [8]byte
	binary.BigEndian.PutUint64(stamp[:], ms)
	copy(raw[0:6], stamp[2:8]) // low 48 bits of the millisecond timestamp
	copy(raw[6:], rv[:])

	// 128 bits into 26 base32 characters (130 bits of room; the top 2 go unused).
	out := make([]byte, 26)
	idx := len(out)
	var acc uint32
	var bits uint
	for i := len(raw) - 1; i >= 0; i-- {
		acc |= uint32(raw[i]) << bits
		bits += 8
		for bits >= 5 {
			idx--
			out[idx] = crockford[acc&31]
			acc >>= 5
			bits -= 5
		}
	}
	for idx > 0 {
		idx--
		out[idx] = crockford[acc&31]
		acc >>= 5
	}
	return string(out)
}

// increment adds one to a big-endian integer, reporting whether it wrapped.
func increment(b *[10]byte) bool {
	for i := len(b) - 1; i >= 0; i-- {
		b[i]++
		if b[i] != 0 {
			return false
		}
	}
	return true
}
