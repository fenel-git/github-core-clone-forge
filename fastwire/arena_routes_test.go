package fastwire

import (
	"bytes"
	"regexp"
	"testing"
)

func TestArenaReusesCapacityAfterConsumption(t *testing.T) {
	a := NewArena(128)
	a.Append([]byte("first"))
	if string(a.Bytes()) != "first" {
		t.Fatal("first response lost")
	}
	before := &a.Bytes()[0]
	a.Reset()
	a.Append([]byte("next"))
	if string(a.Bytes()) != "next" || &a.Bytes()[0] != before {
		t.Fatal("arena did not reuse allocation")
	}
}
func TestFrameSidebandRoundTrip(t *testing.T) {
	for _, limit := range []int{6, 1000, 65520} {
		input := bytes.Repeat([]byte("payload"), 11000)
		wire, ok := Sideband(input, limit)
		if !ok {
			t.Fatal("valid limit rejected")
		}
		var decoded []byte
		for len(wire) > 0 {
			n, ok := PacketLength(wire[:4])
			if !ok || n > limit || n > len(wire) || n < 5 || wire[4] != 1 {
				t.Fatal("invalid framing")
			}
			decoded = append(decoded, wire[5:n]...)
			wire = wire[n:]
		}
		if !bytes.Equal(decoded, input) {
			t.Fatal("payload changed")
		}
	}
	for _, limit := range []int{-1, 0, 5, 65521} {
		if _, ok := Sideband([]byte{1}, limit); ok {
			t.Fatal("bad limit accepted", limit)
		}
	}
}
func TestRouteGrammarParity(t *testing.T) {
	issue := regexp.MustCompile(`^/api/v3/repos/([A-Za-z0-9_-][A-Za-z0-9_.-]*)/([A-Za-z0-9_-][A-Za-z0-9_.-]*)/issues(?:/([1-9][0-9]*))?/?$`)
	git := regexp.MustCompile(`^/([^/]+)/([^/]+)\.git/(info/refs|git-upload-pack|git-receive-pack)$`)
	for _, owner := range []string{"owner", "_", "-", "a.b", ".bad", "日本", "percent%20"} {
		for _, repo := range []string{"repo", "r.git", "..", "x-y", "_"} {
			for _, tail := range []string{"issues", "issues/", "issues/1", "issues/22/", "issues/0", "issues/1//", "issues/1/comments"} {
				path := "/api/v3/repos/" + owner + "/" + repo + "/" + tail
				want := issue.FindStringSubmatch(path)
				o, r, n, ok := IssuePath(path)
				if ok != (want != nil) || ok && (o != want[1] || r != want[2] || n != want[3]) {
					t.Fatal("issue grammar differs", path)
				}
			}
			for _, tail := range []string{"info/refs", "git-upload-pack", "git-receive-pack", "git-upload-pack/", "bad"} {
				path := "/" + owner + "/" + repo + ".git/" + tail
				want := git.FindStringSubmatch(path)
				o, r, s, ok := GitPath(path)
				if ok != (want != nil) || ok && (o != want[1] || r != want[2] || s != want[3]) {
					t.Fatal("Git grammar differs", path)
				}
			}
		}
	}
}
func TestBearerMatch(t *testing.T) {
	for _, token := range []string{"", "short", "token 日本"} {
		for _, header := range []string{"Bearer " + token, "bearer " + token, "Bearer " + token + "x", "Basic " + token, ""} {
			if got, want := MatchBearer(header, token), header == "Bearer "+token; got != want {
				t.Fatal("authorization mismatch")
			}
		}
	}
}
