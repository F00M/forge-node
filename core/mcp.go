// MCP over streamable HTTP at /mcp (JSON answers, stateless). Speaks the 2026-07-28 revision through
// server/discover and the earlier revisions through the initialize handshake. The tools are thin wrappers
// over the same forge operations the REST surface uses, and answer with the REST JSON shapes.
package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

const mcpModern = "2026-07-28"

var mcpSupported = []string{mcpModern, "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

const mcpInstructions = "Tools for this git forge: repositories, issues and pull requests. Owners and repository names match the REST API."

func serverInfo() *Obj { return O("name", "forge-mcp", "title", "Forge MCP", "version", "1.0.0") }

func pStr(d string) *Obj  { return O("type", "string", "description", d) }
func pNum(d string) *Obj  { return O("type", "number", "description", d) }
func pBool(d string) *Obj { return O("type", "boolean", "description", d) }

func toolSchema(props *Obj, required ...string) *Obj {
	if required == nil {
		required = []string{}
	}
	return O("type", "object", "properties", props, "required", required, "additionalProperties", false)
}

func tool(name, title, description string, input *Obj, readOnly bool) *Obj {
	return O("name", name, "title", title, "description", description, "inputSchema", input, "annotations", O("readOnlyHint", readOnly))
}

func enumProp(desc string, values ...string) *Obj {
	return O("type", "string", "enum", values, "description", desc)
}

var toolList = []*Obj{
	tool("get_me", "Get my user", "Get the authenticated user (the token owner): login, id and profile.", toolSchema(O()), true),
	tool("create_repo", "Create repository", "Create a new repository owned by the authenticated user (or by an organization when owner names one).", toolSchema(O(
		"name", pStr("Repository name"),
		"description", pStr("Repository description"),
		"private", pBool("Whether the repository is private"),
		"autoInit", pBool("Initialize with a README commit"),
		"owner", pStr("Owner login (defaults to the authenticated user)"),
	), "name"), false),
	tool("get_repo", "Get repository", "Get a repository by owner and name.", toolSchema(O("owner", pStr("Repository owner"), "repo", pStr("Repository name")), "owner", "repo"), true),
	tool("list_repos", "List repositories", "List the repositories of the authenticated user.", toolSchema(O("page", pNum("Page number (1-based)"), "perPage", pNum("Results per page (max 100)"))), true),
	tool("create_issue", "Create issue", "Create a new issue in a repository.", toolSchema(O(
		"owner", pStr("Repository owner"),
		"repo", pStr("Repository name"),
		"title", pStr("Issue title"),
		"body", pStr("Issue body (Markdown)"),
		"labels", O("type", "array", "items", O("type", "string"), "description", "Names of existing labels to attach"),
	), "owner", "repo", "title"), false),
	tool("get_issue", "Get issue", "Get one issue of a repository by number.", toolSchema(O("owner", pStr("Repository owner"), "repo", pStr("Repository name"), "issue_number", pNum("Issue number")), "owner", "repo", "issue_number"), true),
	tool("list_issues", "List issues", "List the issues of a repository, filtered by state (open, closed or all).", toolSchema(O(
		"owner", pStr("Repository owner"),
		"repo", pStr("Repository name"),
		"state", enumProp("Issue state filter (default open)", "open", "closed", "all"),
		"page", pNum("Page number (1-based)"),
		"perPage", pNum("Results per page (max 100)"),
	), "owner", "repo"), true),
	tool("update_issue", "Update issue", "Update an issue: title, body or state (open/closed).", toolSchema(O(
		"owner", pStr("Repository owner"),
		"repo", pStr("Repository name"),
		"issue_number", pNum("Issue number"),
		"title", pStr("New title"),
		"body", pStr("New body"),
		"state", enumProp("New state", "open", "closed"),
	), "owner", "repo", "issue_number"), false),
	tool("add_issue_comment", "Comment on issue", "Add a comment to an issue or pull request.", toolSchema(O("owner", pStr("Repository owner"), "repo", pStr("Repository name"), "issue_number", pNum("Issue or pull request number"), "body", pStr("Comment text")), "owner", "repo", "issue_number", "body"), false),
	tool("create_pull_request", "Create pull request", "Open a pull request from a head branch into a base branch.", toolSchema(O(
		"owner", pStr("Repository owner"),
		"repo", pStr("Repository name"),
		"title", pStr("Pull request title"),
		"body", pStr("Pull request description"),
		"head", pStr("Branch with the changes"),
		"base", pStr("Branch to merge into"),
		"draft", pBool("Open as a draft"),
	), "owner", "repo", "title", "head", "base"), false),
	tool("get_pull_request", "Get pull request", "Get one pull request by number.", toolSchema(O("owner", pStr("Repository owner"), "repo", pStr("Repository name"), "pull_number", pNum("Pull request number")), "owner", "repo", "pull_number"), true),
	tool("list_pull_requests", "List pull requests", "List the pull requests of a repository, filtered by state (open, closed or all).", toolSchema(O(
		"owner", pStr("Repository owner"),
		"repo", pStr("Repository name"),
		"state", enumProp("State filter (default open)", "open", "closed", "all"),
	), "owner", "repo"), true),
	tool("merge_pull_request", "Merge pull request", "Merge a pull request (merge, squash or rebase).", toolSchema(O(
		"owner", pStr("Repository owner"),
		"repo", pStr("Repository name"),
		"pull_number", pNum("Pull request number"),
		"merge_method", enumProp("Merge method (default merge)", "merge", "squash", "rebase"),
		"commit_title", pStr("Title of the merge commit"),
		"commit_message", pStr("Extra detail for the merge commit"),
	), "owner", "repo", "pull_number"), false),
	tool("list_branches", "List branches", "List the branches of a repository with their tip commits.", toolSchema(O("owner", pStr("Repository owner"), "repo", pStr("Repository name")), "owner", "repo"), true),
}

// toolsJSON is the tools/list result, serialized once: two calls answer byte-identical arrays.
var toolsJSON = Raw(toJSON(toolList))

var toolNames = func() map[string]bool {
	m := map[string]bool{}
	for _, t := range toolList {
		name, _ := t.Get("name")
		m[name.(string)] = true
	}
	return m
}()

type toolInputError struct{ msg string }

func (e *toolInputError) Error() string { return e.msg }

func stringArg(args map[string]any, names ...string) string {
	for _, name := range names {
		if s, ok := args[name].(string); ok && s != "" {
			return s
		}
	}
	panic(&toolInputError{"missing required argument: " + names[0]})
}

func optionalString(args map[string]any, names ...string) (string, bool) {
	for _, name := range names {
		if s, ok := args[name].(string); ok {
			return s, true
		}
	}
	return "", false
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

func numberArg(args map[string]any, names ...string) float64 {
	for _, name := range names {
		switch v := args[name].(type) {
		case float64:
			if v == math.Trunc(v) && !math.IsInf(v, 0) {
				return v
			}
		case string:
			if isDigits(v) {
				f, _ := strconv.ParseFloat(v, 64)
				return f
			}
		}
	}
	panic(&toolInputError{"missing required argument: " + names[0]})
}

type Mcp struct {
	forge *Forge
	views *Views
}

func NewMcp(f *Forge, v *Views) *Mcp { return &Mcp{forge: f, views: v} }

func rpcError(id any, code int, message string) *Obj {
	return O("jsonrpc", "2.0", "id", id, "error", O("code", code, "message", message))
}

// handle answers one POST body (already authenticated): status and JSON text ("" for 202).
func (m *Mcp) handle(body []byte) (int, []byte) {
	message, err := parseJSON(body)
	if err != nil {
		return 400, toJSON(rpcError(nil, -32700, "Parse error"))
	}
	if list, ok := message.([]any); ok {
		if len(list) == 0 {
			return 400, toJSON(rpcError(nil, -32600, "Invalid Request: empty batch"))
		}
		var answers []any
		for _, item := range list {
			if a := m.one(item); a != nil {
				answers = append(answers, a)
			}
		}
		if len(answers) == 0 {
			return 202, []byte{}
		}
		return 200, toJSON(answers)
	}
	answer := m.one(message)
	if answer == nil {
		return 202, []byte{}
	}
	return 200, toJSON(answer)
}

type rpcErr struct {
	code    int
	message string
}

func (m *Mcp) one(message any) (answer *Obj) {
	request, ok := message.(map[string]any)
	if !ok {
		return rpcError(nil, -32600, "Invalid Request")
	}
	id, hasID := request["id"]
	method, isStr := request["method"].(string)
	if !isStr {
		if !hasID {
			return nil
		}
		_, hasResult := request["result"]
		_, hasError := request["error"]
		if hasResult || hasError {
			return nil
		}
		return rpcError(id, -32600, "Invalid Request")
	}
	if !hasID {
		return nil
	}
	params, ok := request["params"].(map[string]any)
	if !ok {
		params = map[string]any{}
	}
	defer func() {
		if r := recover(); r != nil {
			msg := "Internal error"
			if e, ok := r.(error); ok {
				msg = e.Error()
			} else {
				msg = fmt.Sprint(r)
			}
			answer = rpcError(id, -32603, msg)
		}
	}()
	result, rerr := m.dispatch(method, params)
	if rerr != nil {
		return rpcError(id, rerr.code, rerr.message)
	}
	return O("jsonrpc", "2.0", "id", id, "result", result)
}

func capabilities() *Obj { return O("tools", O("listChanged", false)) }

func (m *Mcp) dispatch(method string, params map[string]any) (any, *rpcErr) {
	switch method {
	case "server/discover":
		return O(
			"supportedVersions", mcpSupported,
			"capabilities", capabilities(),
			"serverInfo", serverInfo(),
			"instructions", mcpInstructions,
			"_meta", O("io.modelcontextprotocol/serverInfo", serverInfo()),
		), nil
	case "initialize":
		requested := mcpSupported[1]
		if s, ok := params["protocolVersion"].(string); ok {
			requested = s
		}
		chosen := mcpSupported[1]
		if containsStr(mcpSupported, requested) && requested != mcpModern {
			chosen = requested
		}
		return O("protocolVersion", chosen, "capabilities", capabilities(), "serverInfo", serverInfo(), "instructions", mcpInstructions), nil
	case "ping", "logging/setLevel":
		return O(), nil
	case "tools/list":
		return O("tools", toolsJSON), nil
	case "tools/call":
		return m.call(params)
	case "resources/list":
		return O("resources", []any{}), nil
	case "resources/templates/list":
		return O("resourceTemplates", []any{}), nil
	case "prompts/list":
		return O("prompts", []any{}), nil
	}
	return nil, &rpcErr{-32601, "Method not found: " + method}
}

func toolError(text string) *Obj {
	return O("content", []any{O("type", "text", "text", text)}, "isError", true)
}

func (m *Mcp) call(params map[string]any) (result any, rerr *rpcErr) {
	name, ok := params["name"].(string)
	if !ok || !toolNames[name] {
		return nil, &rpcErr{-32602, "Unknown tool: " + jsString(orUndef(params, "name"))}
	}
	args, ok := params["arguments"].(map[string]any)
	if !ok {
		args = map[string]any{}
	}
	defer func() {
		if r := recover(); r != nil {
			switch e := r.(type) {
			case *toolInputError:
				result, rerr = toolError(e.msg), nil
			case *ForgeError:
				detail := ""
				if len(e.Errors) > 0 {
					detail = ": " + toJSONString(e.Errors)
				}
				result, rerr = toolError(strconv.Itoa(e.Status)+" "+e.Message+detail), nil
			default:
				panic(r)
			}
		}
	}()
	value, isList := m.run(name, args)
	text := string(toJSON(value))
	var structured any = value
	if isList {
		structured = O("items", value)
	}
	return O("content", []any{O("type", "text", "text", text)}, "structuredContent", structured, "isError", false), nil
}

func (m *Mcp) repoArg(args map[string]any) *Repo {
	owner := stringArg(args, "owner")
	name := stringArg(args, "repo", "name")
	r := m.forge.visibleRepo(owner, name)
	if r == nil {
		panic(ferr(404, "Not Found: repository "+owner+"/"+name))
	}
	return r
}

func (m *Mcp) issueArg(r *Repo, args map[string]any, names ...string) *Issue {
	n := numberArg(args, names...)
	issue := r.Issues[int(n)]
	if issue == nil || float64(int(n)) != n {
		panic(ferr(404, "Not Found: #"+jsNumber(n)))
	}
	return issue
}

func pageArgs(args map[string]any) (int, int) {
	perPage := 30.0
	if v, ok := args["perPage"].(float64); ok {
		perPage = v
	}
	perPage = math.Min(100, math.Max(1, perPage))
	page := 1.0
	if v, ok := args["page"].(float64); ok {
		page = v
	}
	page = math.Max(1, page)
	return int((page - 1) * perPage), int(page * perPage)
}

func clampSlice(n, start, end int) (int, int) {
	if start > n {
		start = n
	}
	if end > n {
		end = n
	}
	if end < start {
		end = start
	}
	return start, end
}

// run executes one tool; the bool reports a list answer (wrapped as {items} in structuredContent).
func (m *Mcp) run(name string, args map[string]any) (any, bool) {
	f := m.forge
	v := m.views
	switch name {
	case "get_me":
		return v.userFull(f.viewer), false
	case "create_repo":
		repoName := stringArg(args, "name")
		owner := f.viewer
		if login, ok := optionalString(args, "owner", "organization"); ok && login != "" && strings.ToLower(login) != strings.ToLower(f.viewer.Login) {
			other := f.user(login)
			if other == nil || other.Type != "Organization" {
				panic(ferr(404, "Not Found: owner "+login))
			}
			owner = other
		}
		var desc *string
		if d, ok := optionalString(args, "description"); ok {
			desc = &d
		}
		r, err := f.createRepo(owner, createRepoOptions{
			Name:        repoName,
			Description: desc,
			Private:     args["private"] == true,
			AutoInit:    args["autoInit"] == true || args["auto_init"] == true,
			HasIssues:   true,
		})
		must(err)
		return Raw(v.repo(r)), false
	case "get_repo":
		return Raw(v.repo(m.repoArg(args))), false
	case "list_repos":
		repos := f.viewerRepos()
		start, end := pageArgs(args)
		start, end = clampSlice(len(repos), start, end)
		out := []any{}
		for _, r := range repos[start:end] {
			out = append(out, Raw(v.repo(r)))
		}
		return out, true
	case "create_issue":
		r := m.repoArg(args)
		title := stringArg(args, "title")
		var body any = Undefined
		if b, ok := optionalString(args, "body"); ok {
			body = b
		}
		issue, err := f.createIssue(r, title, body, orUndef(args, "labels"))
		must(err)
		return Raw(v.issue(issue)), false
	case "get_issue":
		r := m.repoArg(args)
		return Raw(v.issue(m.issueArg(r, args, "issue_number", "number"))), false
	case "list_issues":
		r := m.repoArg(args)
		state := "open"
		if s, ok := optionalString(args, "state"); ok {
			state = strings.ToLower(s)
		}
		start, end := pageArgs(args)
		var list []*Issue
		for i := len(r.IssueList) - 1; i >= 0; i-- {
			issue := r.IssueList[i]
			if state == "all" || issue.State == state {
				list = append(list, issue)
			}
		}
		start, end = clampSlice(len(list), start, end)
		out := []any{}
		for _, issue := range list[start:end] {
			out = append(out, Raw(v.issue(issue)))
		}
		return out, true
	case "update_issue":
		r := m.repoArg(args)
		issue := m.issueArg(r, args, "issue_number", "number")
		p := newIssuePatch()
		p.Title, p.Body, p.State = orUndef(args, "title"), orUndef(args, "body"), orUndef(args, "state")
		must(f.updateIssue(issue, p))
		return Raw(v.issue(issue)), false
	case "add_issue_comment":
		r := m.repoArg(args)
		issue := m.issueArg(r, args, "issue_number", "number")
		body := stringArg(args, "body")
		c, err := f.addComment(issue, body)
		must(err)
		return v.comment(c), false
	case "create_pull_request":
		r := m.repoArg(args)
		title := stringArg(args, "title")
		head := stringArg(args, "head")
		base := stringArg(args, "base")
		var body any = Undefined
		if b, ok := optionalString(args, "body"); ok {
			body = b
		}
		issue, err := f.createPull(r, title, body, head, base, orUndef(args, "draft"))
		must(err)
		return Raw(v.pull(issue)), false
	case "get_pull_request":
		r := m.repoArg(args)
		issue := m.issueArg(r, args, "pull_number", "number")
		if issue.Pull == nil {
			panic(ferr(404, "Not Found: not a pull request"))
		}
		return Raw(v.pull(issue)), false
	case "list_pull_requests":
		r := m.repoArg(args)
		state := "open"
		if s, ok := optionalString(args, "state"); ok {
			state = strings.ToLower(s)
		}
		out := []any{}
		for i := len(r.IssueList) - 1; i >= 0; i-- {
			issue := r.IssueList[i]
			if issue.Pull != nil && (state == "all" || issue.State == state) {
				out = append(out, Raw(v.pull(issue)))
			}
		}
		return out, true
	case "merge_pull_request":
		r := m.repoArg(args)
		issue := m.issueArg(r, args, "pull_number", "number")
		if issue.Pull == nil {
			panic(ferr(404, "Not Found: not a pull request"))
		}
		var method any = Undefined
		if s, ok := optionalString(args, "merge_method", "mergeMethod"); ok {
			method = s
		}
		sha, err := f.mergePull(issue, method, orUndef(args, "commit_title"), orUndef(args, "commit_message"), Undefined)
		must(err)
		return O("sha", sha, "merged", true, "message", "Pull Request successfully merged", "number", issue.Number, "html_url", v.origin+"/"+r.Owner.Login+"/"+r.Name+"/pull/"+strconv.Itoa(issue.Number)), false
	case "list_branches":
		r := m.repoArg(args)
		out := []any{}
		for _, ref := range r.Refs.Keys() {
			if strings.HasPrefix(ref, "refs/heads/") {
				oid, _ := r.Refs.Get(ref)
				out = append(out, v.branchShort(r, ref[11:], oid))
			}
		}
		return out, true
	}
	panic(&toolInputError{"unknown tool " + name})
}
