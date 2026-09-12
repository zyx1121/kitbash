// Package uuid generates the UUIDv7 identifiers spec/mcp-surface.yaml asks
// for. Version 7 sorts by creation time, which is what makes a Process id
// useful in a log, and it is short enough to implement here rather than take a
// dependency for.
package uuid

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// V7 returns a version 7 UUID in its canonical 8-4-4-4-12 form: 48 bits of
// Unix milliseconds, then random bits, with the version and variant fields set
// as RFC 9562 requires.
func V7() string {
	var b [16]byte
	// crypto/rand.Read never fails; it panics on a broken system reader.
	rand.Read(b[:])

	ms := time.Now().UnixMilli()
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	b[6] = (b[6] & 0x0f) | 0x70 // version 7
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10

	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}
