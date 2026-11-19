package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPAuthAndCacheInvalidation(t *testing.T) {
	t.Setenv("MAKEFUN_FORGE_LOGIN", "port-test")
	t.Setenv("MAKEFUN_TRIAL_NOW", "2026-01-02T03:04:05.000Z")
	t.Setenv("MAKEFUN_SHOP_TOKEN", "port-test-token")
	server := httptest.NewServer(newApp())
	defer server.Close()
	call := func(method, path, body, token string) (int, any, http.Header) {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", token)
		}
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		var v any
		if len(data) > 0 && json.Unmarshal(data, &v) != nil {
			t.Fatalf("not JSON: %s", data)
		}
		return res.StatusCode, v, res.Header
	}
	if code, _, _ := call("GET", "/healthz", "", ""); code != 200 {
		t.Fatal(code)
	}
	if code, _, _ := call("GET", "/api/v3/user", "", ""); code != 401 {
		t.Fatal(code)
	}
	auth := "Bearer port-test-token"
	if code, _, _ := call("POST", "/api/v3/user/repos", `{"name":"cache"}`, auth); code != 201 {
		t.Fatal(code)
	}
	if code, _, _ := call("POST", "/api/v3/repos/port-test/cache/issues", `{"title":"before"}`, auth); code != 201 {
		t.Fatal(code)
	}
	for i := 0; i < 2; i++ {
		code, v, h := call("GET", "/api/v3/repos/port-test/cache/issues/1", "", auth)
		if code != 200 || obj(v)["title"] != "before" || !strings.HasPrefix(h.Get("X-Github-Request-Id"), "FORGE-") {
			t.Fatal(code, v, h)
		}
	}
	if code, _, _ := call("PATCH", "/api/v3/repos/port-test/cache/issues/1", `{"title":"after","state":"closed"}`, auth); code != 200 {
		t.Fatal(code)
	}
	if code, v, _ := call("GET", "/api/v3/repos/port-test/cache/issues/1", "", auth); code != 200 || obj(v)["title"] != "after" {
		t.Fatal(code, v)
	}
	if code, v, _ := call("GET", "/api/v3/repos/port-test/cache/issues", "", auth); code != 200 || len(arr(v)) != 0 {
		t.Fatal(code, v)
	}
	if code, v, _ := call("GET", "/api/v3/repos/port-test/cache/issues?state=all", "", auth); code != 200 || len(arr(v)) != 1 {
		t.Fatal(code, v)
	}
	if code, _, _ := call("POST", "/api/graphql", `{`, auth); code != 400 {
		t.Fatal(code)
	}
	if code, v, _ := call("POST", "/mcp", `{`, auth); code != 400 || num(obj(obj(v)["error"])["code"]) != -32700 {
		t.Fatal(code, v)
	}
}
func TestJSONUnicodeIsNotHTMLEscaped(t *testing.T) {
	data := []byte(js(J{"x": "<>&é日本"}))
	if bytes.Contains(data, []byte(`\u003c`)) {
		t.Fatalf("HTML escaping: %s", data)
	}
}
