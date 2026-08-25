package store

import (
	"crypto/rand"
	"encoding/binary"
	"time"
)

// crockford is Crockford base32: no I, L, O or U, so an id read aloud or
// retyped from a screenshot cannot turn into a different id.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewID returns a 26-character lexicographically sortable identifier: 48 bits
// of millisecond timestamp followed by 80 bits of randomness (ULID layout).
//
// Sortable matters here. Events, turns and messages are read in insertion
// order constantly, so an id that already sorts by creation time saves both a
// secondary index and a clock-skew tiebreaker.
func NewID() string { return newIDAt(time.Now()) }

func newIDAt(t time.Time) string {
	var raw [16]byte

	var stamp [8]byte
	binary.BigEndian.PutUint64(stamp[:], uint64(t.UTC().UnixMilli()))
	copy(raw[0:6], stamp[2:8]) // low 48 bits of the millisecond timestamp

	if _, err := rand.Read(raw[6:]); err != nil {
		// crypto/rand does not fail on any platform zerg supports. If it ever
		// did, a duplicated id would corrupt state quietly — so fail loudly.
		panic("zerg: crypto/rand unavailable: " + err.Error())
	}

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
