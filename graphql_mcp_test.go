package main

import (
	"net/url"
	"strings"
	"testing"
)

func TestGraphQLParserFragmentsVariablesMutationAndPaging(t *testing.T) {
	api := NewForge("tester", "2026-01-01T00:00:00Z")
	gql := func(query, operation string, vars J) J {
		t.Helper()
		got := obj(GraphQL(J{"query": query, "operationName": operation, "variables": vars}, api, ""))
		if got["errors"] != nil {
			t.Fatalf("GraphQL error: %s", js(got))
		}
		return obj(got["data"])
	}
	viewer := gql(`query A($show: Boolean = true) { who: viewer { ...U } } query B { viewer { bad } } fragment U on User { login @include(if: $show) }`, "A", nil)
	if obj(viewer["who"])["login"] != "tester" {
		t.Fatalf("fragment/variable projection: %s", js(viewer))
	}
	created := api.Request("POST", mustGQLURL("/api/v3/user/repos"), J{"name": "port-test"})
	if created.Status != 201 {
		t.Fatalf("create repository status=%d body=%s", created.Status, js(created.Body))
	}
	repo := obj(gql(`query Q { repository(owner:"tester",name:"port-test") { id } }`, "Q", nil)["repository"])
	rid := str(repo["id"])
	if !strings.HasPrefix(rid, "gql1_") {
		t.Fatalf("repository global id = %q", rid)
	}
	for _, title := range []string{"one", "two"} {
		gql(`mutation Q($repo: ID!, $title: String!) { createIssue(input:{repositoryId:$repo,title:$title}) { issue { id title } } }`, "Q", J{"repo": rid, "title": title})
	}
	query := `query Q($after:String) { repository(owner:"tester",name:"port-test") { issues(first:1,after:$after,orderBy:{field:CREATED_AT,direction:DESC}) { nodes { id title } pageInfo { hasNextPage endCursor } } } }`
	page := func(after any) J { return obj(obj(gql(query, "Q", J{"after": after})["repository"])["issues"]) }
	p1 := page(nil)
	p2 := page(obj(p1["pageInfo"])["endCursor"])
	if obj(p1["pageInfo"])["hasNextPage"] != true || obj(p2["pageInfo"])["hasNextPage"] != false || obj(arr(p1["nodes"])[0])["title"] != "two" || obj(arr(p2["nodes"])[0])["title"] != "one" {
		t.Fatalf("issue pages: %s %s", js(p1), js(p2))
	}
	issueID := obj(arr(p1["nodes"])[0])["id"]
	closed := gql(`mutation Q($id:ID!) { closeIssue(input:{issueId:$id}) { issue { state } } }`, "Q", J{"id": issueID})
	if obj(obj(closed["closeIssue"])["issue"])["state"] != "CLOSED" {
		t.Fatalf("close mutation: %s", js(closed))
	}
	comment := gql(`mutation Q($id:ID!) { addComment(input:{subjectId:$id,body:"comment"}) { commentEdge { node { body id } } } }`, "Q", J{"id": issueID})
	if obj(obj(obj(comment["addComment"])["commentEdge"])["node"])["body"] != "comment" {
		t.Fatalf("comment mutation: %s", js(comment))
	}
}

func TestGraphQLValidationIntrospectionAndBatch(t *testing.T) {
	api := NewForge("tester", "2026-01-01T00:00:00Z")
	bad := obj(GraphQL(J{"query": `{ viewer { absent } }`}, api, ""))
	if bad["data"] != nil || len(arr(bad["errors"])) != 1 {
		t.Fatalf("invalid-field response: %s", js(bad))
	}
	viewerID := obj(obj(GraphQL(J{"query": `{ viewer { id } }`}, api, ""))["data"])["viewer"]
	if obj(viewerID)["id"] != api.Viewer["id"] {
		t.Fatalf("User.id should resolve: %s", js(viewerID))
	}
	repositoryDescription := obj(GraphQL(J{"query": `{ repository(owner:"missing",name:"missing") { description } }`}, api, ""))
	// lite-v2 validates fields while projecting non-null values. A missing
	// repository stays null even if its unused selection names an unknown field.
	if len(arr(repositoryDescription["errors"])) != 0 || obj(repositoryDescription["data"])["repository"] != nil {
		t.Fatalf("missing Repository must preserve lite-v2's null projection: %s", js(repositoryDescription))
	}
	introspected := obj(obj(GraphQL(J{"query": `{ __type(name:"Issue") { name fields(includeDeprecated:true) { name } } }`}, api, ""))["data"])
	fields := arr(obj(introspected["__type"])["fields"])
	if len(fields) < 4 {
		t.Fatalf("introspection fields: %s", js(introspected))
	}
	batch := arr(GraphQL([]any{J{"query": `{ viewer { login } }`}, J{"query": `{ viewer { login } }`}}, api, ""))
	if len(batch) != 2 || obj(obj(batch[0])["data"])["viewer"] == nil || obj(obj(batch[1])["data"])["viewer"] == nil {
		t.Fatalf("batch: %s", js(batch))
	}
}

func TestMCPToolsAndCalls(t *testing.T) {
	api := NewForge("tester", "2026-01-01T00:00:00Z")
	call := func(method string, params J) Reply {
		return MCP(J{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}, api)
	}
	listed := call("tools/list", J{})
	if listed.Status != 200 || len(arr(obj(obj(listed.Body)["result"])["tools"])) != 6 {
		t.Fatalf("tools/list: %s", js(listed.Body))
	}
	made := call("tools/call", J{"name": "create_repo", "arguments": J{"name": "mcp-repo"}})
	if made.Status != 200 || obj(obj(obj(made.Body)["result"])["structuredContent"])["name"] != "mcp-repo" {
		t.Fatalf("create_repo: %s", js(made.Body))
	}
	issue := call("tools/call", J{"name": "create_issue", "arguments": J{"owner": "tester", "repo": "mcp-repo", "title": "bug"}})
	if obj(obj(obj(issue.Body)["result"])["structuredContent"])["number"] != 1 {
		t.Fatalf("create_issue: %s", js(issue.Body))
	}
	invalid := call("tools/call", J{"name": "get_repo", "arguments": J{"owner": "..", "repo": "mcp-repo"}})
	if obj(invalid.Body)["error"] == nil {
		t.Fatalf("invalid tool args: %s", js(invalid.Body))
	}
	unknown := call("tools/call", J{"name": "missing", "arguments": J{}})
	if obj(unknown.Body)["error"] == nil {
		t.Fatalf("unknown tool: %s", js(unknown.Body))
	}
	if notification := MCP(J{"jsonrpc": "2.0", "method": "tools/call", "params": J{"name": "create_repo", "arguments": J{"name": "no-side-effect"}}}, api); notification.Status != 202 || api.Repo("tester", "no-side-effect") != nil {
		t.Fatalf("notification side effect: %#v", notification)
	}
}

func mustGQLURL(path string) *url.URL { u, _ := url.Parse(path); return u }
