// Package fastwire contains byte-level helpers for Git's wire format.
package fastwire

import (
	"crypto/sha1"
	"strconv"
)

// PacketLength decodes exactly four hexadecimal digits. The caller checks
// protocol-specific control values and the maximum packet size separately.
func PacketLength(prefix []byte) (int, bool) {
	if len(prefix) != 4 {
		return 0, false
	}
	n := 0
	for _, c := range prefix {
		switch {
		case '0' <= c && c <= '9':
			c -= '0'
		case 'a' <= c && c <= 'f':
			c = c - 'a' + 10
		case 'A' <= c && c <= 'F':
			c = c - 'A' + 10
		default:
			return 0, false
		}
		n = n<<4 | int(c)
	}
	return n, true
}

// ObjectHash hashes the canonical Git object header and body, without copying
// the body into a temporary buffer. SHA-1 is required by this Git format.
func ObjectHash(kind string, data []byte) [sha1.Size]byte {
	var scratch [64]byte
	header := append(scratch[:0], kind...)
	header = append(header, ' ')
	header = strconv.AppendInt(header, int64(len(data)), 10)
	header = append(header, 0)
	h := sha1.New()
	h.Write(header)
	h.Write(data)
	var sum [sha1.Size]byte
	h.Sum(sum[:0])
	return sum
}
