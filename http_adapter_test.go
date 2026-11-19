package main

import (
	"io"
	"net/http"
	"strconv"
)

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data, _ := io.ReadAll(io.LimitReader(r.Body, bodyLimit+1))
	x := a.dispatchRaw(&rawRequest{method: []byte(r.Method), target: []byte(r.URL.RequestURI()), host: []byte(r.Host), auth: []byte(r.Header.Get("Authorization")), protocol: []byte(r.Header.Get("Git-Protocol")), body: data})
	w.Header().Set("Content-Type", x.typ)
	w.Header().Set("Content-Length", strconv.Itoa(len(x.body)))
	if x.id != "" {
		w.Header().Set("X-Github-Request-Id", x.id)
	}
	if x.git {
		w.Header().Set("Cache-Control", "no-cache")
	}
	for k, v := range x.extra {
		w.Header().Set(k, v)
	}
	w.WriteHeader(x.code)
	if r.Method != "HEAD" {
		_, _ = w.Write(x.body)
	}
}
