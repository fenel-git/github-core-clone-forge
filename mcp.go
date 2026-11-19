package main

import (
	"fmt"
	"math"
	"net/url"
	"regexp"
)

const mcpModernRevision = "2026-07-28"

var mcpLegacyRevisions = []string{"2025-11-25", "2025-06-18"}
var mcpJSONHeaders = map[string]string{"content-type": "application/json; charset=utf-8"}

type mcpProperty struct {
	typ       string
	minLength int
	minimum   int
	enum      []string
	pattern   string
	not       any
}
type mcpTool struct {
	name, description string
	properties        map[string]mcpProperty
	required          []string
}

var mcpRepoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func mcpText() mcpProperty     { return mcpProperty{typ: "string"} }
func mcpNonEmpty() mcpProperty { return mcpProperty{typ: "string", minLength: 1} }
func mcpRepoSegment() mcpProperty {
	return mcpProperty{typ: "string", minLength: 1, pattern: `^[A-Za-z0-9_.-]+$`, not: J{"enum": []any{".", ".."}}}
}

var mcpTools = []mcpTool{
	{name: "create_repo", description: "Create a repository for the authenticated user.", properties: map[string]mcpProperty{"name": mcpNonEmpty(), "description": mcpText(), "private": {typ: "boolean"}}, required: []string{"name"}},
	{name: "get_repo", description: "Get a repository by owner and name.", properties: map[string]mcpProperty{"owner": mcpRepoSegment(), "repo": mcpRepoSegment()}, required: []string{"owner", "repo"}},
	{name: "create_issue", description: "Open an issue in a repository.", properties: map[string]mcpProperty{"owner": mcpRepoSegment(), "repo": mcpRepoSegment(), "title": mcpNonEmpty(), "body": mcpText()}, required: []string{"owner", "repo", "title"}},
	{name: "list_issues", description: "List repository issues, optionally filtered by state.", properties: map[string]mcpProperty{"owner": mcpRepoSegment(), "repo": mcpRepoSegment(), "state": {typ: "string", enum: []string{"open", "closed", "all"}}}, required: []string{"owner", "repo"}},
	{name: "create_pull_request", description: "Open a pull request from a head branch into a base branch.", properties: map[string]mcpProperty{"owner": mcpRepoSegment(), "repo": mcpRepoSegment(), "title": mcpNonEmpty(), "body": mcpText(), "head": mcpNonEmpty(), "base": mcpNonEmpty()}, required: []string{"owner", "repo", "title", "head", "base"}},
	{name: "merge_pull_request", description: "Squash and merge a pull request by number.", properties: map[string]mcpProperty{"owner": mcpRepoSegment(), "repo": mcpRepoSegment(), "pull_number": {typ: "integer", minimum: 1}}, required: []string{"owner", "repo", "pull_number"}},
}

