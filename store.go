package main

import (
	"encoding/base64"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

type Repo struct {
	Data       J
	Git        *GitStore
	Labels     map[string]J
	LabelOrder []string
	Issues     map[int]J
	IssueOrder []int
	Pulls      map[int]J
	PullOrder  []int
	Comments   map[int][]any
	Number     int
	issueMu    sync.RWMutex
	issueCache map[int][]byte
}

type Forge struct {
	Viewer    J
	Repos     map[string]*Repo
	RepoOrder []string
	IDs       map[string]J
	Serial    int
	Instant   time.Time
	Origin    string
}

func NewForge(login, timeText string) *Forge {
	instant, err := time.Parse(time.RFC3339Nano, timeText)
	if err != nil {
		instant = time.UnixMilli(946684800000).UTC()
	} else {
		instant = time.UnixMilli(instant.UnixMilli()).UTC()
	}
	return &Forge{
		Viewer: J{"login": login, "id": 1, "node_id": "U_1", "type": "User", "site_admin": false, "name": login},
		Repos:  make(map[string]*Repo), IDs: make(map[string]J), Serial: 1, Instant: instant, Origin: "http://localhost",
	}
}

func (f *Forge) nextID(prefix string) J {
	f.Serial++
	return J{"id": f.Serial, "node_id": fmt.Sprintf("%s_%d", prefix, f.Serial)}
}

func (f *Forge) stamp() string {
	return f.Instant.Add(time.Duration(f.Serial) * time.Second).UTC().Format("2006-01-02T15:04:05.000Z")
}

func (f *Forge) remember(data J) J {
	if id, ok := data["id"]; ok {
		f.IDs[jsString(id)] = data
		f.IDs[strconv.Itoa(num(id))] = data
	}
	if node, ok := data["node_id"].(string); ok {
		f.IDs[node] = data
	}
	return data
}

func (f *Forge) RepoByID(id any) J {
	data := f.IDs[idKey(id)]
	if data == nil || !truth(data["full_name"]) {
		return nil
	}
	name := str(data["name"])
	if repo := f.Repos[key(name)]; repo != nil && sameObject(repo.Data, data) {
		return data
	}
	return nil
}

func (f *Forge) IssueByID(id any) J {
	data := f.IDs[idKey(id)]
	if data == nil || !truth(data["number"]) || truth(data["head"]) {
		return nil
	}
	return data
}

func idKey(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		if !math.IsNaN(x) && !math.IsInf(x, 0) {
			return strconv.FormatFloat(x, 'f', -1, 64)
		}
	}
	return jsString(v)
}

func sameObject(a, b J) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || !sameValue(av, bv) {
			return false
		}
	}
	return true
}

func sameValue(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			return false
		}
		return sameObject(J(av), J(bv))
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !sameValue(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return fmt.Sprintf("%T:%v", a, a) == fmt.Sprintf("%T:%v", b, b)
	}
}

func key(s string) string { return strings.ToLower(s) }

func (f *Forge) Repo(owner, name string) *Repo {
	if key(owner) != key(str(f.Viewer["login"])) {
		return nil
	}
	return f.Repos[key(name)]
}

func (f *Forge) BranchTip(repo *Repo, name string) string {
	if repo == nil || repo.Git == nil {
		return ""
	}
	if sha, ok := repo.Git.Refs[refName(name)]; ok {
		return sha
	}
	if sha, ok := repo.Git.Refs[name]; ok {
		return sha
	}
	if sha, ok := repo.Git.Refs["refs/tags/"+name]; ok {
		return sha
	}
	if object := repo.Git.Read(name); object != nil && object.Type == "commit" {
		return name
	}
	return ""
}

func (f *Forge) RepoPath(repo *Repo) string {
	if repo == nil {
		return "/api/v3/repos"
	}
	owner := obj(repo.Data["owner"])
	return "/api/v3/repos/" + encodeURIComponent(str(owner["login"])) + "/" + encodeURIComponent(str(repo.Data["name"]))
}

