package fastwire

import (
	"crypto/sha1"
	"fmt"
	"testing"
)

func TestPacketLengths(t *testing.T) {
	for n := 0; n <= 65535; n++ {
		p := fmt.Appendf(nil, "%04X", n)
		got, ok := PacketLength(p)
		if !ok || got != n {
			t.Fatalf("%q: %d, %v", p, got, ok)
		}
	}
	for _, p := range []string{"", "000", "00000", "-001", "00g0", "0x10", "000\x00"} {
		if _, ok := PacketLength([]byte(p)); ok {
			t.Fatalf("accepted %q", p)
		}
	}
}

func TestObjectHash(t *testing.T) {
	for _, kind := range []string{"blob", "tree", "commit", "tag"} {
		for _, data := range [][]byte{nil, {0, 10, 255}, make([]byte, 65536)} {
			want := sha1.Sum(append(fmt.Appendf(nil, "%s %d\x00", kind, len(data)), data...))
			if got := ObjectHash(kind, data); got != want {
				t.Fatalf("%s/%d: %x != %x", kind, len(data), got, want)
			}
		}
	}
}
