package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const bodyLimit = 32 * 1024 * 1024

var hostRoute = regexp.MustCompile(`^(?:[A-Za-z0-9.-]+|\[::1\])(?::[0-9]{1,5})?$`)

type App struct {
	api      *Forge
	token    string
	mu       sync.RWMutex
	sequence atomic.Uint64
}

func (a *App) issueWire(w http.ResponseWriter, owner, name string, n int, id string) bool {
	repo := a.api.Repo(owner, name)
	if repo == nil {
		sendReply(w, nil, missing(), id)
		return true
	}
	repo.issueMu.RLock()
	wire := repo.issueCache[n]
	repo.issueMu.RUnlock()
	if wire == nil {
		if n != 0 {
			issue := repo.Issues[n]
			if issue == nil {
				sendReply(w, nil, missing(), id)
				return true
			}
			wire = []byte(js(issue))
		} else {
			if len(repo.Issues) > 30 {
				return false
			}
			list := []any{}
			for i := len(repo.IssueOrder) - 1; i >= 0; i-- {
				v := repo.Issues[repo.IssueOrder[i]]
				if v["state"] == "open" {
					list = append(list, v)
				}
			}
			wire = []byte(js(list))
		}
		repo.issueMu.Lock()
		if repo.issueCache == nil {
			repo.issueCache = make(map[int][]byte)
		}
		if existing := repo.issueCache[n]; existing != nil {
			wire = existing
		} else {
			repo.issueCache[n] = wire
		}
		repo.issueMu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(wire)))
	w.Header().Set("X-Github-Request-Id", id)
	w.WriteHeader(200)
	_, _ = w.Write(wire)
	return true
}
func newApp() *App {
	login := os.Getenv("MAKEFUN_FORGE_LOGIN")
	if login == "" {
		login = "adaiversen"
	}
	return &App{api: NewForge(login, os.Getenv("MAKEFUN_TRIAL_NOW")), token: os.Getenv("MAKEFUN_SHOP_TOKEN")}
}
func (a *App) authorized(r *http.Request) bool {
	if a.token == "" {
		return false
	}
	auth := r.Header.Get("Authorization")
	if subtle.ConstantTimeCompare([]byte(auth), []byte("Bearer "+a.token)) == 1 {
		return true
	}
	if !strings.HasPrefix(auth, "Basic ") || !strings.Contains(r.RequestURI, ".git/") {
		return false
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
	if err != nil {
		return false
	}
	_, pass, ok := strings.Cut(string(b), ":")
	return ok && subtle.ConstantTimeCompare([]byte(pass), []byte(a.token)) == 1
}
func sendReply(w http.ResponseWriter, r *http.Request, reply Reply, id string) {
	var data []byte
	if reply.Status != 204 && reply.Status != 202 && reply.Body != nil {
		data = []byte(js(reply.Body))
	} else if reply.Status != 204 && reply.Status != 202 && reply.Body == nil {
		data = []byte("null")
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("X-Github-Request-Id", id)
	for k, v := range reply.Headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(reply.Status)
	if r == nil || r.Method != "HEAD" {
		_, _ = w.Write(data)
	}
}
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := "FORGE-" + strconv.FormatUint(a.sequence.Add(1), 36)
	defer func() {
		if recover() != nil {
			sendReply(w, r, fail(500, "Internal Server Error"), id)
		}
	}()
	raw := r.RequestURI
	if r.Method == "GET" && raw == "/healthz" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Length", "11")
		_, _ = w.Write([]byte(`{"ok":true}`))
		return
	}
	send := func(reply Reply) { sendReply(w, r, reply, id) }
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		send(fail(400, "Bad Request"))
		return
	}
	host := r.Host
	if host == "" {
		host = "localhost"
	}
	if !hostRoute.MatchString(host) {
		send(fail(400, "Bad Request"))
		return
	}
	if _, port, e := net.SplitHostPort(host); e == nil {
		n, e := strconv.Atoi(port)
		if e != nil || n > 65535 {
			send(fail(400, "Bad Request"))
			return
		}
	}
	u, err := url.Parse("http://" + host + raw)
	if err != nil {
		send(fail(400, "Bad Request"))
		return
	}
	p := u.Path
	isRest := p == "/api/v3" || strings.HasPrefix(p, "/api/v3/")
	isGraph := p == "/api/graphql"
	isMcp := p == "/mcp"
	// Match encoded path as the Node URL implementation does; decode route captures once.
	escaped := u.EscapedPath()
	gitOwner, gitName, service := gitPath(escaped)
	issueOwner, issueName, num, issue := "", "", 0, false
	if isRest && r.Method == "GET" {
		issueOwner, issueName, num, issue = issuePath(escaped, u.RawQuery)
	}
	if !isRest && !isGraph && !isMcp && service == "" {
		send(missing())
		return
	}
	if !a.authorized(r) {
		reply := fail(401, "Bad credentials")
		if service != "" {
			reply.Headers = map[string]string{"www-authenticate": `Basic realm="Forge"`}
		}
		send(reply)
		return
	}
	// Issue GETs and upload-pack only read application/Git state. Compression
	// caches have per-object locks; the JSON byte cache has its own RWMutex.
	shared := issue ||
		(service != "" && service != "git-receive-pack" && (r.Method == "GET" || r.Method == "POST"))
	if shared {
		a.mu.RLock()
		defer a.mu.RUnlock()
	} else {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.api.Origin = "http://" + host
	}
	if issue && a.issueWire(w, issueOwner, issueName, num, id) {
		return
	}
	if isRest && (r.Method == "GET" || r.Method == "HEAD") {
		send(a.api.Request(r.Method, u, nil))
		return
	}
	var data []byte
	if r.Method != "GET" && r.Method != "HEAD" {
		data, err = io.ReadAll(io.LimitReader(r.Body, bodyLimit+1))
		if err != nil {
			send(fail(400, "Invalid request body"))
			return
		}
		if len(data) > bodyLimit {
			send(fail(413, "Invalid request body"))
			return
		}
	}
	if service != "" {
		owner, e := url.PathUnescape(gitOwner)
		name, e2 := url.PathUnescape(gitName)
		if e != nil || e2 != nil {
			send(fail(400, "Bad path"))
			return
		}
		repo := a.api.Repo(owner, name)
		if repo == nil {
			send(missing())
			return
		}
		repo.Git.HTTP(w, r, u, service, data)
		return
	}
	if (isGraph || isMcp) && r.Method != "POST" {
		send(fail(405, "Method Not Allowed"))
		return
	}
	var body any
	if len(data) > 0 {
		if json.Unmarshal(data, &body) != nil {
			if isMcp {
				send(out(400, J{"jsonrpc": "2.0", "id": nil, "error": J{"code": -32700, "message": "Parse error"}}))
			} else {
				send(fail(400, "Problems parsing JSON"))
			}
			return
		}
	}
	if isGraph {
		send(out(200, GraphQL(body, a.api, a.api.Origin)))
	} else if isMcp {
		send(MCP(body, a.api))
	} else {
		send(a.api.Request(r.Method, u, body))
	}
}
func retryDelay(attempt int) time.Duration {
	if attempt > 3 {
		return 2 * time.Second
	}
	d := 250 * time.Millisecond * time.Duration(1<<attempt)
	if d > 2*time.Second {
		return 2 * time.Second
	}
	return d
}
func lifecycle(server *http.Server, listener net.Listener, env string) error {
	client := &http.Client{Timeout: 30 * time.Second}
	port := listener.Addr().(*net.TCPAddr).Port
	for attempt := 0; ; attempt++ {
		resp, err := client.Post(env+"/register", "application/json", bytes.NewBufferString(js(J{"port": port})))
		if err == nil {
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 || resp.StatusCode == 409 {
				break
			}
			if resp.StatusCode == 400 || resp.StatusCode == 405 {
				return fmt.Errorf("register answered HTTP %d: %s", resp.StatusCode, data)
			}
		}
		time.Sleep(retryDelay(attempt))
	}
	fmt.Println("[entry] registered; waiting for the env to say done")
	for attempt := 0; ; attempt++ {
		resp, err := client.Get(env + "/done")
		if err == nil {
			var body J
			e := json.NewDecoder(resp.Body).Decode(&body)
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 && e == nil && body["done"] == true {
				return nil
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}
func main() {
	standalone := flag.String("listen", "", "standalone diagnostic address; otherwise use official lifecycle")
	flag.Parse()
	env := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if env == "" && *standalone == "" {
		fmt.Fprintln(os.Stderr, "[entry] MAKEFUN_ENV_URL is not set: there is no env to register with")
		os.Exit(1)
	}
	address := ":0"
	if *standalone != "" {
		address = *standalone
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		log.Fatal(err)
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	server := &http.Server{Protocols: protocols, Handler: newApp(), IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384}
	go func() {
		if e := server.Serve(listener); e != nil && !errors.Is(e, http.ErrServerClosed) {
			log.Print(e)
		}
	}()
	fmt.Printf("[entry] listening on port %d\n", listener.Addr().(*net.TCPAddr).Port)
	if *standalone != "" {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
	} else if err = lifecycle(server, listener, env); err != nil {
		fmt.Fprintln(os.Stderr, "[entry]", err)
		_ = server.Close()
		os.Exit(1)
	}
	_ = server.Close()
	fmt.Println("[entry] done")
}