func (f *Forge) Request(method string, u *url.URL, body any) Reply {
	if u == nil {
		return fail(400, "Bad Request")
	}
	urlCopy := *u
	if !urlCopy.IsAbs() {
		base, err := url.Parse(f.Origin)
		if err != nil {
			return fail(400, "Bad Request")
		}
		urlCopy = *base.ResolveReference(&urlCopy)
	}
	parts, ok := decodedPathParts(urlCopy.EscapedPath())
	if !ok {
		return fail(400, "Bad Request")
	}
	if len(parts) < 2 || parts[0] != "api" || parts[1] != "v3" {
		return missing()
	}
	var payload J
	if body != nil {
		var isObject bool
		payload, isObject = body.(map[string]any)
		if !isObject {
			return fail(400, "Expected a JSON object")
		}
		if payload == nil {
			payload = J{}
		}
	}
	is := func(n int, last string) bool { return len(parts) == n && parts[n-1] == last }

	if len(parts) == 2 && method == "GET" {
		return Reply{Status: 200, Body: J{"current_user_url": f.Origin + "/api/v3/user", "repository_url": f.Origin + "/api/v3/repos/{owner}/{repo}"}, Headers: map[string]string{"x-oauth-scopes": "repo, read:org"}}
	}
	if is(3, "meta") && method == "GET" {
		return out(200, J{"installed_version": "3.18.0"})
	}
	if is(3, "user") && method == "GET" {
		return out(200, f.Viewer)
	}
	if len(parts) == 4 && parts[1] == "v3" && parts[2] == "user" && parts[3] == "repos" {
		if method == "GET" {
			items := make([]any, 0, len(f.RepoOrder))
			for _, repoKey := range f.RepoOrder {
				if repo := f.Repos[repoKey]; repo != nil {
					items = append(items, repo.Data)
				}
			}
			return f.page(&urlCopy, items, 30)
		}
		if method == "POST" {
			name := str(payload["name"])
			if !validRepoName(name) || f.Repos[key(name)] != nil {
				return fail(422, "Repository name is invalid or already exists")
			}
			return f.createRepo(name, f.Viewer, payload)
		}
	}
	if len(parts) < 5 || parts[2] != "repos" {
		return missing()
	}
	repo := f.Repo(parts[3], parts[4])
	if repo == nil {
		return missing()
	}
	git := repo.Git
	defaultBranch := str(repo.Data["default_branch"])
	if len(parts) == 5 {
		switch method {
		case "GET":
			return out(200, repo.Data)
		case "DELETE":
			delete(f.Repos, key(parts[4]))
			f.RepoOrder = removeString(f.RepoOrder, key(parts[4]))
			repositoryPath := f.RepoPath(repo)
			for id, item := range f.IDs {
				if sameObject(item, repo.Data) || item["repository_url"] == repositoryPath {
					delete(f.IDs, id)
				}
			}
			return out(204, nil)
		case "PATCH":
			if value, ok := payload["description"].(string); ok {
				repo.Data["description"] = value
			}
			if value, ok := payload["has_issues"].(bool); ok {
				repo.Data["has_issues"] = value
			}
			return out(200, repo.Data)
		}
	}
	if len(parts) == 7 && parts[5] == "git" && parts[6] == "refs" && method == "POST" {
		name, sha := str(payload["ref"]), str(payload["sha"])
		object := git.Read(sha)
		if !strings.HasPrefix(name, "refs/heads/") || !validRefName(name) || object == nil || object.Type != "commit" || hasRef(git, name) {
			return fail(422, "Invalid reference")
		}
		git.SetRef(name, sha)
		return out(201, J{"ref": name, "object": J{"sha": sha, "type": "commit"}})
	}
	if is(6, "branches") && method == "GET" {
		items := make([]any, 0)
		for _, ref := range git.RefOrder {
			sha, ok := git.Refs[ref]
			if ok && strings.HasPrefix(ref, "refs/heads/") {
				items = append(items, J{"name": strings.TrimPrefix(ref, "refs/heads/"), "protected": false, "commit": J{"sha": sha}})
			}
		}
		return f.page(&urlCopy, items, 30)
	}
	if is(6, "commits") && method == "GET" {
		sha := f.BranchTip(repo, queryValue(&urlCopy, "sha", defaultBranch))
		if sha == "" {
			return fail(409, "Git Repository is empty")
		}
		history := git.History(sha)
		items := make([]any, 0, len(history))
		for _, commit := range history {
			items = append(items, J{"sha": commit.Sha, "commit": J{"message": commit.Message}})
		}
		return f.page(&urlCopy, items, 30)
	}
	if len(parts) >= 6 && parts[5] == "contents" {
		return f.contents(repo, strings.Join(parts[6:], "/"), method, &urlCopy, payload)
	}
	if is(6, "labels") && method == "GET" {
		items := make([]any, 0, len(repo.LabelOrder))
		for _, labelKey := range repo.LabelOrder {
			if label := repo.Labels[labelKey]; label != nil {
				items = append(items, label)
			}
		}
		return f.page(&urlCopy, items, 30)
	}
	if is(6, "labels") && method == "POST" {
		name := str(payload["name"])
		if name == "" || repo.Labels[key(name)] != nil {
			return fail(422, "Invalid or duplicate label")
		}
		id := f.nextID("LA")
		label := f.remember(J{"id": id["id"], "node_id": id["node_id"], "name": name, "color": sdef(payload["color"], "ededed"), "description": str(payload["description"]), "default": false})
		labelKey := key(name)
		repo.Labels[labelKey] = label
		repo.LabelOrder = append(repo.LabelOrder, labelKey)
		return out(201, label)
	}
	if len(parts) >= 6 && parts[5] == "issues" {
		return f.issues(repo, parts[6:], method, &urlCopy, payload)
	}
	if len(parts) >= 6 && parts[5] == "pulls" {
		return f.pulls(repo, parts[6:], method, &urlCopy, payload)
	}
	return missing()
}

