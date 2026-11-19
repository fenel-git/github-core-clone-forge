package main

import (
	"crypto/sha256"
	"sync"
)

// Each store owns this cache. Generation changes cover every object and ref
// publication, including tags and objects used in have/shallow negotiation.
type uploadKey struct {
	generation  uint64
	head, phase string
	v2          bool
	request     [32]byte
}
type uploadFlight struct {
	done    chan struct{}
	reply   []byte
	failure any
}
type uploadCache struct {
	mu      sync.RWMutex
	entries map[uploadKey][]byte
	flights map[uploadKey]*uploadFlight
	size    int
}

func (c *uploadCache) get(key uploadKey, build func() []byte) []byte {
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
	if f.failure == nil && len(f.reply) > 0 {
		size := cap(f.reply)
		if size <= 2<<20 {
			if len(c.entries) >= 16 || c.size+size > 2<<20 {
				c.entries = nil
				c.size = 0
			}
			if c.entries == nil {
				c.entries = make(map[uploadKey][]byte)
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

func (s *GitStore) cloneBytes(phase string, v2 bool, body []byte, build func() []byte) []byte {
	if len(body) > 16<<10 {
		return build()
	}
	return s.cloneCache.get(uploadKey{s.cloneGeneration, s.Refs[gitHead], phase, v2, sha256.Sum256(body)}, build)
}
