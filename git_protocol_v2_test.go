package main

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

func TestConcurrentV2ShallowFetchBytes(t *testing.T) {
	s := NewGitStore()
	blob := s.Write("blob", []byte("file contents\n"))
	files := map[string]GitFile{"example": {Sha: blob, Mode: "100644"}}
	root := s.Commit(files, nil, "root")
	head := s.Commit(files, []string{root}, "head")
	s.SetRef(gitHead, head)
	request := func(cmd string, args ...string) []byte {
		b := append(gitPktText("command="+cmd+"\n"), "0001"...)
		for _, arg := range args {
			b = append(b, gitPktText(arg+"\n")...)
		}
		return append(b, "0000"...)
	}
	if refs := s.upload(request("ls-refs", "symrefs", "ref-prefix refs/heads/"), true); !bytes.Contains(refs, []byte(head+" "+gitHead+"\n")) {
		t.Fatalf("bad ref listing: %q", refs)
	}
	q := request("fetch", "want "+head, "deepen 1", "done")
	var wg sync.WaitGroup
	results := make([][]byte, 50)
	start := make(chan struct{})
	for i := range results {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; results[i] = s.upload(q, true) }()
	}
	close(start)
	wg.Wait()
	for i, b := range results {
		if !bytes.Equal(results[0], b) {
			t.Fatalf("fetch %d differs", i)
		}
	}
	if !bytes.Contains(results[0], []byte("shallow "+head+"\n")) {
		t.Fatal("missing shallow boundary")
	}
	cursor := gitCursorFor(results[0])
	var pack []byte
	inPack := false
	for cursor.p < cursor.end {
		prefix := cursor.take(4)
		if bytes.Equal(prefix, []byte("0000")) || bytes.Equal(prefix, []byte("0001")) {
			continue
		}
		n := 0
		for _, c := range prefix {
			n = n*16 + strings.IndexByte("0123456789abcdef", c)
		}
		if n < 4 || n-4 > cursor.end-cursor.p {
			t.Fatalf("bad response packet length %q", prefix)
		}
		data := cursor.take(n - 4)
		if string(data) == "packfile\n" {
			inPack = true
			continue
		}
		if inPack {
			if len(data) == 0 || data[0] != 1 {
				t.Fatal("bad sideband channel")
			}
			pack = append(pack, data[1:]...)
		}
	}
	objects := gitUnpack(pack, s.lookup)
	if objects[head] == nil || objects[root] != nil || objects[blob] == nil {
		t.Fatal("invalid shallow object selection")
	}
}