func (f *Forge) createRepo(name string, owner J, body J) Reply {
	id := f.nextID("R")
	data := f.remember(J{
		"id": id["id"], "node_id": id["node_id"], "name": name, "full_name": str(owner["login"]) + "/" + name,
		"owner": owner, "private": body["private"] == true, "description": str(body["description"]), "default_branch": "main",
		"has_issues": true, "fork": false, "archived": false,
		"permissions": J{"admin": true, "maintain": true, "push": true, "triage": true, "pull": true}, "created_at": f.stamp(),
	})
	git := NewGitStore()
	if body["auto_init"] == true {
		sha := git.Write("blob", []byte("# "+name+"\n"))
		commit := git.Commit(map[string]GitFile{"README.md": {Sha: sha, Mode: "100644"}}, []string{}, "Initial commit")
		git.SetRef(refName("main"), commit)
	}
	repoKey := key(name)
	f.Repos[repoKey] = &Repo{Data: data, Git: git, Labels: make(map[string]J), Issues: make(map[int]J), Pulls: make(map[int]J), Comments: make(map[int][]any)}
	f.RepoOrder = append(f.RepoOrder, repoKey)
	return out(201, data)
}

func (f *Forge) page(u *url.URL, items []any, fallback int) Reply {
	size := numericQuery(u, "per_page", float64(fallback))
	if size == 0 || math.IsNaN(size) {
		size = float64(fallback)
	}
	size = math.Floor(size)
	if size < 1 {
		size = 1
	}
	if size > 100 {
		size = 100
	}
	page := math.Floor(numericQuery(u, "page", 1))
	if page < 1 || math.IsNaN(page) {
		page = 1
	}
	startF := (page - 1) * size
	if startF > float64(int(^uint(0)>>1)) {
		return out(200, []any{})
	}
	start := int(startF)
	end := start + int(size)
	if end > len(items) {
		end = len(items)
	}
	if start > len(items) {
		start = len(items)
	}
	values := append([]any{}, items[start:end]...)
	if startF+size >= float64(len(items)) {
		return out(200, values)
	}
	setURLSearchParam(u, "page", strconv.FormatFloat(page+1, 'f', 0, 64))
	return Reply{Status: 200, Body: values, Headers: map[string]string{"link": "<" + u.String() + ">; rel=\"next\""}}
}

