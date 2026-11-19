package main

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type rawRequest struct {
	method, target, host, auth, protocol []byte
	fields                               []byte
	close                                bool
	length                               int
	body                                 []byte
}

func token(s []byte) bool {
	if len(s) == 0 {
		return false
	}
	for _, b := range s {
		if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", b) >= 0) {
			return false
		}
	}
	return true
}
func (q *rawRequest) read(r *bufio.Reader) int {
	*q = rawRequest{fields: q.fields[:0]}
	if q.fields == nil {
		q.fields = make([]byte, 0, 16384)
	}
	save := func(v []byte) []byte {
		at := len(q.fields)
		q.fields = append(q.fields, v...)
		return q.fields[at:len(q.fields)]
	}
	line, e := r.ReadSlice('\n')
	if e == io.EOF && len(line) == 0 {
		return -1
	}
	if e != nil || !bytes.HasSuffix(line, []byte("\r\n")) {
		return 400
	}
	method, rest, ok := bytes.Cut(line[:len(line)-2], []byte(" "))
	target, version, ok2 := bytes.Cut(rest, []byte(" "))
	if !ok || !ok2 || !token(method) || len(target) == 0 {
		return 400
	}
	for _, c := range target {
		if c <= 32 || c == 127 {
			return 400
		}
	}
	q.method, q.target = save(method), save(target)
	switch {
	case bytes.Equal(version, []byte("HTTP/1.1")):
	case bytes.Equal(version, []byte("HTTP/1.0")):
		q.close = true
	default:
		return 400
	}
	size := len(line)
	hasLen, hasHost, hasAuth, hasProto := false, false, false, false
	for {
		line, e = r.ReadSlice('\n')
		size += len(line)
		if e != nil || size > 16384 || !bytes.HasSuffix(line, []byte("\r\n")) {
			return 400
		}
		if len(line) == 2 {
			break
		}
		name, v, ok := bytes.Cut(line[:len(line)-2], []byte(":"))
		if !ok || !token(name) {
			return 400
		}
		v = bytes.Trim(v, " 	")
		for _, c := range v {
			if c < 32 && c != 9 || c == 127 {
				return 400
			}
		}
		switch {
		case bytes.EqualFold(name, []byte("host")):
			if hasHost || len(v) == 0 {
				return 400
			}
			hasHost = true
			q.host = save(v)
		case bytes.EqualFold(name, []byte("authorization")):
			if hasAuth {
				return 400
			}
			hasAuth = true
			q.auth = save(v)
		case bytes.EqualFold(name, []byte("git-protocol")):
			if hasProto {
				return 400
			}
			hasProto = true
			q.protocol = save(v)
		case bytes.EqualFold(name, []byte("transfer-encoding")):
			return 501
		case bytes.EqualFold(name, []byte("expect")):
			return 417
		case bytes.EqualFold(name, []byte("content-length")):
			if hasLen || len(v) == 0 {
				return 400
			}
			hasLen = true
			for _, c := range v {
				if c < '0' || c > '9' {
					return 400
				}
				q.length = q.length*10 + int(c-'0')
				if q.length > bodyLimit {
					return 413
				}
			}
		case bytes.EqualFold(name, []byte("connection")):
			for s := v; len(s) != 0; {
				var part []byte
				part, s, _ = bytes.Cut(s, []byte(","))
				if bytes.EqualFold(bytes.Trim(part, " 	"), []byte("close")) {
					q.close = true
				}
			}
		}
	}
	if !hasHost && !q.close {
		return 400
	}
	return 0
}
func mustRawURL(path string) *url.URL {
	u, err := url.Parse("http://localhost" + path)
	if err != nil {
		panic(err)
	}
	return u
}

type wireResponse struct {
	code    int
	typ, id string
	body    []byte
	extra   map[string]string
	git     bool
	direct  bool
	cached  []byte
	idAt    int
}

func appendWire(b []byte, x wireResponse, head, closeConn bool) []byte {
	b = append(b, "HTTP/1.1 "...)
	b = strconv.AppendInt(b, int64(x.code), 10)
	b = append(b, ' ')
	b = append(b, http.StatusText(x.code)...)
	b = append(b, "\r\nContent-Type: "...)
	b = append(b, x.typ...)
	b = append(b, "\r\nContent-Length: "...)
	b = strconv.AppendInt(b, int64(len(x.body)), 10)
	if x.id != "" {
		b = append(b, "\r\nX-Github-Request-Id: "...)
		b = append(b, x.id...)
	}
	if x.git {
		b = append(b, "\r\nCache-Control: no-cache"...)
	}
	for k, v := range x.extra {
		if strings.ContainsAny(k+v, "\r\n") {
			continue
		}
		b = append(b, "\r\n"...)
		b = append(b, k...)
		b = append(b, ": "...)
		b = append(b, v...)
	}
	if closeConn {
		b = append(b, "\r\nConnection: close"...)
	}
	b = append(b, "\r\n\r\n"...)
	if !head {
		b = append(b, x.body...)
	}
	return b
}
func writeWire(c net.Conn, b []byte) error {
	for len(b) > 0 {
		n, e := c.Write(b)
		if e != nil {
			return e
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
func serveRaw(l net.Listener, a *App) error {
	slots := make(chan struct{}, 256)
	for {
		c, e := l.Accept()
		if e != nil {
			return e
		}
		select {
		case slots <- struct{}{}:
		default:
			c.Close()
			continue
		}
		if tcp, ok := c.(*net.TCPConn); ok {
			_ = tcp.SetNoDelay(true)
		}
		go func() { defer func() { c.Close(); <-slots }(); serveRawConn(c, a) }()
	}
}
func serveRawConn(c net.Conn, a *App) {
	rd := bufio.NewReaderSize(c, 16384)
	var q rawRequest
	var scratch []byte
	wire := make([]byte, 0, 4096)
	for {
		_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
		status := q.read(rd)
		if status < 0 {
			return
		}
		if status != 0 {
			_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
			_ = writeWire(c, appendWire(wire[:0], wireResponse{code: status, typ: "application/json", body: []byte(`{"message":"Invalid request"}`)}, false, true))
			return
		}
		if q.length > 0 {
			if cap(scratch) < q.length {
				scratch = make([]byte, q.length)
			}
			q.body = scratch[:q.length]
			if _, e := io.ReadFull(rd, q.body); e != nil {
				return
			}
		}
		x := a.dispatchRaw(&q)
		if x.direct && !q.close {
			_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if writeWire(c, x.cached) != nil {
				return
			}
			if cap(scratch) > 256*1024 {
				scratch = nil
			}
			continue
		}
		if len(x.cached) > 0 && !q.close {
			wire = append(wire[:0], x.cached...)
			off := x.idAt
			for i := 0; i < 20; i++ {
				wire[off+i] = ' '
			}
			copy(wire[off+20-len(x.id):off+20], x.id)
		} else {
			wire = appendWire(wire[:0], x, bytes.Equal(q.method, []byte("HEAD")), q.close)
		}
		_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if writeWire(c, wire) != nil || q.close {
			return
		}
		if cap(scratch) > 256*1024 {
			scratch = nil
		}
		if cap(wire) > 256*1024 {
			wire = nil
		}
	}
}
