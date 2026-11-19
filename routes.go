package main

import (
	"strconv"
	"strings"
)

func gitPath(path string) (owner, name, service string) {
	if len(path) == 0 || path[0] != '/' {
		return
	}
	owner, rest, ok := strings.Cut(path[1:], "/")
	if !ok || owner == "" {
		return "", "", ""
	}
	repo, service, ok := strings.Cut(rest, "/")
	if !ok {
		return "", "", ""
	}
	name, ok = strings.CutSuffix(repo, ".git")
	if !ok || name == "" {
		return "", "", ""
	}
	switch service {
	case "info/refs", "git-upload-pack", "git-receive-pack":
		return owner, name, service
	}
	return "", "", ""
}
func issueSegment(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || i > 0 && c == '.') {
			return false
		}
	}
	return true
}
func issuePath(path, query string) (owner, name string, n int, ok bool) {
	rest, ok := strings.CutPrefix(path, "/api/v3/repos/")
	if !ok {
		return "", "", 0, false
	}
	owner, rest, ok = strings.Cut(rest, "/")
	if !ok || !issueSegment(owner) {
		return "", "", 0, false
	}
	name, rest, ok = strings.Cut(rest, "/")
	if !ok || !issueSegment(name) {
		return "", "", 0, false
	}
	rest = strings.TrimSuffix(rest, "/")
	if rest == "issues" {
		return owner, name, 0, query == ""
	}
	s, ok := strings.CutPrefix(rest, "issues/")
	if !ok || s == "" || s[0] < '1' || s[0] > '9' {
		return "", "", 0, false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return "", "", 0, false
		}
	}
	n, e := strconv.Atoi(s)
	if e != nil {
		n = -1
	}
	return owner, name, n, true
}