func (f *Forge) contents(repo *Repo, path, method string, u *url.URL, body J) Reply {
	if path != "" && !validPath(path) {
		return fail(422, "Invalid path")
	}
	branch := str(body["branch"])
	if method == "GET" {
		branch = queryValue(u, "ref", str(repo.Data["default_branch"]))
	} else if _, ok := body["branch"]; !ok || body["branch"] == nil {
		branch = str(repo.Data["default_branch"])
	}
	sha := f.BranchTip(repo, branch)
	if sha == "" && (branch != str(repo.Data["default_branch"]) || method != "PUT") {
		return missing()
	}
	files := make(map[string]GitFile)
	if sha != "" {
		files = repo.Git.Files(sha)
	}
	old, hasOld := files[path]
	if method == "GET" {
		if hasOld {
			return out(200, f.file(repo, path, old))
		}
		prefix := ""
		if path != "" {
			prefix = path + "/"
		}
		listed := make(map[string]J)
		for name, file := range files {
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			remaining := strings.TrimPrefix(name, prefix)
			segments := strings.Split(remaining, "/")
			if len(segments) == 1 {
				listed[segments[0]] = f.file(repo, name, file)
			} else if _, ok := listed[segments[0]]; !ok {
				listed[segments[0]] = J{"type": "dir", "name": segments[0], "path": prefix + segments[0], "size": 0}
			}
		}
		if len(listed) == 0 {
			return missing()
		}
		names := make([]string, 0, len(listed))
		for name := range listed {
			names = append(names, name)
		}
		sortStrings(names)
		items := make([]any, 0, len(names))
		for _, name := range names {
			items = append(items, listed[name])
		}
		return out(200, items)
	}
	if (method != "PUT" && method != "DELETE") || path == "" || !validRefName(branch) || body == nil {
		return fail(422, "Invalid content request")
	}
	if method == "DELETE" && !hasOld {
		return missing()
	}
	bodySHA, hasSHA := body["sha"]
	if (hasOld || hasSHA) && (!hasSHA || bodySHA != old.Sha) {
		return fail(409, "SHA does not match current file")
	}
	if method == "PUT" {
		content, ok := body["content"].(string)
		if !ok {
			return fail(422, "Invalid base64 content")
		}
		content = removeWhitespace(content)
		decoded, valid := decodeBase64JS(content)
		if !valid {
			return fail(422, "Invalid base64 content")
		}
		for name := range files {
			if name != path && (strings.HasPrefix(name, path+"/") || strings.HasPrefix(path, name+"/")) {
				return fail(409, "File and directory conflict")
			}
		}
		blob := repo.Git.Write("blob", decoded)
		mode := "100644"
		if hasOld {
			mode = old.Mode
		}
		files[path] = GitFile{Sha: blob, Mode: mode}
	} else {
		delete(files, path)
	}
	message := str(body["message"])
	if _, ok := body["message"].(string); !ok {
		if method == "DELETE" {
			message = "Delete " + path
		} else {
			message = "Update " + path
		}
	}
	parents := []string{}
	if sha != "" {
		parents = append(parents, sha)
	}
	commit := repo.Git.Commit(files, parents, message)
	repo.Git.SetRef(refName(branch), commit)
	if method == "DELETE" {
		return out(200, J{"content": nil, "commit": J{"sha": commit}})
	}
	status := 201
	if hasOld {
		status = 200
	}
	return out(status, J{"content": f.file(repo, path, files[path]), "commit": J{"sha": commit}})
}

func (f *Forge) file(repo *Repo, path string, file GitFile) J {
	var data []byte
	if object := repo.Git.Read(file.Sha); object != nil {
		data = object.Data
	}
	nameParts := strings.Split(path, "/")
	return J{"type": "file", "name": nameParts[len(nameParts)-1], "path": path, "sha": file.Sha, "mode": file.Mode,
		"size": len(data), "encoding": "base64", "content": base64.StdEncoding.EncodeToString(data)}
}

