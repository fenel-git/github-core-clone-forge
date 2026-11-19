package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type gqlToken struct {
	kind, value string
	offset      int
}
type gqlDirective struct {
	name string
	args J
}
type gqlSelection struct {
	kind, name, alias, condition string
	args                         J
	directives                   []gqlDirective
	selections                   []gqlSelection
}
type gqlVariable struct {
	name, typ  string
	def        any
	hasDefault bool
}
type gqlOperation struct {
	kind, name string
	variables  []gqlVariable
	selections []gqlSelection
}
type gqlFragment struct {
	condition  string
	directives []gqlDirective
	selections []gqlSelection
}
type gqlDocument struct {
	operations []gqlOperation
	fragments  map[string]gqlFragment
}
type gqlParser struct {
	tokens []gqlToken
	at     int
}
type gqlError struct{ message string }

func (e gqlError) Error() string              { return e.message }
func gqlErr(format string, args ...any) error { return gqlError{fmt.Sprintf(format, args...)} }

var gqlNameRE = regexp.MustCompile(`^[_A-Za-z][_0-9A-Za-z]*$`)
var gqlNumRE = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)

func gqlLex(src string) ([]gqlToken, error) {
	out := []gqlToken{}
	for i := 0; i < len(src); {
		c := src[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == ',' || c == 0xef && i+2 < len(src) && src[i+1] == 0xbb && src[i+2] == 0xbf {
			if c == 0xef {
				i += 3
			} else {
				i++
			}
			continue
		}
		if c == '#' {
			for i < len(src) && src[i] != '\r' && src[i] != '\n' {
				i++
			}
			continue
		}
		start := i
		if strings.HasPrefix(src[i:], "...") {
			out = append(out, gqlToken{"punct", "...", i})
			i += 3
			continue
		}
		if c == '"' {
			if strings.HasPrefix(src[i:], `"""`) {
				i += 3
				var b strings.Builder
				closed := false
				for i < len(src) {
					if strings.HasPrefix(src[i:], `\"""`) {
						b.WriteString(`"""`)
						i += 4
						continue
					}
					if strings.HasPrefix(src[i:], `"""`) {
						i += 3
						closed = true
						break
					}
					b.WriteByte(src[i])
					i++
				}
				if !closed {
					return nil, gqlErr("Unterminated block string at offset %d", start)
				}
				out = append(out, gqlToken{"string", b.String(), start})
				continue
			}
			i++
			escaped := false
			closed := false
			for i < len(src) {
				x := src[i]
				i++
				if !escaped && x == '"' {
					closed = true
					break
				}
				if !escaped && x == '\\' {
					escaped = true
				} else {
					escaped = false
				}
			}
			if !closed {
				return nil, gqlErr("Unterminated string at offset %d", start)
			}
			var val string
			if err := json.Unmarshal([]byte(src[start:i]), &val); err != nil {
				return nil, gqlErr("Invalid string at offset %d", start)
			}
			out = append(out, gqlToken{"string", val, start})
			continue
		}
		if c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' {
			i++
			for i < len(src) {
				x := src[i]
				if x == '_' || x >= 'A' && x <= 'Z' || x >= 'a' && x <= 'z' || x >= '0' && x <= '9' {
					i++
				} else {
					break
				}
			}
			out = append(out, gqlToken{"name", src[start:i], start})
			continue
		}
		if c == '-' || c >= '0' && c <= '9' {
			i++
			for i < len(src) {
				x := src[i]
				if x >= '0' && x <= '9' || x == '.' || x == 'e' || x == 'E' || x == '+' || x == '-' {
					i++
				} else {
					break
				}
			}
			v := src[start:i]
			if !gqlNumRE.MatchString(v) {
				return nil, gqlErr("Invalid number %q at offset %d", v, start)
			}
			out = append(out, gqlToken{"number", v, start})
			continue
		}
		if strings.ContainsRune("!$():=@[]{|}&", rune(c)) {
			out = append(out, gqlToken{"punct", string(c), i})
			i++
			continue
		}
		return nil, gqlErr("Unexpected character %q at offset %d", string(c), i)
	}
	out = append(out, gqlToken{"eof", "", len(src)})
	return out, nil
}
func newGQLParser(src string) (*gqlParser, error) {
	t, e := gqlLex(src)
	if e != nil {
		return nil, e
	}
	return &gqlParser{tokens: t}, nil
}
func (p *gqlParser) peek() gqlToken {
	if p.at >= len(p.tokens) {
		return gqlToken{kind: "eof"}
	}
	return p.tokens[p.at]
}
func (p *gqlParser) take() gqlToken { t := p.peek(); p.at++; return t }
func (p *gqlParser) expect(s string) error {
	t := p.take()
	if t.value != s {
		return gqlErr("Expected %q at offset %d, got %q", s, t.offset, t.value)
	}
	return nil
}
func (p *gqlParser) name() (string, error) {
	t := p.take()
	if t.kind != "name" {
		return "", gqlErr("Expected a name at offset %d, got %q", t.offset, t.value)
	}
	return t.value, nil
}
func (p *gqlParser) typeRef() (string, error) {
	var typ string
	if p.peek().value == "[" {
		p.take()
		inner, e := p.typeRef()
		if e != nil {
			return "", e
		}
		if e = p.expect("]"); e != nil {
			return "", e
		}
		typ = "[" + inner + "]"
	} else {
		n, e := p.name()
		if e != nil {
			return "", e
		}
		typ = n
	}
	if p.peek().value == "!" {
		p.take()
		typ += "!"
	}
	return typ, nil
}
func (p *gqlParser) value() (any, error) {
	t := p.peek()
	switch t.value {
	case "$":
		p.take()
		n, e := p.name()
		if e != nil {
			return nil, e
		}
		return J{"__gql_variable": n}, nil
	case "[":
		p.take()
		a := []any{}
		for p.peek().value != "]" {
			if p.peek().kind == "eof" {
				return nil, gqlErr("Unterminated list at offset %d", t.offset)
			}
			v, e := p.value()
			if e != nil {
				return nil, e
			}
			a = append(a, v)
		}
		p.take()
		return a, nil
	case "{":
		p.take()
		o := J{}
		for p.peek().value != "}" {
			if p.peek().kind == "eof" {
				return nil, gqlErr("Unterminated input object at offset %d", t.offset)
			}
			k, e := p.name()
			if e != nil {
				return nil, e
			}
			if e = p.expect(":"); e != nil {
				return nil, e
			}
			v, e := p.value()
			if e != nil {
				return nil, e
			}
			o[k] = v
		}
		p.take()
		return o, nil
	}
	if t.kind == "string" {
		p.take()
		return t.value, nil
	}
	if t.kind == "number" {
		p.take()
		if strings.ContainsAny(t.value, ".eE") {
			v, _ := strconv.ParseFloat(t.value, 64)
			return v, nil
		}
		v, e := strconv.ParseInt(t.value, 10, 64)
		if e == nil {
			return v, nil
		}
		return json.Number(t.value), nil
	}
	if t.kind == "name" {
		p.take()
		switch t.value {
		case "null":
			return nil, nil
		case "true":
			return true, nil
		case "false":
			return false, nil
		default:
			return t.value, nil
		}
	}
	return nil, gqlErr("Expected a value at offset %d", t.offset)
}
func (p *gqlParser) args() (J, error) {
	a := J{}
	if p.peek().value != "(" {
		return a, nil
	}
	p.take()
	for p.peek().value != ")" {
		if p.peek().kind == "eof" {
			return nil, gqlErr("Unterminated arguments")
		}
		k, e := p.name()
		if e != nil {
			return nil, e
		}
		if e = p.expect(":"); e != nil {
			return nil, e
		}
		v, e := p.value()
		if e != nil {
			return nil, e
		}
		if _, ok := a[k]; ok {
			return nil, gqlErr("There can be only one argument named %q", k)
		}
		a[k] = v
	}
	p.take()
	return a, nil
}
func (p *gqlParser) directives() ([]gqlDirective, error) {
	a := []gqlDirective{}
	for p.peek().value == "@" {
		p.take()
		n, e := p.name()
		if e != nil {
			return nil, e
		}
		args, e := p.args()
		if e != nil {
			return nil, e
		}
		a = append(a, gqlDirective{n, args})
	}
	return a, nil
}
func (p *gqlParser) variableDefs() ([]gqlVariable, error) {
	v := []gqlVariable{}
	if p.peek().value != "(" {
		return v, nil
	}
	p.take()
	seen := map[string]bool{}
	for p.peek().value != ")" {
		if p.peek().kind == "eof" {
			return nil, gqlErr("Unterminated variable definitions")
		}
		if e := p.expect("$"); e != nil {
			return nil, e
		}
		n, e := p.name()
		if e != nil {
			return nil, e
		}
		if seen[n] {
			return nil, gqlErr("There can be only one variable named $%s.", n)
		}
		seen[n] = true
		if e = p.expect(":"); e != nil {
			return nil, e
		}
		typ, e := p.typeRef()
		if e != nil {
			return nil, e
		}
		x := gqlVariable{name: n, typ: typ}
		if p.peek().value == "=" {
			p.take()
			x.def, e = p.value()
			if e != nil {
				return nil, e
			}
			x.hasDefault = true
		}
		if _, e = p.directives(); e != nil {
			return nil, e
		}
		v = append(v, x)
	}
	p.take()
	return v, nil
}
func (p *gqlParser) selectionSet() ([]gqlSelection, error) {
	if e := p.expect("{"); e != nil {
		return nil, e
	}
	s := []gqlSelection{}
	for p.peek().value != "}" {
		if p.peek().kind == "eof" {
			return nil, gqlErr("Unterminated selection set")
		}
		x, e := p.selection()
		if e != nil {
			return nil, e
		}
		s = append(s, x)
	}
	p.take()
	if len(s) == 0 {
		return nil, gqlErr("Selection set must not be empty.")
	}
	return s, nil
}
func (p *gqlParser) selection() (gqlSelection, error) {
	if p.peek().value == "..." {
		p.take()
		if p.peek().value == "on" {
			p.take()
			cond, e := p.name()
			if e != nil {
				return gqlSelection{}, e
			}
			d, e := p.directives()
			if e != nil {
				return gqlSelection{}, e
			}
			s, e := p.selectionSet()
			return gqlSelection{kind: "inline", condition: cond, directives: d, selections: s}, e
		}
		if p.peek().value == "@" || p.peek().value == "{" {
			d, e := p.directives()
			if e != nil {
				return gqlSelection{}, e
			}
			s, e := p.selectionSet()
			return gqlSelection{kind: "inline", directives: d, selections: s}, e
		}
		n, e := p.name()
		if e != nil {
			return gqlSelection{}, e
		}
		d, e := p.directives()
		return gqlSelection{kind: "spread", name: n, directives: d}, e
	}
	first, e := p.name()
	if e != nil {
		return gqlSelection{}, e
	}
	n, a := first, first
	if p.peek().value == ":" {
		p.take()
		a = first
		n, e = p.name()
		if e != nil {
			return gqlSelection{}, e
		}
	}
	args, e := p.args()
	if e != nil {
		return gqlSelection{}, e
	}
	d, e := p.directives()
	if e != nil {
		return gqlSelection{}, e
	}
	s := []gqlSelection{}
	if p.peek().value == "{" {
		s, e = p.selectionSet()
	}
	return gqlSelection{kind: "field", name: n, alias: a, args: args, directives: d, selections: s}, e
}
func parseGQL(src string) (gqlDocument, error) {
	p, e := newGQLParser(src)
	if e != nil {
		return gqlDocument{}, e
	}
	d := gqlDocument{fragments: map[string]gqlFragment{}}
	for p.peek().kind != "eof" {
		if p.peek().value == "{" {
			s, e := p.selectionSet()
			if e != nil {
				return d, e
			}
			d.operations = append(d.operations, gqlOperation{kind: "query", selections: s})
			continue
		}
		def, e := p.name()
		if e != nil {
			return d, e
		}
		if def == "fragment" {
			n, e := p.name()
			if e != nil {
				return d, e
			}
			if n == "on" {
				return d, gqlErr("Fragment name cannot be \"on\".")
			}
			if _, ok := d.fragments[n]; ok {
				return d, gqlErr("There can be only one fragment named %q.", n)
			}
			if e = p.expect("on"); e != nil {
				return d, e
			}
			cond, e := p.name()
			if e != nil {
				return d, e
			}
			dirs, e := p.directives()
			if e != nil {
				return d, e
			}
			sel, e := p.selectionSet()
			if e != nil {
				return d, e
			}
			d.fragments[n] = gqlFragment{cond, dirs, sel}
			continue
		}
		if def != "query" && def != "mutation" && def != "subscription" {
			return d, gqlErr("Unexpected definition %q", def)
		}
		op := gqlOperation{kind: def}
		if p.peek().kind == "name" {
			op.name = p.take().value
		}
		op.variables, e = p.variableDefs()
		if e != nil {
			return d, e
		}
		if _, e = p.directives(); e != nil {
			return d, e
		}
		op.selections, e = p.selectionSet()
		if e != nil {
			return d, e
		}
		d.operations = append(d.operations, op)
	}
	if len(d.operations) == 0 {
		return d, gqlErr("Document contains no operation")
	}
	return d, nil
}