func mcpSchema(t mcpTool) J {
	props := J{}
	for k, p := range t.properties {
		x := J{"type": p.typ}
		if p.minLength != 0 {
			x["minLength"] = p.minLength
		}
		if p.minimum != 0 {
			x["minimum"] = p.minimum
		}
		if p.enum != nil {
			a := make([]any, len(p.enum))
			for i, v := range p.enum {
				a[i] = v
			}
			x["enum"] = a
		}
		if p.pattern != "" {
			x["pattern"] = p.pattern
		}
		if p.not != nil {
			x["not"] = p.not
		}
		props[k] = x
	}
	req := make([]any, len(t.required))
	for i, v := range t.required {
		req[i] = v
	}
	return J{"type": "object", "properties": props, "required": req}
}
func mcpToolsList() []any {
	a := make([]any, 0, len(mcpTools))
	for _, t := range mcpTools {
		a = append(a, J{"name": t.name, "description": t.description, "inputSchema": mcpSchema(t)})
	}
	return a
}
func mcpResult(id any, v any) Reply {
	return Reply{Status: 200, Body: J{"jsonrpc": "2.0", "id": id, "result": v}, Headers: mcpJSONHeaders}
}
func mcpError(id any, code int, message string, status int) Reply {
	return Reply{Status: status, Body: J{"jsonrpc": "2.0", "id": id, "error": J{"code": code, "message": message}}, Headers: mcpJSONHeaders}
}
func mcpValidProperty(v any, p mcpProperty) bool {
	switch p.typ {
	case "integer":
		var n float64
		switch x := v.(type) {
		case float64:
			n = x
		case int:
			n = float64(x)
		case int64:
			n = float64(x)
		default:
			return false
		}
		return !math.IsNaN(n) && !math.IsInf(n, 0) && math.Trunc(n) == n && math.Abs(n) <= 9007199254740991 && n >= float64(p.minimum)
	case "boolean":
		_, ok := v.(bool)
		return ok
	default:
		s, ok := v.(string)
		if !ok || len(s) < p.minLength {
			return false
		}
		if p.enum != nil {
			found := false
			for _, x := range p.enum {
				if x == s {
					found = true
				}
			}
			if !found {
				return false
			}
		}
		return true
	}
}
func mcpValidRepo(v any) bool {
	s, ok := v.(string)
	return ok && mcpRepoRE.MatchString(s) && s != "." && s != ".."
}
func mcpPick(args J, keys ...string) J {
	out := J{}
	for _, k := range keys {
		if v, ok := args[k]; ok {
			out[k] = v
		}
	}
	return out
}
func mcpDispatch(api *Forge, name string, args J) Reply {
	if name == "create_repo" {
		return api.Request("POST", mcpURL("/api/v3/user/repos"), mcpPick(args, "name", "description", "private"))
	}
	owner := url.PathEscape(str(args["owner"]))
	repo := url.PathEscape(str(args["repo"]))
	path := "/api/v3/repos/" + owner + "/" + repo
	switch name {
	case "get_repo":
		return api.Request("GET", mcpURL(path), nil)
	case "create_issue":
		return api.Request("POST", mcpURL(path+"/issues"), mcpPick(args, "title", "body"))
	case "list_issues":
		p := path + "/issues"
		if state, ok := args["state"]; ok {
			p += "?state=" + url.QueryEscape(str(state))
		}
		return api.Request("GET", mcpURL(p), nil)
	case "create_pull_request":
		return api.Request("POST", mcpURL(path+"/pulls"), mcpPick(args, "title", "body", "head", "base"))
	case "merge_pull_request":
		return api.Request("PUT", mcpURL(fmt.Sprintf("%s/pulls/%v/merge", path, args["pull_number"])), J{"merge_method": "squash"})
	}
	return fail(404, "Not Found")
}
func mcpURL(path string) *url.URL { u, _ := url.Parse(path); return u }
func mcpToolResult(text string, data J, isError bool) J {
	v := J{"content": []any{J{"type": "text", "text": text}}}
	if data != nil {
		v["structuredContent"] = data
	}
	if isError {
		v["isError"] = true
	}
	return v
}
func mcpFindTool(name any) (mcpTool, bool) {
	s, ok := name.(string)
	if !ok {
		return mcpTool{}, false
	}
	for _, t := range mcpTools {
		if t.name == s {
			return t, true
		}
	}
	return mcpTool{}, false
}
func mcpCallTool(id any, params J, api *Forge) Reply {
	tool, ok := mcpFindTool(params["name"])
	if !ok {
		return mcpError(id, -32602, "Missing or unknown tool name", 200)
	}
	raw, ok := params["arguments"]
	args := J{}
	if ok {
		var good bool
		args, good = raw.(map[string]any)
		if !good {
			return mcpError(id, -32602, "Tool arguments must be an object", 200)
		}
	}
	for key, prop := range tool.properties {
		v, exists := args[key]
		if !exists {
			for _, required := range tool.required {
				if required == key {
					return mcpError(id, -32602, "Missing argument: "+key, 200)
				}
			}
			continue
		}
		if !mcpValidProperty(v, prop) || ((key == "owner" || key == "repo") && !mcpValidRepo(v)) {
			return mcpError(id, -32602, "Invalid argument: "+key, 200)
		}
	}
	answer := mcpDispatch(api, tool.name, args)
	if answer.Status < 200 || answer.Status >= 300 {
		detail := ""
		if message, ok := obj(answer.Body)["message"].(string); ok {
			detail = ": " + message
		}
		return mcpResult(id, mcpToolResult(fmt.Sprintf("REST request failed with HTTP %d%s", answer.Status, detail), nil, true))
	}
	var data J
	if m, ok := answer.Body.(map[string]any); ok {
		data = m
	}
	return mcpResult(id, mcpToolResult(js(answer.Body), data, false))
}

// MCP implements the stateless JSON-RPC surface. Bearer authentication and JSON parse errors belong to HTTP routing.
func MCP(body any, api *Forge) Reply {
	request, ok := body.(map[string]any)
	if !ok || request["jsonrpc"] != "2.0" {
		return mcpError(nil, -32600, "Invalid Request", 400)
	}
	method, ok := request["method"].(string)
	if !ok || method == "" {
		return mcpError(nil, -32600, "Invalid Request", 400)
	}
	id, idPresent := request["id"]
	if !idPresent {
		return Reply{Status: 202, Body: nil}
	}
	valid := id == nil
	if _, ok := id.(string); ok {
		valid = true
	}
	if n, ok := id.(float64); ok && !math.IsNaN(n) && !math.IsInf(n, 0) {
		valid = true
	}
	if _, ok := id.(int); ok {
		valid = true
	}
	if _, ok := id.(int64); ok {
		valid = true
	}
	if !valid {
		return mcpError(nil, -32600, "Invalid Request ID", 400)
	}
	params := J{}
	if raw, ok := request["params"]; ok {
		var good bool
		params, good = raw.(map[string]any)
		if !good {
			return mcpError(id, -32602, "Params must be an object", 200)
		}
	}
	switch method {
	case "server/discover":
		server := J{"name": "github-core-clone", "version": "1.0.0"}
		return mcpResult(id, J{"serverInfo": server, "capabilities": J{"tools": J{}}, "supportedVersions": []any{mcpModernRevision}, "_meta": J{"io.modelcontextprotocol/serverInfo": server}})
	case "initialize":
		version := mcpLegacyRevisions[0]
		if requested, ok := params["protocolVersion"].(string); ok {
			for _, v := range mcpLegacyRevisions {
				if requested == v {
					version = requested
					break
				}
			}
		}
		return mcpResult(id, J{"serverInfo": J{"name": "github-core-clone", "version": "1.0.0"}, "capabilities": J{"tools": J{}}, "protocolVersion": version})
	case "ping":
		return mcpResult(id, J{})
	case "tools/list":
		return mcpResult(id, J{"tools": mcpToolsList()})
	case "tools/call":
		return mcpCallTool(id, params, api)
	default:
		return mcpError(id, -32601, "Method not found", 200)
	}
}