func (f *Forge) issues(repo *Repo, path []string, method string, u *url.URL, body J) Reply {
	if len(path) == 0 {
		if method == "GET" {
			state := queryValue(u, "state", "open")
			ascending := queryValue(u, "direction", "") == "asc"
			items := make([]any, 0, len(repo.IssueOrder))
			for _, number := range repo.IssueOrder {
				if issue := repo.Issues[number]; issue != nil && (state == "all" || state == str(issue["state"])) {
					items = append(items, issue)
				}
			}
			if !ascending {
				reverseAny(items)
			}
			return f.page(u, items, 30)
		}
		if method == "POST" {
			title, ok := body["title"].(string)
			if !ok || strings.TrimSpace(title) == "" {
				return fail(422, "Title is required")
			}
			labels := make([]any, 0)
			for _, labelValue := range arr(body["labels"]) {
				labelName := str(labelValue)
				if _, isString := labelValue.(string); !isString {
					labelName = str(obj(labelValue)["name"])
				}
				label := repo.Labels[key(labelName)]
				if label == nil {
					return fail(422, "Unknown label")
				}
				labels = append(labels, label)
			}
			id := f.nextID("I")
			stamp, repositoryURL := f.stamp(), f.RepoPath(repo)
			repo.Number++
			number := repo.Number
			item := f.remember(J{
				"id": id["id"], "node_id": id["node_id"], "number": number, "title": title, "body": str(body["body"]), "state": "open", "labels": labels,
				"comments": 0, "user": f.Viewer, "created_at": stamp, "updated_at": stamp, "closed_at": nil,
				"repository_url": repositoryURL, "url": fmt.Sprintf("%s/issues/%d", repositoryURL, number),
			})
			repo.Issues[number] = item
			delete(repo.issueCache, 0)
			repo.IssueOrder = append(repo.IssueOrder, number)
			return out(201, item)
		}
	}
	number, validNumber := issueNumber(path[0])
	issue := repo.Issues[number]
	if !validNumber || issue == nil {
		return missing()
	}
	if len(path) == 1 {
		if method == "GET" {
			return out(200, issue)
		}
		if method == "PATCH" {
			if value, ok := body["title"].(string); ok {
				issue["title"] = value
			}
			if value, ok := body["body"].(string); ok {
				issue["body"] = value
			}
			if state := str(body["state"]); state == "closed" || state == "open" {
				issue["state"] = state
				if state == "closed" {
					issue["closed_at"] = f.stamp()
				} else {
					issue["closed_at"] = nil
				}
			}
			issue["updated_at"] = f.stamp()
			delete(repo.issueCache, number)
			delete(repo.issueCache, 0)
			return out(200, issue)
		}
	}
	if len(path) == 2 && path[1] == "comments" {
		comments := repo.Comments[number]
		if comments == nil {
			comments = []any{}
		}
		if method == "GET" {
			return f.page(u, comments, 30)
		}
		if method == "POST" {
			text, ok := body["body"].(string)
			if !ok || strings.TrimSpace(text) == "" {
				return fail(422, "Comment body is required")
			}
			id := f.nextID("IC")
			created := f.stamp()
			item := J{"id": id["id"], "node_id": id["node_id"], "body": text, "user": f.Viewer, "created_at": created, "updated_at": created}
			comments = append(comments, item)
			repo.Comments[number] = comments
			issue["comments"] = len(comments)
			delete(repo.issueCache, number)
			delete(repo.issueCache, 0)
			return out(201, item)
		}
	}
	return missing()
}

