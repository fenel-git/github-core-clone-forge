package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"sync"
)

// Scope by GitStore identity. The exact packet bytes distinguish depth, wants
// order, protocol options and framing; ordered refs distinguish tag changes
// and reachability changes even when HEAD is unchanged.
type uploadKey struct{ request, refs [32]byte }
type uploadFlight struct {
	done    chan struct{}
	reply   wireResponse
	failure any
}
type uploadCache struct {
	mu      sync.RWMutex
	entries map[uploadKey]wireResponse
	flights map[uploadKey]*uploadFlight
	size    int
}

func (c *uploadCache) get(key uploadKey, build func() wireResponse) wireResponse {
	c.mu.RLock()
	x, ok := c.entries[key]
	c.mu.RUnlock()
	if ok {
		return x
	}
	c.mu.Lock()
	if x, ok = c.entries[key]; ok {
		c.mu.Unlock()
		return x
	}
	if f := c.flights[key]; f != nil {
		c.mu.Unlock()
		<-f.done
		if f.failure != nil {
			panic(f.failure)
		}
		return f.reply
	}
	f := &uploadFlight{done: make(chan struct{})}
	if c.flights == nil {
		c.flights = make(map[uploadKey]*uploadFlight)
	}
	c.flights[key] = f
	c.mu.Unlock()
	func() { defer func() { f.failure = recover() }(); f.reply = build() }()
	c.mu.Lock()
	delete(c.flights, key)
	if f.failure == nil && f.reply.code == 200 && len(f.reply.cached) > 0 {
		size := cap(f.reply.cached) + cap(f.reply.body)
		if size <= 2<<20 {
			if len(c.entries) >= 16 || c.size+size > 2<<20 {
				c.entries = nil
				c.size = 0
			}
			if c.entries == nil {
				c.entries = make(map[uploadKey]wireResponse)
			}
			c.entries[key] = f.reply
			c.size += size
		}
	}
	close(f.done)
	c.mu.Unlock()
	if f.failure != nil {
		panic(f.failure)
	}
	return f.reply
}

func (s *GitStore) uploadResponse(body []byte, v2 bool) wireResponse {
	const typ = "application/x-git-upload-pack-result"
	build := func() wireResponse { return gitResponse(200, typ, s.upload(body, v2)) }
	// Negotiation with have/shallow depends on objects outside the current ref
	// snapshot. Bypass caching for those and non-fetch or oversized requests.
	if !v2 || len(body) > 16<<10 || !bytes.HasPrefix(body, []byte("0012command=fetch\n")) || !bytes.Contains(body, []byte("0009done\n")) || bytes.Contains(body, []byte("have ")) || bytes.Contains(body, []byte("shallow ")) {
		return build()
	}
	key := uploadKey{request: sha256.Sum256(body)}
	h := sha256.New()
	var n [8]byte
	for _, ref := range s.RefOrder {
		sha, ok := s.Refs[ref]
		if !ok {
			continue
		}
		binary.BigEndian.PutUint64(n[:], uint64(len(ref)))
		h.Write(n[:])
		h.Write([]byte(ref))
		h.Write([]byte(sha))
	}
	h.Sum(key.refs[:0])
	return s.uploads.get(key, func() wireResponse {
		x := build()
		if len(x.body) <= 1<<20 {
			x.cached = appendWire(make([]byte, 0, len(x.body)+160), x, false, false)
			x.direct = true
		}
		return x
	})
}
