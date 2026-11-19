package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"testing"
)

func testGitRawEntry(code int, payload, base []byte) []byte {
	n := len(payload)
	h := []byte{byte(code<<4 | (n & 15))}
	n >>= 4
	for n > 0 {
		h[len(h)-1] |= 128
		h = append(h, byte(n&127))
		n >>= 7
	}
	var b bytes.Buffer
	b.Write(h)
	b.Write(base)
	z := zlib.NewWriter(&b)
	z.Write(payload)
	z.Close()
	return b.Bytes()
}
func testGitPack(entries ...[]byte) []byte {
	var b bytes.Buffer
	b.WriteString("PACK")
	binary.Write(&b, binary.BigEndian, uint32(2))
	binary.Write(&b, binary.BigEndian, uint32(len(entries)))
	for _, e := range entries {
		b.Write(e)
	}
	sum := sha1.Sum(b.Bytes())
	b.Write(sum[:])
	return b.Bytes()
}
func TestGitDeltasAndPackValidation(t *testing.T) {
	s := NewGitStore()
	base := []byte("hello")
	id := gitHash("blob", base)
	raw, _ := hex.DecodeString(id)
	delta := []byte{5, 6, 0x90, 5, 1, '!'}
	plain := testGitRawEntry(3, base, nil)
	ref := testGitRawEntry(7, delta, raw)
	ofs := testGitRawEntry(6, delta, []byte{byte(len(plain))})
	for name, p := range map[string][]byte{"ofs": testGitPack(plain, ofs), "ref": testGitPack(plain, ref), "forward-ref": testGitPack(ref, plain)} {
		t.Run(name, func(t *testing.T) {
			got := gitUnpack(p, s.lookup)
			out := got[gitHash("blob", []byte("hello!"))]
			if out == nil || string(out.Data) != "hello!" {
				t.Fatal("delta wrong")
			}
		})
	}
	s.Write("blob", base)
	got := gitUnpack(testGitPack(ref), s.lookup)
	if len(got) != 1 {
		t.Fatal("thin delta")
	}
	bad := testGitPack(plain)
	bad[len(bad)-1] ^= 1
	cases := map[string][]byte{"checksum": bad, "trailing": testGitPack(append(plain, 1)), "missing-base": testGitPack(ref), "wrong-ofs": testGitPack(plain, testGitRawEntry(6, delta, []byte{1})), "zero-op": testGitPack(plain, testGitRawEntry(7, []byte{5, 1, 0}, raw)), "copy-range": testGitPack(plain, testGitRawEntry(7, []byte{5, 6, 0x90, 6}, raw)), "size-mismatch": testGitPack(plain, testGitRawEntry(7, []byte{6, 6, 1, '!'}, raw))}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			if gitAttempt(func() { gitUnpack(p, NewGitStore().lookup) }) {
				t.Fatal("accepted invalid pack")
			}
		})
	}
}
func testGitPush(s *GitStore, old, next, ref string, objects []*gitStoredObject) string {
	b := gitPktText(old + " " + next + " " + ref + "\x00report-status\n")
	b = append(b, []byte("0000")...)
	if next != gitZero {
		b = append(b, gitPackFor(objects)...)
	}
	return string(s.receive(b))
}
func TestGitReceiveQuarantineAndGraphValidation(t *testing.T) {
	src, dst := NewGitStore(), NewGitStore()
	blob := src.Write("blob", []byte("x"))
	tip := src.Commit(map[string]GitFile{"x": {blob, "100644"}}, nil, "one")
	closure := gitWalk([]string{tip}, src.lookup, nil, nil)
	objects := []*gitStoredObject{}
	for _, id := range closure.ids {
		objects = append(objects, src.lookup(id))
	}
	if r := testGitPush(dst, gitZero, tip, gitHead, objects); !bytes.Contains([]byte(r), []byte("ok "+gitHead)) {
		t.Fatal(r)
	}
	sibling := src.Commit(map[string]GitFile{"x": {blob, "100644"}}, nil, "sibling")
	objects = append(objects, src.lookup(sibling))
	if r := testGitPush(dst, tip, sibling, gitHead, objects); !bytes.Contains([]byte(r), []byte("non-fast-forward")) {
		t.Fatal(r)
	}
	if dst.Read(sibling) != nil || dst.Refs[gitHead] != tip {
		t.Fatal("rejected push leaked objects or moved ref")
	}
	bad := src.Write("commit", []byte("tree "+gitHash("tree", []byte("missing"))+"\n\nbad\n"))
	if r := testGitPush(dst, gitZero, bad, "refs/heads/bad", []*gitStoredObject{src.lookup(bad)}); !bytes.Contains([]byte(r), []byte("invalid object graph")) {
		t.Fatal(r)
	}
	if dst.Read(bad) != nil || dst.Refs["refs/heads/bad"] != "" {
		t.Fatal("invalid graph leaked")
	}
	if r := testGitPush(dst, tip, gitZero, gitHead, nil); !bytes.Contains([]byte(r), []byte("ok "+gitHead)) {
		t.Fatal(r)
	}
	if len(dst.RefOrder) != 0 {
		t.Fatal("delete did not remove ordered ref")
	}
}
func TestGitAtomicReceiveRollsBackRefsAndObjects(t *testing.T) {
	src, dst := NewGitStore(), NewGitStore()
	old := dst.Commit(nil, nil, "old")
	dst.SetRef(gitHead, old)
	next := src.Commit(nil, nil, "new ref")
	closure := gitWalk([]string{next}, src.lookup, nil, nil)
	var objects []*gitStoredObject
	for _, id := range closure.ids {
		objects = append(objects, src.lookup(id))
	}
	ref := "refs/heads/new"
	other := "refs/heads/other"
	b := append(gitPktText(gitZero+" "+next+" "+ref+"\x00report-status atomic\n"), gitPktText(old+" "+next+" "+other+"\n")...)
	b = append(b, []byte("0000")...)
	b = append(b, gitPackFor(objects)...)
	report := dst.receive(b)
	if !bytes.Contains(report, []byte("ng "+ref+" atomic push failure")) || !bytes.Contains(report, []byte("ng "+other+" stale ref")) {
		t.Fatalf("report %q", report)
	}
	if dst.Refs[gitHead] != old || len(dst.Refs) != 1 || dst.Read(next) != nil {
		t.Fatal("failed transaction published refs or objects")
	}
	b = append(gitPktText(gitZero+" "+next+" "+ref+"\x00report-status atomic\n"), gitPktText(gitZero+" "+next+" "+other+"\n")...)
	b = append(b, []byte("0000")...)
	b = append(b, gitPackFor(objects)...)
	report = dst.receive(b)
	if !bytes.Contains(report, []byte("ok "+ref)) || !bytes.Contains(report, []byte("ok "+other)) || dst.Refs[ref] != next || dst.Refs[other] != next || dst.Read(next) == nil {
		t.Fatalf("did not publish transaction: %q", report)
	}
}
func TestGitDeterministicCommitAndCopy(t *testing.T) {
	s := NewGitStore()
	data := []byte("hello\n")
	b := s.Write("blob", data)
	if b != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Fatal(b)
	}
	data[0] = 'X'
	if string(s.Read(b).Data) != "hello\n" {
		t.Fatal("write aliases caller")
	}
	files := map[string]GitFile{"z/file": {b, "100755"}, "a.txt": {b, "100644"}}
	c := s.Commit(files, nil, "initial")
	if c != "62ffe5544e7fcbc88b665dd5d619c68fa25eefe7" || s.Commit(files, nil, "initial") != c {
		t.Fatal("nondeterministic commit")
	}
	if len(s.Files(c)) != 2 || s.History(c)[0].Message != "initial" {
		t.Fatal("commit round trip")
	}
	got := s.Read(b)
	got.Data[0] = 'Z'
	if string(s.Read(b).Data) != "hello\n" {
		t.Fatal("read aliases store")
	}
	p := gitPackFor([]*gitStoredObject{s.objects[c], s.objects[b]})
	incoming := gitUnpack(p, s.lookup)
	if len(incoming) != 2 || !bytes.Equal(incoming[b].Data, s.objects[b].Data) {
		t.Fatal("pack roundtrip")
	}
}