func (f *Forge) diff(repo *Repo, base, head string) (changes []fileChange, current map[string]GitFile, conflict bool) {
	baseHistory := repo.Git.History(base)
	reachable := make(map[string]bool)
	for _, commit := range repo.Git.History(head) {
		reachable[commit.Sha] = true
	}
	ancestor := ""
	for _, commit := range baseHistory {
		if reachable[commit.Sha] {
			ancestor = commit.Sha
			break
		}
	}
	before := make(map[string]GitFile)
	if ancestor != "" {
		before = repo.Git.Files(ancestor)
	}
	after := repo.Git.Files(head)
	current = repo.Git.Files(base)
	pathsMap := make(map[string]bool)
	for path := range before {
		pathsMap[path] = true
	}
	for path := range after {
		pathsMap[path] = true
	}
	paths := make([]string, 0, len(pathsMap))
	for path := range pathsMap {
		paths = append(paths, path)
	}
	sortStrings(paths)
	for _, path := range paths {
		old, oldOK := before[path]
		target, targetOK := after[path]
		if sameFile(old, oldOK, target, targetOK) {
			continue
		}
		currentFile, currentOK := current[path]
		changes = append(changes, fileChange{Path: path, Old: old, HasOld: oldOK, Target: target, HasTarget: targetOK, Current: currentFile, HasCurrent: currentOK})
		if !sameFile(old, oldOK, currentFile, currentOK) && !sameFile(target, targetOK, currentFile, currentOK) {
			conflict = true
		}
	}
	return
}

type fileChange struct {
	Path       string
	Old        GitFile
	HasOld     bool
	Target     GitFile
	HasTarget  bool
	Current    GitFile
	HasCurrent bool
}

func (f *Forge) pulls(repo *Repo, path []string, method string, u *url.URL, body J) Reply {
	if len(path) == 0 {
		if method == "GET" {
			state := queryValue(u, "state", "open")
			items := make([]any, 0, len(repo.PullOrder))
			for _, number := range repo.PullOrder {
				if pull := repo.Pulls[number]; pull != nil && (state == "all" || state == str(pull["state"])) {
					items = append(items, pull)
				}
			}
			reverseAny(items)
			return f.page(u, items, 30)
		}
		if method == "POST" {
			rawHead, base := str(body["head"]), str(body["base"])
			head := rawHead
			if colon := strings.Index(head, ":"); colon >= 0 {
				head = head[colon+1:]
			}
			headSHA, headOK := repo.Git.Refs[refName(head)]
			baseSHA, baseOK := repo.Git.Refs[refName(base)]
			title := str(body["title"])
			if !headOK || !baseOK || head == base || strings.TrimSpace(title) == "" {
				return fail(422, "Invalid head, base or title")
			}
			id := f.nextID("PR")
			stamp, repositoryURL := f.stamp(), f.RepoPath(repo)
			repo.Number++
			number := repo.Number
			side := func(name, sha string) J {
				return J{"ref": name, "sha": sha, "label": str(f.Viewer["login"]) + ":" + name, "repo": repo.Data, "user": f.Viewer}
			}
			_, _, conflict := f.diff(repo, baseSHA, headSHA)
			item := f.remember(J{
				"id": id["id"], "node_id": id["node_id"], "number": number, "title": title, "body": str(body["body"]),
				"head": side(head, headSHA), "base": side(base, baseSHA), "state": "open", "merged": false, "mergeable": !conflict,
				"merge_commit_sha": nil, "user": f.Viewer, "created_at": stamp, "updated_at": stamp, "closed_at": nil, "merged_at": nil,
				"repository_url": repositoryURL, "url": fmt.Sprintf("%s/pulls/%d", repositoryURL, number),
			})
			repo.Pulls[number] = item
			repo.PullOrder = append(repo.PullOrder, number)
			return out(201, item)
		}
	}
	number, validNumber := issueNumber(firstPath(path))
	pull := repo.Pulls[number]
	if !validNumber || pull == nil {
		return missing()
	}
	if len(path) == 1 && method == "GET" {
		return out(200, pull)
	}
	if len(path) == 2 && path[1] == "files" && method == "GET" {
		baseSide, headSide := obj(pull["base"]), obj(pull["head"])
		changes, _, _ := f.diff(repo, str(baseSide["sha"]), str(headSide["sha"]))
		items := make([]any, 0, len(changes))
		for _, change := range changes {
			a := gitLines(repo.Git, change.Old, change.HasOld)
			b := gitLines(repo.Git, change.Target, change.HasTarget)
			first, last := 0, 0
			for first < len(a) && first < len(b) && a[first] == b[first] {
				first++
			}
			for last < len(a)-first && last < len(b)-first && a[len(a)-last-1] == b[len(b)-last-1] {
				last++
			}
			deletions, additions := len(a)-first-last, len(b)-first-last
			sha := change.Old.Sha
			if change.HasTarget {
				sha = change.Target.Sha
			}
			status := "modified"
			if !change.HasOld {
				status = "added"
			} else if !change.HasTarget {
				status = "removed"
			}
			items = append(items, J{"sha": sha, "filename": change.Path, "status": status, "additions": additions, "deletions": deletions, "changes": additions + deletions})
		}
		return f.page(u, items, 30)
	}
	if len(path) == 2 && path[1] == "merge" && method == "PUT" {
		if str(pull["state"]) != "open" {
			return fail(405, "Pull request is closed")
		}
		baseSide, headSide := obj(pull["base"]), obj(pull["head"])
		base, baseOK := repo.Git.Refs[refName(str(baseSide["ref"]))]
		head, headOK := repo.Git.Refs[refName(str(headSide["ref"]))]
		if !baseOK || !headOK {
			return fail(405, "Branch missing")
		}
		if requested, ok := body["sha"]; ok && requested != head {
			return fail(409, "Head changed")
		}
		changes, current, conflict := f.diff(repo, base, head)
		if conflict {
			pull["mergeable"] = false
			return fail(405, "Pull request is not mergeable")
		}
		for _, change := range changes {
			if change.HasTarget {
				current[change.Path] = change.Target
			} else {
				delete(current, change.Path)
			}
		}
		parents := []string{base}
		if body["merge_method"] == "merge" {
			parents = append(parents, head)
		}
		title := sdef(body["commit_title"], str(pull["title"])+" (#"+strconv.Itoa(num(pull["number"]))+")")
		sha := repo.Git.Commit(current, parents, title)
		repo.Git.SetRef(refName(str(baseSide["ref"])), sha)
		pull["state"], pull["merged"], pull["mergeable"], pull["merge_commit_sha"] = "closed", true, true, sha
		pull["closed_at"], pull["merged_at"] = f.stamp(), f.stamp()
		return out(200, J{"sha": sha, "merged": true, "message": "Pull Request successfully merged"})
	}
	return missing()
}

