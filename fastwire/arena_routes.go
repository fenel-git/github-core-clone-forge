package fastwire

import (
	"crypto/subtle"
	"strings"
)

// Arena belongs to one response builder. Bytes returns its owned storage;
// callers must finish consuming the bytes before Reset or another Append.
// Published cache entries must own the arena rather than return it to a pool.
type Arena struct{ buffer []byte }

func NewArena(capacity int) Arena { return Arena{buffer: make([]byte, 0, capacity)} }
func (a *Arena) Append(p []byte)  { a.buffer = append(a.buffer, p...) }
func (a *Arena) Bytes() []byte    { return a.buffer }
func (a *Arena) Reset()           { a.buffer = a.buffer[:0] }

// Sideband allocates exactly one buffer for the complete channel-1 response.
func Sideband(data []byte, limit int) ([]byte, bool) {
	if limit <= 5 || limit > 65520 {
		return nil, false
	}
	if len(data) == 0 {
		return nil, true
	}
	stride := limit - 5
	packets := 1 + (len(data)-1)/stride
	a := NewArena(len(data) + 5*packets)
	const digits = "0123456789abcdef"
	for len(data) > 0 {
		n := min(len(data), stride)
		size := n + 5
		prefix := [5]byte{digits[size>>12], digits[(size>>8)&15], digits[(size>>4)&15], digits[size&15], 1}
		a.Append(prefix[:])
		a.Append(data[:n])
		data = data[n:]
	}
	return a.Bytes(), true
}

func issueName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || i > 0 && c == '.' {
			continue
		}
		return false
	}
	return true
}

func IssuePath(path string) (owner, repo, number string, ok bool) {
	rest, ok := strings.CutPrefix(path, "/api/v3/repos/")
	if !ok {
		return "", "", "", false
	}
	owner, rest, ok = strings.Cut(rest, "/")
	if !ok || !issueName(owner) {
		return "", "", "", false
	}
	repo, rest, ok = strings.Cut(rest, "/")
	if !ok || !issueName(repo) {
		return "", "", "", false
	}
	if rest == "issues" || rest == "issues/" {
		return owner, repo, "", true
	}
	number, ok = strings.CutPrefix(rest, "issues/")
	if !ok {
		return "", "", "", false
	}
	number = strings.TrimSuffix(number, "/")
	if number == "" || number[0] < '1' || number[0] > '9' {
		return "", "", "", false
	}
	for i := 1; i < len(number); i++ {
		if number[i] < '0' || number[i] > '9' {
			return "", "", "", false
		}
	}
	return owner, repo, number, true
}
func GitPath(path string) (owner, repo, service string, ok bool) {
	rest, ok := strings.CutPrefix(path, "/")
	if !ok {
		return "", "", "", false
	}
	owner, rest, ok = strings.Cut(rest, "/")
	if !ok || owner == "" {
		return "", "", "", false
	}
	segment, service, ok := strings.Cut(rest, "/")
	if !ok {
		return "", "", "", false
	}
	repo, ok = strings.CutSuffix(segment, ".git")
	if !ok || repo == "" {
		return "", "", "", false
	}
	switch service {
	case "info/refs", "git-upload-pack", "git-receive-pack":
		return owner, repo, service, true
	}
	return "", "", "", false
}

// MatchBearer preserves exact scheme and token comparison semantics.
func MatchBearer(header, token string) bool {
	if len(header) != 7+len(token) || header[:7] != "Bearer " {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(header[7:]), []byte(token)) == 1
}
