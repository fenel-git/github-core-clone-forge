package main

import (
	"bytes"
	"fmt"
	"testing"
)

func TestGraphQLMutationReplayBytes(t *testing.T) {
	run := func() [][]byte {
		f := NewForge("replay-user", "2026-02-03T04:05:06Z")
		f.Request("POST", mustGQLURL("/api/v3/user/repos"), J{"name": "r"})
		call := func(q string, v J) J {
			r := obj(GraphQL(J{"query": q, "variables": v}, f, ""))
			if r["errors"] != nil {
				t.Fatalf("GraphQL failed: %s", js(r))
			}
			return obj(r["data"])
		}
		repository := obj(call(`{repository(owner:"replay-user",name:"r"){id}}`, nil)["repository"])
		var outputs [][]byte
		var ids []any
		for i := 0; i < 20; i++ {
			d := call(`mutation($r:ID!,$t:String!){createIssue(input:{repositoryId:$r,title:$t}){issue{id number title state}}}`, J{"r": repository["id"], "t": fmt.Sprintf("issue %d 日本", i)})
			created := obj(d["createIssue"])["issue"].(J)
			// Resolve the ID through repository scope, as the baseline's public
			// node lookup expects an owner/repository-qualified locator.
			resolved := call(`query($n:Int!){repository(owner:"replay-user",name:"r"){issue(number:$n){id}}}`, J{"n": created["number"]})
			ids = append(ids, obj(obj(resolved["repository"])["issue"])["id"])
			outputs = append(outputs, []byte(js(d)))
		}
		for _, id := range ids {
			d := call(`mutation($i:ID!){addComment(input:{subjectId:$i,body:"reviewed"}){commentEdge{node{id body}}}}`, J{"i": id})
			outputs = append(outputs, []byte(js(d)))
			d = call(`mutation($i:ID!){closeIssue(input:{issueId:$i}){issue{id state}}}`, J{"i": id})
			outputs = append(outputs, []byte(js(d)))
		}
		outputs = append(outputs, []byte(js(call(`{repository(owner:"replay-user",name:"r"){issues(first:100,states:[CLOSED],orderBy:{field:CREATED_AT,direction:DESC}){nodes{id number title state} pageInfo{hasNextPage endCursor}}}}`, nil))))
		outputs = append(outputs, []byte(js(f.Request("GET", mustGQLURL("/api/v3/repos/replay-user/r/issues?state=all&per_page=100"), nil).Body)))
		return outputs
	}
	a, b := run(), run()
	if len(a) != len(b) {
		t.Fatal("operation count differs")
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			t.Fatalf("GraphQL/REST replay diverged at %d", i)
		}
	}
}