func decodedPathParts(escaped string) ([]string, bool) {
	if escaped == "" {
		escaped = "/"
	}
	parts := strings.Split(strings.TrimPrefix(escaped, "/"), "/")
	for i := range parts {
		decoded, err := url.PathUnescape(parts[i])
		if err != nil {
			return nil, false
		}
		parts[i] = decoded
	}
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts, true
}

func validRepoName(name string) bool {
	if name == "" || len(name) > 100 || name == "." || name == ".." {
		return false
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '.' || c == '-') {
			return false
		}
	}
	return true
}

func validRefName(name string) bool {
	if name == "" || strings.Contains(name, "..") || strings.Contains(name, "@{") {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c <= 0x20 || c == 0x7f || strings.ContainsRune(" ~^:?*\\", rune(c)) {
			return false
		}
	}
	for _, component := range strings.Split(name, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}
	return true
}

func validPath(path string) bool {
	if path == "" || strings.ContainsRune(path, 0) {
		return false
	}
	for _, component := range strings.Split(path, "/") {
		if component == "" || component == "." || component == ".." || key(component) == ".git" {
			return false
		}
	}
	return true
}

func refName(branch string) string { return "refs/heads/" + branch }

func hasRef(git *GitStore, name string) bool {
	_, ok := git.Refs[name]
	return ok
}

func removeString(values []string, target string) []string {
	for i, value := range values {
		if value == target {
			return append(values[:i], values[i+1:]...)
		}
	}
	return values
}

