package main

import (
	"bytes"
	"net/url"
	"strconv"
	"strings"
)

const jsonType = "application/json; charset=utf-8"

func rawReply(r Reply, id string) wireResponse {
	x := wireResponse{code: r.Status, typ: jsonType, id: id, extra: r.Headers}
	if r.Status != 204 && r.Status != 202 {
		x.body = []byte(js(r.Body))
	}
	return x
}

// Cache templates are immutable under concurrent reads. Patch the connection's
// reusable copy, not shared bytes, and left-pad with legal header whitespace.
func (a *App) cachedIssue(u *url.URL, id string) (wireResponse, bool) {
	m := issueRoute.FindStringSubmatch(u.EscapedPath())
	if m == nil || m[3] == "" && u.RawQuery != "" {
		return wireResponse{}, false
	}
	key := strings.ToLower(m[1]+"/"+m[2]) + "/" + m[3]
	a.cacheMu.RLock()
	x, ok := a.issueCache[key]
	a.cacheMu.RUnlock()
	if ok {
		x.id = id
		return x, true
	}
	r := a.api.Repo(m[1], m[2])
	if r == nil {
		return rawReply(missing(), id), true
	}
	var b []byte
	if m[3] != "" {
		n, _ := strconv.Atoi(m[3])
		j := r.Issues[n]
		if j == nil {
			return rawReply(missing(), id), true
		}
		b = issueJSON(nil, j)
	} else {
		if len(r.IssueOrder) > 30 {
			return wireResponse{}, false
		}
		b = append(b, '[')
		for i := len(r.IssueOrder) - 1; i >= 0; i-- {
			j := r.Issues[r.IssueOrder[i]]
			if j["state"] == "open" {
				if len(b) > 1 {
					b = append(b, ',')
				}
				b = issueJSON(b, j)
			}
		}
		b = append(b, ']')
	}
	x = wireResponse{code: 200, typ: jsonType, id: strings.Repeat("0", 20), body: b}
	if len(b) <= 64*1024 {
		x.cached = appendWire(nil, x, false, false)
		marker := []byte("\r\nX-Github-Request-Id: ")
		x.idAt = bytes.Index(x.cached, marker) + len(marker)
		a.cacheMu.Lock()
		if a.issueCache == nil {
			a.issueCache = make(map[string]wireResponse)
		}
		if len(a.issueCache) < 128 {
			a.issueCache[key] = x
		}
		a.cacheMu.Unlock()
	}
	x.id = id
	return x, true
}
