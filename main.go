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

var gitRoute = regexp.MustCompile(`^/([^/]+)/([^/]+)\.git/(info/refs|git-upload-pack|git-receive-pack)$`)
var hostRoute = regexp.MustCompile(`^(?:[A-Za-z0-9.-]+|\[::1\])(?::[0-9]{1,5})?$`)

var issueRoute = regexp.MustCompile(`^/api/v3/repos/([A-Za-z0-9_-][A-Za-z0-9_.-]*)/([A-Za-z0-9_-][A-Za-z0-9_.-]*)/issues(?:/([1-9][0-9]*))?/?$`)

type App struct {
	api           *Forge
	token, bearer []byte
	mu            sync.RWMutex
	sequence      atomic.Uint64
	cacheMu       sync.RWMutex
	issueCache    map[string]wireResponse
}

func newApp() *App {
	login := os.Getenv("MAKEFUN_FORGE_LOGIN")
	if login == "" {
		login = "adaiversen"
	}
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	return &App{api: NewForge(login, os.Getenv("MAKEFUN_TRIAL_NOW")), token: []byte(token), bearer: []byte("Bearer " + token)}
}
func (a *App) authorized(r *rawRequest) bool {
	if len(a.token) == 0 {
		return false
	}
	auth := r.auth
	if subtle.ConstantTimeCompare(auth, a.bearer) == 1 {
		return true
	}
	if !bytes.HasPrefix(auth, []byte("Basic ")) || !bytes.Contains(r.target, []byte(".git/")) {
		return false
	}
	b, err := base64.StdEncoding.AppendDecode(nil, auth[len("Basic "):])
	if err != nil {
		return false
	}
	_, pass, ok := bytes.Cut(b, []byte(":"))
	return ok && subtle.ConstantTimeCompare(pass, a.token) == 1
}
func (a *App) dispatchRaw(r *rawRequest) (x wireResponse) {
	id := "FORGE-" + strconv.FormatUint(a.sequence.Add(1), 36)
	defer func() {
		if recover() != nil {
			x = rawReply(fail(500, "Internal Server Error"), id)
		}
	}()
	raw := r.target
	if bytes.Equal(r.method, []byte("GET")) && bytes.Equal(raw, []byte("/healthz")) {
		return wireResponse{code: 200, typ: jsonType, body: []byte(`{"ok":true}`)}
	}
	send := func(reply Reply) { x = rawReply(reply, id) }
	if !bytes.HasPrefix(raw, []byte("/")) || bytes.HasPrefix(raw, []byte("//")) {
		send(fail(400, "Bad Request"))
		return
	}
	host := string(r.host)
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
	u, err := url.Parse("http://" + host + string(raw))
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
	match := gitRoute.FindStringSubmatch(escaped)
	if !isRest && !isGraph && !isMcp && match == nil {
		send(missing())
		return
	}
	if !a.authorized(r) {
		reply := fail(401, "Bad credentials")
		if match != nil {
			reply.Headers = map[string]string{"www-authenticate": `Basic realm="Forge"`}
		}
		send(reply)
		return
	}
	// Issue GETs and upload-pack only read application/Git state. Compression
	// caches have per-object locks; the JSON byte cache has its own RWMutex.
	shared := (isRest && bytes.Equal(r.method, []byte("GET")) && issueRoute.MatchString(escaped)) ||
		(match != nil && match[3] != "git-receive-pack" && (bytes.Equal(r.method, []byte("GET")) || bytes.Equal(r.method, []byte("POST"))))
	if shared {
		a.mu.RLock()
		defer a.mu.RUnlock()
	} else {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.api.Origin = "http://" + host
		if !bytes.Equal(r.method, []byte("GET")) && !bytes.Equal(r.method, []byte("HEAD")) {
			a.cacheMu.Lock()
			a.issueCache = nil
			a.cacheMu.Unlock()
		}
	}
	if isRest && bytes.Equal(r.method, []byte("GET")) {
		if hit, ok := a.cachedIssue(u, id); ok {
			return hit
		}
	}
	if isRest && (bytes.Equal(r.method, []byte("GET")) || bytes.Equal(r.method, []byte("HEAD"))) {
		send(a.api.Request(string(r.method), u, nil))
		return
	}
	data := r.body
	if match != nil {
		owner, e := url.PathUnescape(match[1])
		name, e2 := url.PathUnescape(match[2])
		if e != nil || e2 != nil {
			send(fail(400, "Bad path"))
			return
		}
		repo := a.api.Repo(owner, name)
		if repo == nil {
			send(missing())
			return
		}
		return repo.Git.rawHTTP(r.method, r.protocol, u, match[3], data)
	}
	if (isGraph || isMcp) && !bytes.Equal(r.method, []byte("POST")) {
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
		reply := a.api.Request(string(r.method), u, body)
		if reply.Status == 201 && bytes.Equal(r.method, []byte("POST")) && strings.HasSuffix(strings.TrimSuffix(escaped, "/"), "/issues") {
			if j, ok := reply.Body.(J); ok && j["repository_url"] != nil {
				return wireResponse{code: 201, typ: jsonType, id: id, body: issueJSON(nil, j)}
			}
		}
		send(reply)
	}
	return
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
func lifecycle(listener net.Listener, env string) error {
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
	app := newApp()
	go func() {
		if e := serveRaw(listener, app); e != nil && !errors.Is(e, net.ErrClosed) {
			log.Print(e)
		}
	}()
	fmt.Printf("[entry] listening on port %d\n", listener.Addr().(*net.TCPAddr).Port)
	if *standalone != "" {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
	} else if err = lifecycle(listener, env); err != nil {
		fmt.Fprintln(os.Stderr, "[entry]", err)
		_ = listener.Close()
		os.Exit(1)
	}
	_ = listener.Close()
	fmt.Println("[entry] done")
}