func gqlResolve(v any, vars J) any {
	switch x := v.(type) {
	case map[string]any:
		if n, ok := x["__gql_variable"].(string); ok {
			return vars[n]
		}
		o := J{}
		for k, y := range x {
			o[k] = gqlResolve(y, vars)
		}
		return o
	case []any:
		a := make([]any, len(x))
		for i, y := range x {
			a[i] = gqlResolve(y, vars)
		}
		return a
	default:
		return v
	}
}
func gqlID(p J) string {
	b, _ := json.Marshal(p)
	return "gql1_" + base64.RawURLEncoding.EncodeToString(b)
}
func gqlDecodeID(v any) J {
	s, ok := v.(string)
	if !ok || !strings.HasPrefix(s, "gql1_") {
		return nil
	}
	b, e := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, "gql1_"))
	if e != nil {
		return nil
	}
	var m J
	if json.Unmarshal(b, &m) != nil || num(m["version"]) != 1 {
		return nil
	}
	return m
}
func gqlString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
func gqlRepoPath(owner, name string) string {
	return "/api/v3/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
}
func gqlRequest(api *Forge, method, path string, body any, allowMissing bool) (any, error) {
	u, e := url.Parse(path)
	if e != nil {
		return nil, e
	}
	r := api.Request(method, u, body)
	if allowMissing && r.Status == 404 {
		return nil, nil
	}
	if r.Status < 200 || r.Status >= 300 {
		msg := gqlString(obj(r.Body)["message"])
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", r.Status)
		}
		return nil, gqlErr("%s", msg)
	}
	return r.Body, nil
}
func gqlLocator(repo J) (string, string) {
	owner := gqlString(repo["owner"])
	if owner == "" {
		owner = gqlString(obj(repo["owner"])["login"])
	}
	full := gqlString(repo["full_name"])
	if full == "" {
		full = gqlString(repo["nameWithOwner"])
	}
	parts := strings.Split(full, "/")
	name := gqlString(repo["name"])
	if name == "" && len(parts) > 0 {
		name = parts[len(parts)-1]
	}
	if owner == "" && len(parts) > 1 {
		owner = parts[0]
	}
	return owner, name
}
func gqlRepoByID(api *Forge, id any) J {
	d := gqlDecodeID(id)
	if d != nil && d["type"] == "Repository" {
		for _, x := range []any{d["nodeId"], d["databaseId"]} {
			if x != nil {
				if r := api.RepoByID(x); r != nil {
					return r
				}
			}
		}
		o, n := gqlString(d["owner"]), gqlString(d["name"])
		if o != "" && n != "" {
			v, _ := gqlRequest(api, "GET", gqlRepoPath(o, n), nil, true)
			return obj(v)
		}
		return nil
	}
	return api.RepoByID(id)
}
func gqlIssueByID(api *Forge, id any) (J, string, string, int) {
	d := gqlDecodeID(id)
	if d != nil && d["type"] == "Issue" {
		o, r := gqlString(d["owner"]), gqlString(d["repo"])
		n := num(d["number"])
		if o != "" && r != "" && n > 0 {
			v, _ := gqlRequest(api, "GET", fmt.Sprintf("%s/issues/%d", gqlRepoPath(o, r), n), nil, true)
			return obj(v), o, r, n
		}
		if x := api.IssueByID(d["nodeId"]); x != nil {
			return x, o, r, num(obj(x)["number"])
		}
	}
	if x := api.IssueByID(id); x != nil {
		return x, "", "", num(obj(x)["number"])
	}
	return nil, "", "", 0
}
func gqlPullByID(api *Forge, id any) (J, string, string, int) {
	d := gqlDecodeID(id)
	if d == nil || d["type"] != "PullRequest" {
		return nil, "", "", 0
	}
	o, r, n := gqlString(d["owner"]), gqlString(d["repo"]), num(d["number"])
	if o == "" || r == "" || n == 0 {
		return nil, o, r, n
	}
	v, _ := gqlRequest(api, "GET", fmt.Sprintf("%s/pulls/%d", gqlRepoPath(o, r), n), nil, true)
	return obj(v), o, r, n
}
func gqlUpperState(v any) any {
	s := gqlString(v)
	if s == "" {
		return nil
	}
	if strings.EqualFold(s, "open") {
		return "OPEN"
	}
	if strings.EqualFold(s, "closed") {
		return "CLOSED"
	}
	return strings.ToUpper(s)
}
func gqlTypeFields(n string) []string {
	switch n {
	case "Issue":
		return []string{"id", "number", "title", "state", "body"}
	case "PullRequest":
		return []string{"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files"}
	case "Repository":
		return []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"}
	case "Query":
		return []string{"viewer", "repository", "node", "__type"}
	case "Mutation":
		return []string{"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"}
	case "User":
		return []string{"login", "id", "name"}
	case "IssueConnection":
		return []string{"nodes", "pageInfo"}
	case "PullRequestConnection":
		return []string{"nodes"}
	case "PageInfo":
		return []string{"hasNextPage", "endCursor"}
	case "PullRequestChangedFile":
		return []string{"path"}
	case "PullRequestFileConnection":
		return []string{"nodes"}
	case "Ref":
		return []string{"name"}
	case "__Type":
		return []string{"name", "fields", "enumValues"}
	case "__Field", "__EnumValue":
		return []string{"name"}
	case "CreateIssuePayload", "CloseIssuePayload":
		return []string{"issue"}
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		return []string{"pullRequest"}
	case "AddCommentPayload":
		return []string{"commentEdge"}
	case "CommentEdge":
		return []string{"node"}
	case "Comment":
		return []string{"id", "body"}
	}
	return nil
}

