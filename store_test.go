package main

import (
	"net/url"
	"testing"
	"time"
)

func requestURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestForgeRepoLifecycleAndOrderedPagination(t *testing.T) {
	api := NewForge("octo", "2026-01-02T03:04:05Z")
	created := api.Request("POST", requestURL(t, "/api/v3/user/repos"), J{"name": "Demo", "auto_init": true})
	if created.Status != 201 || obj(created.Body)["id"] != 2 || obj(created.Body)["node_id"] != "R_2" || obj(created.Body)["created_at"] != "2026-01-02T03:04:07.000Z" {
		t.Fatalf("unexpected repo creation metadata: %#v", created)
	}
	repo := api.Repo("OCTO", "demo")
	if repo == nil || repo.Data["full_name"] != "octo/Demo" || len(repo.IssueOrder) != 0 {
		t.Fatalf("unexpected repo state: %#v", repo)
	}
	if repo.Git.Refs["refs/heads/main"] != "f9fe4c554c5f134c7c0084b385c388a7aee7a883" {
		t.Fatalf("auto-init commit hash = %q", repo.Git.Refs["refs/heads/main"])
	}
	branches := api.Request("GET", requestURL(t, "/api/v3/repos/octo/demo/branches"), nil)
	got := arr(branches.Body)
	if len(got) != 1 || obj(got[0])["name"] != "main" {
		t.Fatalf("branches = %#v, want main", branches.Body)
	}

	for _, title := range []string{"one", "two", "three"} {
		r := api.Request("POST", requestURL(t, "/api/v3/repos/octo/demo/issues"), J{"title": title})
		if r.Status != 201 {
			t.Fatalf("create issue status = %d: %#v", r.Status, r.Body)
		}
	}
	page := api.Request("GET", requestURL(t, "/api/v3/repos/octo/demo/issues?per_page=2"), nil)
	items := arr(page.Body)
	if len(items) != 2 || obj(items[0])["title"] != "three" || obj(items[1])["title"] != "two" {
		t.Fatalf("default issue order = %#v, want three then two", page.Body)
	}
	if page.Headers["link"] != "<http://localhost/api/v3/repos/octo/demo/issues?per_page=2&page=2>; rel=\"next\"" {
		t.Fatalf("pagination link = %q", page.Headers["link"])
	}
}

func TestForgeIssueValidationMutationAndStableDates(t *testing.T) {
	api := NewForge("octo", "2026-01-02T03:04:05Z")
	if r := api.Request("POST", requestURL(t, "/api/v3/user/repos"), J{"name": "repo"}); r.Status != 201 {
		t.Fatalf("create repo status = %d", r.Status)
	}
	base := "/api/v3/repos/octo/repo/issues"
	if r := api.Request("POST", requestURL(t, base), J{"title": "  \n"}); r.Status != 422 {
		t.Fatalf("blank title status = %d, want 422", r.Status)
	}
	created := api.Request("POST", requestURL(t, base), J{"title": "bug", "body": "details"})
	issue := obj(created.Body)
	if created.Status != 201 || issue["number"] != 1 {
		t.Fatalf("create issue = %#v", created)
	}
	stamp, ok := issue["created_at"].(string)
	if !ok || stamp != "2026-01-02T03:04:08.000Z" {
		t.Fatalf("created_at = %#v", issue["created_at"])
	}
	comment := api.Request("POST", requestURL(t, base+"/1/comments"), J{"body": "  "})
	if comment.Status != 422 {
		t.Fatalf("blank comment status = %d, want 422", comment.Status)
	}
	comment = api.Request("POST", requestURL(t, base+"/1/comments"), J{"body": "note"})
	if comment.Status != 201 || issue["comments"] != 1 {
		t.Fatalf("comment response/state = %#v / %#v", comment, issue)
	}
	updated := api.Request("PATCH", requestURL(t, base+"/1"), J{"state": "closed"})
	if updated.Status != 200 || obj(updated.Body)["closed_at"] != "2026-01-02T03:04:09.000Z" {
		t.Fatalf("issue update = %#v", updated)
	}
}

func TestForgeContentAndMergeDeterministicHashes(t *testing.T) {
	api := NewForge("octo", "2026-01-02T03:04:05Z")
	create := api.Request("POST", requestURL(t, "/api/v3/user/repos"), J{"name": "repo", "auto_init": true})
	if create.Status != 201 {
		t.Fatalf("create repo status = %d", create.Status)
	}
	repo := api.Repo("octo", "repo")
	main := repo.Git.Refs["refs/heads/main"]
	ref := api.Request("POST", requestURL(t, "/api/v3/repos/octo/repo/git/refs"), J{"ref": "refs/heads/topic", "sha": main})
	if ref.Status != 201 {
		t.Fatalf("create branch = %#v", ref)
	}
	content := api.Request("PUT", requestURL(t, "/api/v3/repos/octo/repo/contents/a%2Fb"), J{"message": "add file", "content": "aGVsbG8K", "branch": "topic"})
	if content.Status != 201 || obj(content.Body)["content"] == nil {
		t.Fatalf("create content = %#v", content)
	}
	if obj(obj(content.Body)["content"])["path"] != "a/b" || repo.Git.Refs["refs/heads/topic"] != "5b959b98c9ec0c8fe666d44dd58a88fb86f9a235" {
		t.Fatalf("encoded path or topic commit changed: %#v, %s", content.Body, repo.Git.Refs["refs/heads/topic"])
	}
	pull := api.Request("POST", requestURL(t, "/api/v3/repos/octo/repo/pulls"), J{"title": "merge", "head": "topic", "base": "main"})
	if pull.Status != 201 || obj(pull.Body)["mergeable"] != true {
		t.Fatalf("create pull = %#v", pull)
	}
	merged := api.Request("PUT", requestURL(t, "/api/v3/repos/octo/repo/pulls/1/merge"), J{"merge_method": "merge"})
	if merged.Status != 200 || obj(merged.Body)["sha"] != "1c4be1ebad75f146d33e71a0ce388301370a521d" || repo.Git.Refs["refs/heads/main"] != obj(merged.Body)["sha"] {
		t.Fatalf("merge result = %#v", merged)
	}
}

func TestForgeDateFallbackAndIDs(t *testing.T) {
	api := NewForge("octo", "not a date")
	if !api.Instant.Equal(time.Unix(946684800, 0).UTC()) {
		t.Fatalf("fallback instant = %v", api.Instant)
	}
	r := api.Request("POST", requestURL(t, "/api/v3/user/repos"), J{"name": "repo"})
	repo := obj(r.Body)
	if api.RepoByID(repo["id"]) == nil || api.RepoByID(repo["node_id"]) == nil {
		t.Fatalf("repo ID lookup failed for %#v", repo)
	}
}
