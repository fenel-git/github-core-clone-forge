package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestRESTConcurrentReadersSequentialMutations(t *testing.T) {
	t.Setenv("MAKEFUN_FORGE_LOGIN", "determinism")
	t.Setenv("MAKEFUN_SHOP_TOKEN", "test-secret")
	t.Setenv("MAKEFUN_TRIAL_NOW", "2026-01-01T00:00:00Z")
	run := func() [][]byte {
		server := httptest.NewServer(newApp())
		defer server.Close()
		call := func(method, path, body string, want int) []byte {
			t.Helper()
			req, e := http.NewRequest(method, server.URL+path, bytes.NewBufferString(body))
			if e != nil {
				t.Fatal(e)
			}
			req.Header.Set("Authorization", "Bearer test-secret")
			req.Header.Set("Content-Type", "application/json")
			res, e := server.Client().Do(req)
			if e != nil {
				t.Fatal(e)
			}
			defer res.Body.Close()
			wire, e := io.ReadAll(res.Body)
			if e != nil || res.StatusCode != want || res.ContentLength != int64(len(wire)) {
				t.Fatalf("%s %s: status=%d bytes=%s err=%v", method, path, res.StatusCode, wire, e)
			}
			return wire
		}
		transcript := [][]byte{call("POST", "/api/v3/user/repos", `{"name":"r"}`, 201)}
		path := "/api/v3/repos/determinism/r/issues"
		for i := 0; i < 200; i++ {
			wire := call("POST", path, fmt.Sprintf(`{"title":"issue %d"}`, i), 201)
			var issue J
			if json.Unmarshal(wire, &issue) != nil || num(issue["number"]) != i+1 {
				t.Fatal("bad issue sequence")
			}
			transcript = append(transcript, wire)
		}
		first := call("GET", path+"/1", "", 200)
		for i := 0; i < 2000; i++ {
			if !bytes.Equal(first, call("GET", path+"/1", "", 200)) {
				t.Fatalf("read %d differs", i)
			}
		}
		var wg sync.WaitGroup
		errors := make(chan error, 50)
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req, _ := http.NewRequest("GET", server.URL+path+"/1", nil)
				req.Header.Set("Authorization", "Bearer test-secret")
				res, e := server.Client().Do(req)
				if e != nil {
					errors <- e
					return
				}
				defer res.Body.Close()
				b, e := io.ReadAll(res.Body)
				if e != nil || res.StatusCode != 200 || !bytes.Equal(b, first) {
					errors <- fmt.Errorf("concurrent read changed")
				}
			}()
		}
		wg.Wait()
		close(errors)
		for e := range errors {
			t.Fatal(e)
		}
		transcript = append(transcript, call("PATCH", path+"/1", `{"title":"changed","state":"closed"}`, 200))
		changed := call("GET", path+"/1", "", 200)
		if bytes.Equal(first, changed) {
			t.Fatal("cache not invalidated")
		}
		transcript = append(transcript, changed)
		return transcript
	}
	a, b := run(), run()
	if len(a) != len(b) {
		t.Fatal("transcript length differs")
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			t.Fatalf("repeated HTTP operation %d is nondeterministic", i)
		}
	}
}
