package main

import (
	"bufio"
	"strings"
	"testing"
)

func TestRawRequestFraming(t *testing.T) {
	for _, tc := range []struct {
		s  string
		ok bool
	}{
		{"POST /a HTTP/1.1\r\nHost: localhost\r\nContent-Length: 3\r\n\r\nabc", true},
		{"GET /a HTTP/1.1\r\nHost: localhost\r\n\r\n", true},
		{"POST /a HTTP/1.1\r\nHost: localhost\r\nContent-Length: 3\r\nContent-Length: 4\r\n\r\nabc", false},
		{"POST /a HTTP/1.1\r\nHost: localhost\r\nTransfer-Encoding: chunked\r\nContent-Length: 3\r\n\r\nabc", false},
		{"GET /a HTTP/1.1\r\nHost: localhost\r\n Bad: folded\r\n\r\n", false},
	} {
		var q rawRequest
		if got := q.read(bufio.NewReader(strings.NewReader(tc.s))) == 0; got != tc.ok {
			t.Errorf("framing result %v != %v", got, tc.ok)
		}
	}
}
func TestIssueEncodingParity(t *testing.T) {
	f := NewForge("u日本", "2026-01-01T00:00:00Z")
	f.Request("POST", mustRawURL("/api/v3/user/repos"), J{"name": "r"})
	r := f.Request("POST", mustRawURL("/api/v3/repos/u日本/r/issues"), J{"title": "control\x01\n\t\u2028<>&😀", "body": "quoted\"\\"})
	if r.Status != 201 {
		t.Fatal(r)
	}
	if got := string(issueJSON(nil, r.Body.(J))); got != js(r.Body) {
		t.Fatalf("got %s; want %s", got, js(r.Body))
	}
}
