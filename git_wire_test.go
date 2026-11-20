package main

import (
	"bytes"
	"fmt"
	"testing"
)

func referenceSideband(data []byte, limit int) []byte {
	var wire []byte
	for len(data) > 0 {
		n := min(len(data), limit-5)
		wire = fmt.Appendf(wire, "%04x", n+5)
		wire = append(wire, 1)
		wire = append(wire, data[:n]...)
		data = data[n:]
	}
	return wire
}

func TestSidebandByteParity(t *testing.T) {
	for _, limit := range []int{1000, gitMaxPacket} {
		for _, size := range []int{0, 1, 15, 255, limit - 6, limit - 5, limit - 4, 2 * (limit - 5), 2*(limit-5) + 1} {
			data := make([]byte, size)
			for i := range data {
				data[i] = byte(i)
			}
			if got, want := gitSideband(data, limit), referenceSideband(data, limit); !bytes.Equal(got, want) {
				t.Fatalf("limit %d size %d: framing mismatch", limit, size)
			}
		}
	}
}

func TestSidebandSingleOutputAllocation(t *testing.T) {
	data := make([]byte, 3*gitMaxPacket)
	n := testing.AllocsPerRun(10, func() { _ = gitSideband(data, gitMaxPacket) })
	if n > 1 {
		t.Fatalf("sideband assembly allocates %v times; expected at most one output buffer", n)
	}
}

func BenchmarkSidebandAssembly(b *testing.B) {
	for _, n := range []int{2048, 192 * 1024} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			p := make([]byte, n)
			b.ReportAllocs()
			b.SetBytes(int64(n))
			for b.Loop() {
				_ = gitSideband(p, gitMaxPacket)
			}
		})
	}
}