func removeInt(values []int, target int) []int {
	for i, value := range values {
		if value == target {
			return append(values[:i], values[i+1:]...)
		}
	}
	return values
}

func numericQuery(u *url.URL, name string, fallback float64) float64 {
	if u == nil {
		return fallback
	}
	values, ok := u.Query()[name]
	if !ok || len(values) == 0 {
		return fallback
	}
	text := strings.TrimSpace(values[0])
	if text == "" {
		return 0
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return math.NaN()
	}
	return value
}

func queryValue(u *url.URL, name, fallback string) string {
	if u == nil {
		return fallback
	}
	values, ok := u.Query()[name]
	if !ok || len(values) == 0 {
		return fallback
	}
	return values[0]
}

func setURLSearchParam(u *url.URL, key, value string) {
	parts := strings.Split(u.RawQuery, "&")
	out := make([]string, 0, len(parts)+1)
	found := false
	for _, part := range parts {
		if part == "" {
			continue
		}
		k, _, _ := strings.Cut(part, "=")
		decoded, err := url.QueryUnescape(k)
		if err == nil && decoded == key {
			if !found {
				out = append(out, url.QueryEscape(key)+"="+url.QueryEscape(value))
				found = true
			}
			continue
		}
		out = append(out, part)
	}
	if !found {
		out = append(out, url.QueryEscape(key)+"="+url.QueryEscape(value))
	}
	u.RawQuery = strings.Join(out, "&")
}

func reverseAny(items []any) {
	for left, right := 0, len(items)-1; left < right; left, right = left+1, right-1 {
		items[left], items[right] = items[right], items[left]
	}
}

func issueNumber(text string) (int, bool) {
	if strings.TrimSpace(text) == "" {
		return 0, true
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value != math.Trunc(value) || value > float64(math.MaxInt) || value < float64(math.MinInt) {
		return 0, false
	}
	return int(value), true
}

func firstPath(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}

func sameFile(a GitFile, aOK bool, b GitFile, bOK bool) bool {
	if !aOK || !bOK {
		return !aOK && !bOK
	}
	return a.Sha == b.Sha && a.Mode == b.Mode
}

func gitLines(git *GitStore, file GitFile, exists bool) []string {
	if !exists {
		return []string{}
	}
	object := git.Read(file.Sha)
	if object == nil || len(object.Data) == 0 {
		return []string{}
	}
	for _, b := range object.Data {
		if b == 0 {
			return []string{}
		}
	}
	text := string(object.Data)
	if strings.HasSuffix(text, "\n") {
		text = strings.TrimSuffix(text, "\n")
	}
	return strings.Split(text, "\n")
}

func sortStrings(values []string) {
	// Small local insertion sort keeps the file self-contained and avoids additional state.
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func removeWhitespace(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, value)
}

func decodeBase64JS(value string) ([]byte, bool) {
	validChars := func(c byte) bool {
		return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/'
	}
	if len(value)%4 == 1 {
		return nil, false
	}
	padding := strings.Count(value, "=")
	if padding > 2 || strings.Contains(value[:storeMaxInt(0, len(value)-padding)], "=") {
		return nil, false
	}
	for i := 0; i < len(value)-padding; i++ {
		if !validChars(value[i]) {
			return nil, false
		}
	}
	for i := len(value) - padding; i < len(value); i++ {
		if i >= 0 && value[i] != '=' {
			return nil, false
		}
	}
	if padding == 1 && len(value)%4 != 0 || padding == 2 && len(value)%4 != 0 {
		return nil, false
	}
	if padding == 1 && len(value) < 4 || padding == 2 && len(value) < 4 {
		return nil, false
	}
	encoding := base64.RawStdEncoding
	if padding > 0 {
		encoding = base64.StdEncoding
	}
	decoded, err := encoding.DecodeString(value)
	if err != nil {
		return nil, false
	}
	return decoded, true
}

func storeMaxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func encodeURIComponent(value string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.!~*'()", rune(c)) {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		}
	}
	return b.String()
}
