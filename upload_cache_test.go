package main

import (
	"bufio"
	"bytes"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestUploadSingleflight(t *testing.T) {
	var c uploadCache
	var calls atomic.Int32
	var wg sync.WaitGroup
	key := uploadKey{}
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			x := c.get(key, func() wireResponse { calls.Add(1); return wireResponse{code: 200, cached: []byte("wire")} })
			if string(x.cached) != "wire" {
				t.Error("wrong response")
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("built %d times", calls.Load())
	}
}
func TestUploadCacheFullWireAndRefChange(t *testing.T) {
	s := NewGitStore()
	a := s.Commit(nil, nil, "first")
	s.SetRef(gitHead, a)
	request := func(id string) []byte {
		return []byte("0012command=fetch\n00010032want " + id + "\n000ddeepen 1\n0009done\n0000")
	}
	body := request(a)
	x := s.uploadResponse(body, true)
	y := s.uploadResponse(body, true)
	if !x.direct || !bytes.Equal(x.cached, appendWire(nil, x, false, false)) || &x.cached[0] != &y.cached[0] {
		t.Fatal("full wire not reused")
	}
	b := s.Commit(nil, []string{a}, "second")
	s.SetRef(gitHead, b)
	z := s.uploadResponse(request(b), true)
	if bytes.Equal(x.cached, z.cached) {
		t.Fatal("stale pack")
	}
}
func TestRawFieldsSurviveReaderRefill(t *testing.T) {
	data := "GET /api/v3 HTTP/1.1\r\nHost: localhost\r\nX-Padding: " + strings.Repeat("z", 160) + "\r\nAuthorization: Bearer private\r\n\r\n"
	var q rawRequest
	if q.read(bufio.NewReaderSize(strings.NewReader(data), 180)) != 0 || string(q.target) != "/api/v3" || string(q.host) != "localhost" || string(q.auth) != "Bearer private" {
		t.Fatal("fields changed when reader refilled")
	}
}