var gqlEnums = map[string][]string{"IssueState": {"OPEN", "CLOSED"}, "MergeableState": {"MERGEABLE", "CONFLICTING", "UNKNOWN"}, "PullRequestState": {"OPEN", "CLOSED", "MERGED"}, "PullRequestMergeMethod": {"MERGE", "SQUASH", "REBASE"}, "RepositoryPermission": {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"}, "SearchType": {"ISSUE", "REPOSITORY", "USER", "DISCUSSION"}, "IssueOrderField": {"CREATED_AT", "UPDATED_AT", "COMMENTS"}, "OrderDirection": {"ASC", "DESC"}}

func gqlIntrospection(n any) any {
	name, ok := n.(string)
	if !ok {
		return nil
	}
	if f := gqlTypeFields(name); f != nil {
		a := []any{}
		for _, x := range f {
			a = append(a, J{"name": x})
		}
		return J{"name": name, "fields": a}
	}
	if vals, ok := gqlEnums[name]; ok {
		a := []any{}
		for _, x := range vals {
			a = append(a, J{"name": x})
		}
		return J{"name": name, "enumValues": a}
	}
	return nil
}
func gqlAllowed(t string) []string { return gqlTypeFields(t) }
func gqlChild(t, f string) string {
	m := map[string]map[string]string{
		"Query": {"viewer": "User", "repository": "Repository", "node": "Node", "__type": "__Type"}, "Mutation": {"createIssue": "CreateIssuePayload", "closeIssue": "CloseIssuePayload", "createPullRequest": "CreatePullRequestPayload", "mergePullRequest": "MergePullRequestPayload", "addComment": "AddCommentPayload"}, "Repository": {"owner": "User", "defaultBranchRef": "Ref", "issue": "Issue", "issues": "IssueConnection", "pullRequests": "PullRequestConnection", "pullRequest": "PullRequest"}, "IssueConnection": {"nodes": "Issue", "pageInfo": "PageInfo"}, "PullRequestConnection": {"nodes": "PullRequest"}, "PullRequest": {"files": "PullRequestFileConnection"}, "PullRequestFileConnection": {"nodes": "PullRequestChangedFile"}, "__Type": {"fields": "__Field", "enumValues": "__EnumValue"}, "CreateIssuePayload": {"issue": "Issue"}, "CloseIssuePayload": {"issue": "Issue"}, "CreatePullRequestPayload": {"pullRequest": "PullRequest"}, "MergePullRequestPayload": {"pullRequest": "PullRequest"}, "AddCommentPayload": {"commentEdge": "CommentEdge"}, "CommentEdge": {"node": "Comment"}}
	return m[t][f]
}
func gqlPageCursor(n int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("issues:%d", n)))
}
func gqlCursor(v any) int {
	s, ok := v.(string)
	if !ok {
		return 0
	}
	b, e := base64.RawURLEncoding.DecodeString(s)
	if e != nil {
		return 0
	}
	var n int
	if _, e = fmt.Sscanf(string(b), "issues:%d", &n); e != nil || n < 0 {
		return 0
	}
	return n
}
func gqlList(api *Forge, path string) ([]any, error) {
	v, e := gqlRequest(api, "GET", path, nil, false)
	if e != nil {
		return nil, e
	}
	return arr(v), nil
}
func gqlIssueConnection(api *Forge, owner, name string, args J) (any, error) {
	order := obj(args["orderBy"])
	direction := strings.ToLower(gqlString(order["direction"]))
	if direction == "" {
		direction = "desc"
	}
	first := 30
	if n, ok := args["first"]; ok {
		first = num(n)
		if first < 0 {
			first = 0
		}
	}
	if first > 100 {
		first = 100
	}
	states := []string{}
	for _, v := range arr(args["states"]) {
		states = append(states, strings.ToUpper(jsString(v)))
	}
	params := url.Values{}
	params.Set("per_page", "100")
	params.Set("page", "1")
	if len(states) == 1 {
		if states[0] == "OPEN" {
			params.Set("state", "open")
		} else if states[0] == "CLOSED" {
			params.Set("state", "closed")
		} else {
			params.Set("state", "all")
		}
	} else if len(states) == 0 || containsS(states, "OPEN") && containsS(states, "CLOSED") {
		params.Set("state", "all")
	} else if containsS(states, "OPEN") {
		params.Set("state", "open")
	} else if containsS(states, "CLOSED") {
		params.Set("state", "closed")
	}
	field := gqlString(order["field"])
	if field == "CREATED_AT" || field == "UPDATED_AT" {
		if field == "CREATED_AT" {
			params.Set("sort", "created")
		} else {
			params.Set("sort", "updated")
		}
		if direction != "asc" {
			direction = "desc"
		}
		params.Set("direction", direction)
	}
	items, e := gqlList(api, gqlRepoPath(owner, name)+"/issues?"+params.Encode())
	if e != nil {
		return nil, e
	}
	filtered := []any{}
	for _, raw := range items {
		x := obj(raw)
		if x["pull_request"] != nil {
			continue
		}
		if len(states) > 0 && !containsS(states, fmt.Sprint(gqlUpperState(x["state"]))) {
			continue
		}
		filtered = append(filtered, x)
	}
	if field == "CREATED_AT" || field == "UPDATED_AT" {
		key := "created_at"
		if field == "UPDATED_AT" {
			key = "updated_at"
		}
		sign := 1
		if direction != "asc" {
			sign = -1
		}
		sort.SliceStable(filtered, func(i, j int) bool {
			a, _ := time.Parse(time.RFC3339Nano, gqlString(obj(filtered[i])[key]))
			b, _ := time.Parse(time.RFC3339Nano, gqlString(obj(filtered[j])[key]))
			if a.Equal(b) {
				return sign*num(obj(filtered[i])["number"]) < sign*num(obj(filtered[j])["number"])
			}
			return int64(sign)*a.UnixNano() < int64(sign)*b.UnixNano()
		})
	}
	start := gqlCursor(args["after"])
	if start > len(filtered) {
		start = len(filtered)
	}
	end := start + first
	if end > len(filtered) {
		end = len(filtered)
	}
	nodes := append([]any{}, filtered[start:end]...)
	var cursor any
	if len(nodes) > 0 {
		cursor = gqlPageCursor(end)
	}
	return J{"nodes": nodes, "pageInfo": J{"hasNextPage": end < len(filtered), "endCursor": cursor}, "__owner": owner, "__repo": name}, nil
}
func containsS(a []string, s string) bool {
	for _, x := range a {
		if x == s {
			return true
		}
	}
	return false
}
func gqlGraphID(kind string, record J, extra J) string {
	p := J{"version": 1, "type": kind, "nodeId": record["node_id"], "databaseId": record["id"]}
	for k, v := range extra {
		p[k] = v
	}
	return gqlID(p)
}
func gqlFieldValue(t string, parent J, f gqlSelection, api *Forge, vars J, owner, repo string) (any, error) {
	a := J{}
	for k, v := range f.args {
		a[k] = gqlResolve(v, vars)
	}
	switch t {
	case "Query":
		switch f.name {
		case "viewer":
			return api.Viewer, nil
		case "repository":
			o, ok := a["owner"].(string)
			if !ok {
				return nil, gqlErr("repository requires owner and name")
			}
			n, ok := a["name"].(string)
			if !ok {
				return nil, gqlErr("repository requires owner and name")
			}
			v, e := gqlRequest(api, "GET", gqlRepoPath(o, n), nil, true)
			if v == nil {
				return nil, e
			}
			return v, e
		case "node":
			d := gqlDecodeID(a["id"])
			if d != nil {
				switch d["type"] {
				case "Repository":
					return gqlRepoByID(api, a["id"]), nil
				case "Issue":
					x, _, _, _ := gqlIssueByID(api, a["id"])
					return x, nil
				case "PullRequest":
					x, _, _, _ := gqlPullByID(api, a["id"])
					return x, nil
				}
			}
			if r := api.RepoByID(a["id"]); r != nil {
				return r, nil
			}
			return api.IssueByID(a["id"]), nil
		case "__type":
			return gqlIntrospection(a["name"]), nil
		}
	case "User":
		if f.name == "login" || f.name == "id" || f.name == "name" {
			return parent[f.name], nil
		}
	case "Ref":
		if f.name == "name" {
			return parent["name"], nil
		}
	case "Repository":
		o, n := gqlLocator(parent)
		switch f.name {
		case "id":
			return gqlGraphID("Repository", parent, J{"owner": o, "name": n}), nil
		case "databaseId":
			if parent["id"] != nil {
				return parent["id"], nil
			}
			return parent["databaseId"], nil
		case "name":
			return parent["name"], nil
		case "nameWithOwner":
			if parent["full_name"] != nil {
				return parent["full_name"], nil
			}
			return fmt.Sprintf("%s/%s", o, n), nil
		case "owner":
			if s, ok := parent["owner"].(string); ok {
				return J{"login": s}, nil
			}
			if parent["owner"] != nil {
				return parent["owner"], nil
			}
			return J{"login": o}, nil
		case "defaultBranchRef":
			branch := parent["default_branch"]
			if branch == nil {
				branch = parent["defaultBranch"]
			}
			if branch == nil {
				return nil, nil
			}
			return J{"name": branch}, nil
		case "viewerPermission":
			if parent["viewerPermission"] != nil {
				return parent["viewerPermission"], nil
			}
			return "ADMIN", nil
		case "hasIssuesEnabled":
			if parent["has_issues"] != nil {
				return parent["has_issues"], nil
			}
			if parent["hasIssuesEnabled"] != nil {
				return parent["hasIssuesEnabled"], nil
			}
			return true, nil
		case "issue":
			if _, ok := a["number"]; !ok {
				return nil, gqlErr("issue requires number")
			}
			numIssue := num(a["number"])
			return gqlRequest(api, "GET", fmt.Sprintf("%s/issues/%d", gqlRepoPath(o, n), numIssue), nil, true)
		case "issues":
			return gqlIssueConnection(api, o, n, a)
		case "pullRequests":
			first := 30
			if a["first"] != nil {
				first = num(a["first"])
			}
			if first < 0 {
				first = 0
			}
			if first > 100 {
				first = 100
			}
			items, e := gqlList(api, gqlRepoPath(o, n)+"/pulls?state=open&per_page="+strconv.Itoa(gqlMaxInt(first, 1))+"&page=1")
			if e != nil {
				return nil, e
			}
			if len(items) > first {
				items = items[:first]
			}
			return J{"nodes": items, "__owner": o, "__repo": n}, nil
		case "pullRequest":
			if a["number"] == nil {
				return nil, gqlErr("pullRequest requires number")
			}
			return gqlRequest(api, "GET", fmt.Sprintf("%s/pulls/%d", gqlRepoPath(o, n), num(a["number"])), nil, true)
		}
	case "Issue":
		switch f.name {
		case "id":
			return gqlGraphID("Issue", parent, J{"owner": owner, "repo": repo, "number": parent["number"]}), nil
		case "number", "title", "body":
			return parent[f.name], nil
		case "state":
			return gqlUpperState(parent["state"]), nil
		}
	case "IssueConnection":
		if f.name == "nodes" {
			return parent["nodes"], nil
		}
		if f.name == "pageInfo" {
			return parent["pageInfo"], nil
		}
	case "PageInfo":
		if f.name == "hasNextPage" {
			return sdef(parent["hasNextPage"], "false") == "true" || truth(parent["hasNextPage"]), nil
		}
		if f.name == "endCursor" {
			return parent["endCursor"], nil
		}
	case "PullRequestConnection":
		if f.name == "nodes" {
			return parent["nodes"], nil
		}
	case "PullRequest":
		switch f.name {
		case "id":
			return gqlGraphID("PullRequest", parent, J{"owner": owner, "repo": repo, "number": parent["number"]}), nil
		case "number":
			return parent["number"], nil
		case "state":
			if truth(parent["merged"]) {
				return "MERGED", nil
			}
			return gqlUpperState(parent["state"]), nil
		case "merged":
			return truth(parent["merged"]), nil
		case "headRefName":
			if parent["head"] != nil {
				return obj(parent["head"])["ref"], nil
			}
			return parent["headRefName"], nil
		case "baseRefName":
			if parent["base"] != nil {
				return obj(parent["base"])["ref"], nil
			}
			return parent["baseRefName"], nil
		case "mergeable":
			if parent["mergeable"] == true || parent["mergeable_state"] == "clean" || parent["mergeable_state"] == "has_hooks" {
				return "MERGEABLE", nil
			}
			if parent["mergeable"] == false || containsS([]string{"dirty", "behind", "blocked"}, strings.ToLower(gqlString(parent["mergeable_state"]))) {
				return "CONFLICTING", nil
			}
			return "UNKNOWN", nil
		case "files":
			o, r := owner, repo
			if o == "" {
				o = gqlString(parent["__owner"])
			}
			if r == "" {
				r = gqlString(parent["__repo"])
			}
			n := num(parent["number"])
			if o == "" || r == "" || n == 0 {
				return J{"nodes": []any{}}, nil
			}
			first := 30
			if a["first"] != nil {
				first = num(a["first"])
			}
			if first < 0 {
				first = 0
			}
			if first > 100 {
				first = 100
			}
			files, e := gqlList(api, fmt.Sprintf("%s/pulls/%d/files?per_page=%d&page=1", gqlRepoPath(o, r), n, gqlMaxInt(first, 1)))
			if e != nil {
				return nil, e
			}
			if len(files) > first {
				files = files[:first]
			}
			return J{"nodes": files}, nil
		}
	case "PullRequestFileConnection":
		if f.name == "nodes" {
			return parent["nodes"], nil
		}
	case "PullRequestChangedFile":
		if f.name == "path" {
			if parent["filename"] != nil {
				return parent["filename"], nil
			}
			return parent["path"], nil
		}
	case "__Type":
		if f.name == "name" {
			return parent["name"], nil
		}
		if f.name == "fields" || f.name == "enumValues" {
			return parent[f.name], nil
		}
	case "__Field", "__EnumValue":
		if f.name == "name" {
			return parent["name"], nil
		}
	case "CreateIssuePayload", "CloseIssuePayload":
		if f.name == "issue" {
			return parent["issue"], nil
		}
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		if f.name == "pullRequest" {
			return parent["pullRequest"], nil
		}
	case "AddCommentPayload":
		if f.name == "commentEdge" {
			return parent["commentEdge"], nil
		}
	case "CommentEdge":
		if f.name == "node" {
			return parent["node"], nil
		}
	case "Comment":
		if f.name == "body" {
			return parent["body"], nil
		}
		if f.name == "id" {
			return gqlGraphID("Comment", parent, J{}), nil
		}
	case "Mutation":
		return gqlMutation(api, f.name, a)
	}
	if f.name == "__typename" {
		return t, nil
	}
	return nil, gqlErr("Cannot query field %q on type %q.", f.name, t)
}
func gqlMaxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func gqlMutation(api *Forge, name string, a J) (any, error) {
	input := obj(a["input"])
	switch name {
	case "createIssue":
		repo := gqlRepoByID(api, input["repositoryId"])
		if repo == nil {
			return nil, gqlErr("createIssue could not find the repository")
		}
		o, n := gqlLocator(repo)
		body := J{"title": input["title"], "body": sdef(input["body"], "")}
		v, e := gqlRequest(api, "POST", gqlRepoPath(o, n)+"/issues", body, false)
		return J{"issue": v}, e
	case "closeIssue":
		issue, o, r, n := gqlIssueByID(api, input["issueId"])
		if issue == nil || o == "" || r == "" || n == 0 {
			return nil, gqlErr("closeIssue could not find the issue")
		}
		v, e := gqlRequest(api, "PATCH", fmt.Sprintf("%s/issues/%d", gqlRepoPath(o, r), n), J{"state": "closed"}, false)
		return J{"issue": v}, e
	case "createPullRequest":
		repo := gqlRepoByID(api, input["repositoryId"])
		if repo == nil {
			return nil, gqlErr("createPullRequest could not find the repository")
		}
		o, n := gqlLocator(repo)
		v, e := gqlRequest(api, "POST", gqlRepoPath(o, n)+"/pulls", J{"title": input["title"], "head": input["headRefName"], "base": input["baseRefName"]}, false)
		return J{"pullRequest": v}, e
	case "mergePullRequest":
		pr, o, r, n := gqlPullByID(api, input["pullRequestId"])
		if pr == nil || o == "" || r == "" || n == 0 {
			return nil, gqlErr("mergePullRequest could not find the pull request")
		}
		method := strings.ToLower(sdef(input["mergeMethod"], "merge"))
		res, e := gqlRequest(api, "PUT", fmt.Sprintf("%s/pulls/%d/merge", gqlRepoPath(o, r), n), J{"merge_method": method}, false)
		if e != nil {
			return nil, e
		}
		merged := truth(obj(res)["merged"])
		pr["merged"] = merged
		if merged {
			pr["state"] = "closed"
		}
		return J{"pullRequest": pr}, nil
	case "addComment":
		issue, o, r, n := gqlIssueByID(api, input["subjectId"])
		if issue == nil || o == "" || r == "" || n == 0 {
			return nil, gqlErr("addComment could not find the issue")
		}
		v, e := gqlRequest(api, "POST", fmt.Sprintf("%s/issues/%d/comments", gqlRepoPath(o, r), n), J{"body": input["body"]}, false)
		return J{"commentEdge": J{"node": v}}, e
	}
	return nil, gqlErr("Cannot query field %q on type %q.", name, "Mutation")
}
func gqlDirectiveInclude(ds []gqlDirective, vars J) (bool, error) {
	for _, d := range ds {
		v := gqlResolve(d.args["if"], vars)
		if d.name == "skip" && v == true {
			return false, nil
		}
		if d.name == "include" && v == false {
			return false, nil
		}
		if d.name != "skip" && d.name != "include" {
			return false, gqlErr("Unknown directive @%s.", d.name)
		}
	}
	return true, nil
}
func gqlRuntimeType(t string, v any) string {
	if t != "Node" {
		return t
	}
	m := obj(v)
	d := gqlDecodeID(m["id"])
	if d != nil && gqlString(d["type"]) != "" {
		return gqlString(d["type"])
	}
	if m["full_name"] != nil || m["nameWithOwner"] != nil {
		return "Repository"
	}
	if m["head"] != nil || m["headRefName"] != nil {
		return "PullRequest"
	}
	if m["number"] != nil {
		return "Issue"
	}
	return t
}
func gqlProject(value any, t string, selections []gqlSelection, api *Forge, vars J, fragments map[string]gqlFragment, owner, repo string, stack map[string]bool) (any, error) {
	if value == nil {
		return nil, nil
	}
	if a, ok := value.([]any); ok {
		out := make([]any, len(a))
		for i, x := range a {
			v, e := gqlProject(x, t, selections, api, vars, fragments, owner, repo, stack)
			if e != nil {
				return nil, e
			}
			out[i] = v
		}
		return out, nil
	}
	actual := gqlRuntimeType(t, value)
	parent := obj(value)
	result := J{}
	var visit func([]gqlSelection, string) error
	visit = func(ss []gqlSelection, typ string) error {
		for _, sel := range ss {
			include, e := gqlDirectiveInclude(sel.directives, vars)
			if e != nil {
				return e
			}
			if !include {
				continue
			}
			if sel.kind == "inline" {
				if sel.condition == "" || sel.condition == typ || sel.condition == "Node" {
					if e = visit(sel.selections, typ); e != nil {
						return e
					}
				}
				continue
			}
			if sel.kind == "spread" {
				frag, ok := fragments[sel.name]
				if !ok {
					return gqlErr("Unknown fragment %q.", sel.name)
				}
				if stack[sel.name] {
					return gqlErr("Cannot spread fragment %q within itself.", sel.name)
				}
				inc, e := gqlDirectiveInclude(frag.directives, vars)
				if e != nil {
					return e
				}
				if inc && (frag.condition == typ || frag.condition == "Node") {
					stack[sel.name] = true
					e = visit(frag.selections, typ)
					delete(stack, sel.name)
					if e != nil {
						return e
					}
				}
				continue
			}
			if sel.name == "__typename" {
				result[sel.alias] = typ
				continue
			}
			known := gqlAllowed(typ)
			if !containsS(known, sel.name) {
				return gqlErr("Cannot query field %q on type %q.", sel.name, typ)
			}
			childType := gqlChild(typ, sel.name)
			if childType != "" && len(sel.selections) == 0 {
				return gqlErr("Field %q of type %q must have a selection of subfields.", sel.name, childType)
			}
			if childType == "" && len(sel.selections) > 0 {
				return gqlErr("Field %q must not have a selection since type %q has no subfields.", sel.name, typ)
			}
			ctxOwner, ctxRepo := owner, repo
			if x := gqlString(parent["__owner"]); x != "" {
				ctxOwner = x
			}
			if x := gqlString(parent["__repo"]); x != "" {
				ctxRepo = x
			}
			child, e := gqlFieldValue(typ, parent, sel, api, vars, ctxOwner, ctxRepo)
			if e != nil {
				return e
			}
			if childType != "" {
				nextOwner, nextRepo := ctxOwner, ctxRepo
				if typ == "Repository" {
					nextOwner, nextRepo = gqlLocator(parent)
				}
				if typ == "Issue" || typ == "PullRequest" { // retain the repository provenance established by the enclosing connection
				}
				if childMap, ok := child.(map[string]any); ok {
					if x := gqlString(childMap["__owner"]); x != "" {
						nextOwner = x
					}
					if x := gqlString(childMap["__repo"]); x != "" {
						nextRepo = x
					}
					if typ == "Repository" {
						if childMap["owner"] != nil {
							if s := gqlString(childMap["owner"]); s != "" {
								nextOwner = s
							} else if s := gqlString(obj(childMap["owner"])["login"]); s != "" {
								nextOwner = s
							}
						}
						if s := gqlString(childMap["name"]); s != "" {
							nextRepo = s
						}
					}
				}
				child, e = gqlProject(child, childType, sel.selections, api, vars, fragments, nextOwner, nextRepo, stack)
				if e != nil {
					return e
				}
			}
			result[sel.alias] = child
		}
		return nil
	}
	if err := visit(selections, actual); err != nil {
		return nil, err
	}
	return result, nil
}
func gqlExecute(body any, api *Forge) any {
	req, ok := body.(map[string]any)
	if !ok {
		return J{"data": nil, "errors": []any{J{"message": "Request must contain a GraphQL query string."}}}
	}
	query, ok := req["query"].(string)
	if !ok {
		return J{"data": nil, "errors": []any{J{"message": "Request must contain a GraphQL query string."}}}
	}
	doc, e := parseGQL(query)
	if e != nil {
		return J{"data": nil, "errors": []any{J{"message": e.Error()}}}
	}
	opName := gqlString(req["operationName"])
	var op *gqlOperation
	if opName == "" {
		if len(doc.operations) != 1 {
			return J{"data": nil, "errors": []any{J{"message": "Must provide operationName when a document contains multiple operations."}}}
		}
		op = &doc.operations[0]
	} else {
		for i := range doc.operations {
			if doc.operations[i].name == opName {
				op = &doc.operations[i]
				break
			}
		}
	}
	if op == nil {
		return J{"data": nil, "errors": []any{J{"message": fmt.Sprintf("Unknown operation named %q.", opName)}}}
	}
	if op.kind == "subscription" {
		return J{"data": nil, "errors": []any{J{"message": "Subscriptions are not supported."}}}
	}
	provided := obj(req["variables"])
	vars := J{}
	for _, v := range op.variables {
		if x, ok := provided[v.name]; ok {
			vars[v.name] = x
		} else if v.hasDefault {
			vars[v.name] = gqlResolve(v.def, provided)
		} else if strings.HasSuffix(v.typ, "!") {
			return J{"data": nil, "errors": []any{J{"message": fmt.Sprintf("Variable $%s of required type %s was not provided.", v.name, v.typ)}}}
		}
	}
	for k, v := range provided {
		vars[k] = v
	}
	root := "Query"
	if op.kind == "mutation" {
		root = "Mutation"
	}
	data, e := gqlProject(J{}, root, op.selections, api, vars, doc.fragments, "", "", map[string]bool{})
	if e != nil {
		return J{"data": nil, "errors": []any{J{"message": e.Error()}}}
	}
	return J{"data": data}
}

// GraphQL executes a GitHub-shaped GraphQL request or ordered batch using the Forge REST API.
func GraphQL(body any, api *Forge, origin string) any {
	_ = origin
	if batch, ok := body.([]any); ok {
		out := make([]any, len(batch))
		for i, op := range batch {
			out[i] = gqlExecute(op, api)
		}
		return out
	}
	return gqlExecute(body, api)
}
