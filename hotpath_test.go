package main

import (
	"bytes"
	"compress/zlib"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

func TestHotpathReadersShareRWLock(t *testing.T) {
	t.Setenv("MAKEFUN_FORGE_LOGIN", "parallel")
	t.Setenv("MAKEFUN_SHOP_TOKEN", "parallel-token")
	app := newApp()
	u, _ := url.Parse("http://localhost/api/v3/user/repos")
	if r := app.api.Request("POST", u, J{"name": "parallel", "auto_init": true}); r.Status != 201 {
		t.Fatal(r)
	}
	u, _ = url.Parse("http://localhost/api/v3/repos/parallel/parallel/issues")
	if r := app.api.Request("POST", u, J{"title": "immutable cached bytes"}); r.Status != 201 {
		t.Fatal(r)
	}
	for _, target := range []string{"/api/v3/repos/parallel/parallel/issues/1", "/parallel/parallel.git/info/refs?service=git-upload-pack"} {
		// A held read lock must not prevent another GET reader from completing.
		app.mu.RLock()
		done := make(chan int, 1)
		go func(path string) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", path, nil)
			r.Header.Set("Authorization", "Bearer parallel-token")
			r.Header.Set("Git-Protocol", "version=2")
			app.ServeHTTP(w, r)
			done <- w.Code
		}(target)
		select {
		case code := <-done:
			app.mu.RUnlock()
			if code != 200 {
				t.Fatal(target, code)
			}
		case <-time.After(3 * time.Second):
			app.mu.RUnlock()
			<-done
			t.Fatal("reader serialized behind read lock", target)
		}
	}
	var wg sync.WaitGroup
	for n := 0; n < 50; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/api/v3/repos/parallel/parallel/issues/1", nil)
			r.Header.Set("Authorization", "Bearer parallel-token")
			app.ServeHTTP(w, r)
			if w.Code != 200 {
				t.Errorf("parallel issue read: %d", w.Code)
			}
		}()
	}
	wg.Wait()
	// Warm bytes must be invalidated by a real mutation, not copied stale.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("PATCH", "/api/v3/repos/parallel/parallel/issues/1", bytes.NewBufferString(`{"title":"changed"}`))
	r.Header.Set("Authorization", "Bearer parallel-token")
	app.ServeHTTP(w, r)
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/api/v3/repos/parallel/parallel/issues/1", nil)
	r.Header.Set("Authorization", "Bearer parallel-token")
	app.ServeHTTP(w, r)
	if !bytes.Contains(w.Body.Bytes(), []byte(`"title":"changed"`)) {
		t.Fatal("stale cached bytes", w.Body.String())
	}
}

func TestHotpathConcurrentPackCompression(t *testing.T) {
	object := &gitStoredObject{GitObject: GitObject{Type: "blob", Data: bytes.Repeat([]byte("read-only git payload\n"), 3000)}}
	var wg sync.WaitGroup
	results := make(chan []byte, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- gitPackFor([]*gitStoredObject{object}) }()
	}
	wg.Wait()
	close(results)
	var first []byte
	for p := range results {
		if first == nil {
			first = p
		} else if !bytes.Equal(first, p) {
			t.Fatal("concurrent pack encodings differ")
		}
	}
	if len(object.compressed) < 2 || object.compressed[1] != 1 {
		t.Fatal("new objects must use zlib BestSpeed")
	}
	z, err := zlib.NewReader(bytes.NewReader(object.compressed))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(z)
	z.Close()
	if err != nil || !bytes.Equal(decoded, object.Data) {
		t.Fatal("compressed data changed contents", err)
	}
}
