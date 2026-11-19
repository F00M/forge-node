// The GitHub REST v3 surface under /api/v3: repositories, git refs, branches, commits, contents, labels,
// issues, comments and pull requests. Errors are GitHub's {message, documentation_url[, errors]} bodies.
package main

import (
	"encoding/base64"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const jsonType = "application/json; charset=utf-8"
const docsURL = "https://docs.github.com/rest"

func jsonResp(status int, body any, extra string) *Response {
	var b []byte
	switch x := body.(type) {
	case []byte:
		b = x
	case string:
		b = []byte(x)
	case Raw:
		b = x
	default:
		b = toJSON(body)
	}
	return &Response{Status: status, Type: jsonType, Body: b, Extra: extra}
}

func errorJSON(status int, message string, errs []any) *Response {
	body := O("message", message, "documentation_url", docsURL, "status", strconv.Itoa(status))
	if errs != nil {
		body.Set("errors", errs)
	}
	return jsonResp(status, body, "")
}

func notFound() *Response { return errorJSON(404, "Not Found", nil) }

func noContent() *Response { return &Response{Status: 204, Type: jsonType} }

type badJSON struct{}

// parseBody returns the JSON object of a request body ({} when empty); panics badJSON for bad text.
func parseBody(req *Request) map[string]any {
	text := strings.TrimSpace(string(req.Body))
	if text == "" {
		return map[string]any{}
	}
	v, err := parseJSON([]byte(text))
	if err != nil {
		panic(badJSON{})
	}
	m, ok := v.(map[string]any)
	if !ok {
		panic(ferr(422, "Invalid request.\n\nThe request body must be a JSON object."))
	}
	return m
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func intParam(p *Params, name string, fallback, min, max int) int {
	raw, ok := p.Get(name)
	if !ok || raw == "" {
		return fallback
	}
	v, ok := jsParseInt(raw)
	if !ok {
		return fallback
	}
	if v < min {
		v = min
	}
	if v > max {
		v = max
	}
	return v
}

// jsParseInt reads a leading integer as Number.parseInt(text, 10) does.
func jsParseInt(s string) (int, bool) {
	s = strings.TrimLeft(s, " \t\n\r\f\v")
	i := 0
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	start := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == start {
		return 0, false
	}
	n, err := strconv.Atoi(s[start:i])
	if err != nil {
		return 0, false
	}
	if neg {
		n = -n
	}
	return n, true
}

// jsNumberInt reads a path segment as Number(text) and reports whether it is an integer.
func jsNumberInt(s string) (int, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, true
	}
	if n, err := strconv.Atoi(t); err == nil {
		return n, true
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil || math.IsInf(f, 0) || f != math.Trunc(f) {
		return 0, false
	}
	return int(f), true
}

func joinJSON(items [][]byte) []byte {
	n := 2
	for _, it := range items {
		n += len(it) + 1
	}
	out := make([]byte, 0, n)
	out = append(out, '[')
	for i, it := range items {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, it...)
	}
	return append(out, ']')
}

type Rest struct {
	forge *Forge
	views *Views
}

// paginate slices a list of n items by page/per_page and builds GitHub's Link header for it.
func (rt *Rest) paginate(n int, req *Request, params *Params) (int, int, string) {
	perPage := intParam(params, "per_page", 30, 1, 100)
	page := intParam(params, "page", 1, 1, 1000000)
	start := (page - 1) * perPage
	if start > n {
		start = n
	}
	end := start + perPage
	if end > n {
		end = n
	}
	last := int(math.Ceil(float64(n) / float64(perPage)))
	if last < 1 {
		last = 1
	}
	host := req.Host
	if host == "" {
		host = "localhost"
	}
	link := func(k int) string {
		c := params.Clone()
		c.Set("per_page", strconv.Itoa(perPage))
		c.Set("page", strconv.Itoa(k))
		return "http://" + host + req.Path + "?" + c.String()
	}
	var rels []string
	if page < last {
		rels = append(rels, "<"+link(page+1)+">; rel=\"next\"")
		rels = append(rels, "<"+link(last)+">; rel=\"last\"")
	}
	if page > 1 {
		prev := page - 1
		if prev > last {
			prev = last
		}
		rels = append(rels, "<"+link(1)+">; rel=\"first\"")
		rels = append(rels, "<"+link(prev)+">; rel=\"prev\"")
	}
	extra := ""
	if len(rels) > 0 {
		extra = "Link: " + strings.Join(rels, ", ") + "\r\n"
	}
	return start, end, extra
}

func (rt *Rest) handle(req *Request, segments []string) (resp *Response) {
	defer func() {
		if r := recover(); r != nil {
			switch e := r.(type) {
			case badJSON:
				resp = errorJSON(400, "Problems parsing JSON", nil)
			case *ForgeError:
				resp = errorJSON(e.Status, e.Message, e.Errors)
			default:
				panic(r)
			}
		}
	}()
	return rt.route(req, segments)
}

func (rt *Rest) route(req *Request, s []string) *Response {
	method := req.Method
	n := len(s)
	f := rt.forge
	v := rt.views
	params := parseParams(req.Query)

	if n == 0 || (n == 1 && s[0] == "") {
		if method != "GET" && method != "HEAD" {
			return notFound()
		}
		api := v.api
		return jsonResp(200, O(
			"current_user_url", api+"/user",
			"authorizations_url", api+"/authorizations",
			"emojis_url", api+"/emojis",
			"events_url", api+"/events",
			"issues_url", api+"/issues",
			"rate_limit_url", api+"/rate_limit",
			"repository_url", api+"/repos/{owner}/{repo}",
			"user_url", api+"/users/{user}",
			"user_repositories_url", api+"/users/{user}/repos{?type,page,per_page,sort}",
			"graphql_url", v.origin+"/api/graphql",
		), "X-OAuth-Scopes: repo, read:org, workflow, gist\r\nX-Accepted-OAuth-Scopes: \r\nX-GitHub-Media-Type: github.v3; format=json\r\n")
	}
	head := s[0]
	if head == "meta" && n == 1 && method == "GET" {
		return jsonResp(200, O("verifiable_password_authentication", false, "installed_version", "3.16.0", "hooks", []any{}, "git", []any{}, "web", []any{}, "api", []any{}, "packages", []any{}, "pages", []any{}, "importer", []any{}), "")
	}
	if head == "rate_limit" && n == 1 && method == "GET" {
		reset := time.Now().Unix() + 3600
		core := func() *Obj { return O("limit", 5000, "used", 0, "remaining", 5000, "reset", reset) }
		return jsonResp(200, O("resources", O("core", core(), "graphql", core(), "search", O("limit", 30, "used", 0, "remaining", 30, "reset", reset)), "rate", core()), "")
	}
	if head == "zen" && n == 1 {
		return jsonResp(200, toJSON("Keep it logically awesome."), "")
	}
	if head == "user" {
		if n == 1 && method == "GET" {
			return jsonResp(200, v.userFull(f.viewer), "X-OAuth-Scopes: repo, read:org, workflow, gist\r\n")
		}
		if n == 2 && s[1] == "repos" {
			if method == "GET" {
				return rt.listRepos(req, params)
			}
			if method == "POST" {
				return rt.createRepo(req, f.viewer.Login)
			}
		}
		if n == 2 && s[1] == "orgs" && method == "GET" {
			out := []any{}
			for _, u := range f.userOrder {
				if u.Type == "Organization" {
					out = append(out, v.user(u))
				}
			}
			return jsonResp(200, out, "")
		}
		if n == 2 && s[1] == "emails" && method == "GET" {
			return jsonResp(200, []any{O("email", f.viewer.Email, "primary", true, "verified", true, "visibility", "private")}, "")
		}
		return notFound()
	}
	if head == "users" && n >= 2 {
		u := f.user(s[1])
		if u == nil {
			return notFound()
		}
		if n == 2 && method == "GET" {
			return jsonResp(200, v.userFull(u), "")
		}
		if n == 3 && s[2] == "repos" && method == "GET" {
			var repos []*Repo
			for _, r := range f.liveRepos() {
				if r.Owner == u && (!r.Private || u == f.viewer) {
					repos = append(repos, r)
				}
			}
			sort.SliceStable(repos, func(i, j int) bool { return strings.ToLower(repos[i].Name) < strings.ToLower(repos[j].Name) })
			start, end, extra := rt.paginate(len(repos), req, params)
			items := make([][]byte, 0, end-start)
			for _, r := range repos[start:end] {
				items = append(items, v.repo(r))
			}
			return jsonResp(200, joinJSON(items), extra)
		}
		return notFound()
	}
	if head == "orgs" && n >= 2 {
		org := f.user(s[1])
		if org == nil || org.Type != "Organization" {
			return notFound()
		}
		if n == 2 && method == "GET" {
			return jsonResp(200, v.user(org).Set("name", org.Name).Set("description", nil), "")
		}
		if n == 3 && s[2] == "repos" {
			if method == "POST" {
				return rt.createRepo(req, org.Login)
			}
			if method == "GET" {
				var repos []*Repo
				for _, r := range f.liveRepos() {
					if r.Owner == org {
						repos = append(repos, r)
					}
				}
				start, end, extra := rt.paginate(len(repos), req, params)
				items := make([][]byte, 0, end-start)
				for _, r := range repos[start:end] {
					items = append(items, v.repo(r))
				}
				return jsonResp(200, joinJSON(items), extra)
			}
		}
		return notFound()
	}
	if head == "repos" && n >= 3 {
		r := f.visibleRepo(s[1], s[2])
		if r == nil {
			return notFound()
		}
		return rt.repoRoute(req, r, s[3:], params)
	}
	return notFound()
}

// liveRepos lists every live repository in creation order (reposById's iteration order).
func (f *Forge) liveRepos() []*Repo {
	out := make([]*Repo, 0, len(f.repoOrder))
	for _, r := range f.repoOrder {
		if !r.Deleted && f.reposByID[r.ID] == r {
			out = append(out, r)
		}
	}
	return out
}

// ---------------------------------------------------------------- repositories

func (rt *Rest) listRepos(req *Request, params *Params) *Response {
	f := rt.forge
	var repos []*Repo
	for _, r := range f.liveRepos() {
		if r.Owner == f.viewer || r.Owner.Type == "Organization" {
			repos = append(repos, r)
		}
	}
	if vis, _ := params.Get("visibility"); vis == "public" || vis == "private" {
		kept := repos[:0:0]
		for _, r := range repos {
			if r.Private == (vis == "private") {
				kept = append(kept, r)
			}
		}
		repos = kept
	}
	sortKey := params.GetOr("sort", "full_name")
	defDir := "desc"
	if sortKey == "full_name" {
		defDir = "asc"
	}
	direction := params.GetOr("direction", defDir)
	cmp := func(a, b *Repo) int {
		var order int
		switch sortKey {
		case "created", "updated", "pushed":
			var ka, kb int64
			switch sortKey {
			case "created":
				ka, kb = a.CreatedAt, b.CreatedAt
			case "updated":
				ka, kb = a.UpdatedAt, b.UpdatedAt
			default:
				ka, kb = a.PushedAt, b.PushedAt
			}
			if ka < kb {
				order = -1
			} else if ka > kb {
				order = 1
			}
		default:
			ka := strings.ToLower(a.Owner.Login + "/" + a.Name)
			kb := strings.ToLower(b.Owner.Login + "/" + b.Name)
			if ka < kb {
				order = -1
			} else if ka > kb {
				order = 1
			}
		}
		if order == 0 {
			order = a.ID - b.ID
		}
		if direction == "asc" {
			return order
		}
		return -order
	}
	sort.SliceStable(repos, func(i, j int) bool { return cmp(repos[i], repos[j]) < 0 })
	start, end, extra := rt.paginate(len(repos), req, params)
	items := make([][]byte, 0, end-start)
	for _, r := range repos[start:end] {
		items = append(items, rt.views.repo(r))
	}
	return jsonResp(200, joinJSON(items), extra)
}

func strOrNil(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

func (rt *Rest) createRepo(req *Request, ownerLogin string) *Response {
	body := parseBody(req)
	owner := rt.forge.user(ownerLogin)
	if _, ok := body["name"].(string); !ok {
		panic(ferr(422, "Repository creation failed.", map[string]any{"resource": "Repository", "code": "missing_field", "field": "name"}))
	}
	defBranch, _ := body["default_branch"].(string)
	hasIssues := true
	if b, ok := body["has_issues"].(bool); ok && !b {
		hasIssues = false
	}
	r, err := rt.forge.createRepo(owner, createRepoOptions{
		Name:          body["name"],
		Description:   strOrNil(body["description"]),
		Private:       body["private"] == true || body["visibility"] == "private",
		AutoInit:      body["auto_init"] == true,
		Homepage:      strOrNil(body["homepage"]),
		HasIssues:     hasIssues,
		DefaultBranch: defBranch,
	})
	must(err)
	return jsonResp(201, rt.views.repo(r), "Location: "+rt.views.api+"/repos/"+r.Owner.Login+"/"+r.Name+"\r\n")
}

var readmeRe = regexp.MustCompile(`(?i)^readme(\.|$)`)

func (rt *Rest) repoRoute(req *Request, r *Repo, s []string, params *Params) *Response {
	method := req.Method
	f := rt.forge
	v := rt.views
	n := len(s)
	if n == 0 || (n == 1 && s[0] == "") {
		switch method {
		case "GET", "HEAD":
			return jsonResp(200, v.repo(r), "")
		case "PATCH":
			return rt.patchRepo(req, r)
		case "DELETE":
			f.deleteRepo(r)
			return noContent()
		}
		return notFound()
	}
	switch s[0] {
	case "issues":
		return rt.issuesRoute(req, r, s[1:], params)
	case "pulls":
		return rt.pullsRoute(req, r, s[1:], params)
	case "labels":
		return rt.labelsRoute(req, r, s[1:], params)
	case "contents":
		return rt.contentsRoute(req, r, strings.Join(s[1:], "/"), params)
	case "readme":
		if method != "GET" {
			return notFound()
		}
		tip := f.resolveCommitish(r, params.GetOr("ref", ""))
		if tip == "" {
			return errorJSON(404, "Not Found", nil)
		}
		entries, _ := f.store.Tree(f.store.Commit(tip).Tree)
		for _, e := range entries {
			if readmeRe.MatchString(e.Name) && e.Mode != "40000" {
				return rt.contentsRoute(req, r, e.Name, params)
			}
		}
		return errorJSON(404, "Not Found", nil)
	case "branches":
		return rt.branchesRoute(req, r, s[1:], params)
	case "commits":
		return rt.commitsRoute(req, r, s[1:], params)
	case "git":
		return rt.gitRoute(req, r, s[1:], params)
	case "collaborators":
		if method == "GET" && n == 1 {
			u := v.user(r.Owner).Set("permissions", O("admin", true, "maintain", true, "push", true, "triage", true, "pull", true)).Set("role_name", "admin")
			return jsonResp(200, []any{u}, "")
		}
		if method == "GET" && n == 3 && s[2] == "permission" {
			return jsonResp(200, O("permission", "admin", "role_name", "admin", "user", v.user(f.viewer)), "")
		}
		return notFound()
	case "languages":
		if method == "GET" {
			return jsonResp(200, O(), "")
		}
		return notFound()
	case "topics":
		if method == "GET" {
			return jsonResp(200, O("names", []any{}), "")
		}
		return notFound()
	case "tags":
		if method != "GET" {
			return notFound()
		}
		var tags []string
		for _, name := range r.Refs.Keys() {
			if strings.HasPrefix(name, "refs/tags/") {
				tags = append(tags, name)
			}
		}
		sort.SliceStable(tags, func(i, j int) bool { return tags[i] > tags[j] })
		start, end, extra := rt.paginate(len(tags), req, params)
		out := []any{}
		for _, name := range tags[start:end] {
			oid, _ := r.Refs.Get(name)
			peeled := f.store.Peel(oid)
			out = append(out, O("name", name[10:], "commit", O("sha", peeled, "url", v.api+"/repos/"+r.Owner.Login+"/"+r.Name+"/commits/"+peeled), "node_id", "REF_"+name))
		}
		return jsonResp(200, out, extra)
	}
	return notFound()
}

func (rt *Rest) patchRepo(req *Request, r *Repo) *Response {
	body := parseBody(req)
	f := rt.forge
	if v, ok := body["name"]; ok {
		name, isStr := v.(string)
		if !isStr {
			panic(ferr(422, "Validation Failed", map[string]any{"resource": "Repository", "code": "invalid", "field": "name"}))
		}
		must(f.renameRepo(r, name))
	}
	if v, ok := body["description"]; ok {
		r.Description = strOrNil(v)
	}
	if v, ok := body["homepage"]; ok {
		r.Homepage = strOrNil(v)
	}
	if v, ok := body["private"]; ok {
		r.Private = v == true
	}
	if vis := body["visibility"]; vis == "private" || vis == "public" {
		r.Private = vis == "private"
	}
	if v, ok := body["has_issues"]; ok {
		r.HasIssues = v != false
	}
	if v, ok := body["has_wiki"]; ok {
		r.HasWiki = v != false
	}
	if v, ok := body["has_projects"]; ok {
		r.HasProjects = v != false
	}
	if v, ok := body["archived"]; ok {
		r.Archived = v == true
	}
	if v, ok := body["default_branch"]; ok {
		name, isStr := v.(string)
		_, exists := f.branchTip(r, name)
		if !isStr || (r.Refs.Len() > 0 && !exists) {
			panic(ferr(422, "Validation Failed", map[string]any{"resource": "Repository", "code": "invalid", "field": "default_branch"}))
		}
		r.DefaultBranch = name
		r.Version++
	}
	f.touchRepo(r)
	return jsonResp(200, rt.views.repo(r), "")
}

// ---------------------------------------------------------------- git data

func (rt *Rest) gitRoute(req *Request, r *Repo, s []string, params *Params) *Response {
	method := req.Method
	f := rt.forge
	v := rt.views
	kind := ""
	if len(s) > 0 {
		kind = s[0]
	}
	rest := ""
	if len(s) > 1 {
		rest = strings.Join(s[1:], "/")
	}
	if kind == "refs" || kind == "ref" || kind == "matching-refs" {
		if kind == "refs" && rest == "" && method == "POST" {
			body := parseBody(req)
			ref, _ := body["ref"].(string)
			sha, _ := body["sha"].(string)
			if !strings.HasPrefix(ref, "refs/") || len(strings.Split(ref, "/")) < 3 {
				panic(ferr(422, "Reference name must start with 'refs/' and have at least two slashes."))
			}
			if !fullShaRe.MatchString(sha) || !f.store.Has(sha) {
				panic(ferr(422, "Object does not exist"))
			}
			if r.Refs.Has(ref) {
				panic(ferr(422, "Reference already exists"))
			}
			f.setRef(r, ref, sha)
			return jsonResp(201, v.ref(r, ref, sha), "Location: "+v.api+"/repos/"+r.Owner.Login+"/"+r.Name+"/git/"+ref+"\r\n")
		}
		full := "refs"
		if rest != "" {
			full = "refs/" + rest
		}
		if method == "GET" || method == "HEAD" {
			if kind == "refs" || kind == "ref" {
				if exact, ok := r.Refs.Get(full); ok {
					return jsonResp(200, v.ref(r, full, exact), "")
				}
				if kind == "ref" {
					return notFound()
				}
			}
			prefix := full
			if rest == "" {
				prefix = "refs/"
			}
			withSlash := prefix
			if !strings.HasSuffix(prefix, "/") {
				withSlash = prefix + "/"
			}
			var matches []string
			for _, name := range r.Refs.Keys() {
				if name == prefix || strings.HasPrefix(name, withSlash) || (kind == "matching-refs" && strings.HasPrefix(name, prefix)) {
					matches = append(matches, name)
				}
			}
			sort.SliceStable(matches, func(i, j int) bool { return matches[i] < matches[j] })
			if len(matches) == 0 {
				if kind == "matching-refs" {
					return jsonResp(200, []any{}, "")
				}
				return notFound()
			}
			out := make([]any, 0, len(matches))
			for _, name := range matches {
				oid, _ := r.Refs.Get(name)
				out = append(out, v.ref(r, name, oid))
			}
			return jsonResp(200, out, "")
		}
		if method == "PATCH" && kind == "refs" {
			current, ok := r.Refs.Get(full)
			if !ok {
				panic(ferr(422, "Reference does not exist"))
			}
			body := parseBody(req)
			sha, _ := body["sha"].(string)
			if !fullShaRe.MatchString(sha) || !f.store.Has(sha) {
				panic(ferr(422, "Object does not exist"))
			}
			if body["force"] != true && strings.HasPrefix(full, "refs/heads/") && !isAncestor(f.store, current, sha) {
				panic(ferr(422, "Update is not a fast forward"))
			}
			f.setRef(r, full, sha)
			return jsonResp(200, v.ref(r, full, sha), "")
		}
		if method == "DELETE" && kind == "refs" {
			if !r.Refs.Has(full) {
				panic(ferr(422, "Reference does not exist"))
			}
			f.setRef(r, full, "")
			return noContent()
		}
		return notFound()
	}
	if kind == "commits" && len(s) == 2 && method == "GET" {
		c := v.gitCommit(r, s[1])
		if c == nil {
			return notFound()
		}
		return jsonResp(200, c, "")
	}
	if kind == "trees" && len(s) >= 2 && method == "GET" {
		oid := strings.Join(s[1:], "/")
		if resolved := f.resolveCommitish(r, oid); resolved != "" {
			if c := f.store.Commit(resolved); c != nil {
				oid = c.Tree
			}
		}
		if _, ok := f.store.Tree(oid); !ok {
			return notFound()
		}
		repoURL := v.api + "/repos/" + r.Owner.Login + "/" + r.Name
		rec, has := params.Get("recursive")
		recursive := has && rec != "0" && rec != "false"
		out := []any{}
		var walk func(treeOid, prefix string)
		walk = func(treeOid, prefix string) {
			entries, _ := f.store.Tree(treeOid)
			for _, e := range entries {
				path := e.Name
				if prefix != "" {
					path = prefix + "/" + e.Name
				}
				typ := "blob"
				if e.Mode == "40000" {
					typ = "tree"
				} else if e.Mode == "160000" {
					typ = "commit"
				}
				mode := e.Mode
				if mode == "40000" {
					mode = "040000"
				}
				kindPath := "blobs"
				if typ == "tree" {
					kindPath = "trees"
				}
				item := O("path", path, "mode", mode, "type", typ, "sha", e.Oid, "url", repoURL+"/git/"+kindPath+"/"+e.Oid)
				if typ == "blob" {
					size := 0
					if o := f.store.Get(e.Oid); o != nil {
						size = len(o.Data)
					}
					item.Set("size", size)
				}
				out = append(out, item)
				if recursive && typ == "tree" {
					walk(e.Oid, path)
				}
			}
		}
		walk(oid, "")
		return jsonResp(200, O("sha", oid, "url", repoURL+"/git/trees/"+oid, "tree", out, "truncated", false), "")
	}
	if kind == "blobs" && len(s) == 2 && method == "GET" {
		obj := f.store.Get(s[1])
		if obj == nil || obj.Type != ObjBlob {
			return notFound()
		}
		return jsonResp(200, O("sha", s[1], "node_id", "B_"+s[1], "size", len(obj.Data), "url", v.api+"/repos/"+r.Owner.Login+"/"+r.Name+"/git/blobs/"+s[1], "content", base64.StdEncoding.EncodeToString(obj.Data), "encoding", "base64"), "")
	}
	if kind == "blobs" && len(s) == 1 && method == "POST" {
		body := parseBody(req)
		content, ok := body["content"].(string)
		if !ok {
			panic(ferr(422, "Validation Failed", map[string]any{"resource": "Blob", "code": "missing_field", "field": "content"}))
		}
		var data []byte
		if body["encoding"] == "base64" {
			data = nodeBase64Decode(content)
		} else {
			data = []byte(content)
		}
		oid := f.store.Put(ObjBlob, data)
		return jsonResp(201, O("sha", oid, "url", v.api+"/repos/"+r.Owner.Login+"/"+r.Name+"/git/blobs/"+oid), "")
	}
	if kind == "tags" && len(s) == 2 && method == "GET" {
		tag := f.store.Tag(s[1])
		if tag == nil {
			return notFound()
		}
		return jsonResp(200, O("sha", s[1], "tag", tag.Tag, "message", tag.Message, "object", O("sha", tag.Object, "type", tag.Type)), "")
	}
	return notFound()
}

// ---------------------------------------------------------------- branches and commits

func (rt *Rest) branchesRoute(req *Request, r *Repo, s []string, params *Params) *Response {
	if req.Method != "GET" && req.Method != "HEAD" {
		return notFound()
	}
	v := rt.views
	if len(s) == 0 || (len(s) == 1 && s[0] == "") {
		var names []string
		for _, name := range r.Refs.Keys() {
			if strings.HasPrefix(name, "refs/heads/") {
				names = append(names, name)
			}
		}
		sort.SliceStable(names, func(i, j int) bool { return names[i][11:] < names[j][11:] })
		start, end, extra := rt.paginate(len(names), req, params)
		out := []any{}
		for _, name := range names[start:end] {
			oid, _ := r.Refs.Get(name)
			out = append(out, v.branchShort(r, name[11:], oid))
		}
		return jsonResp(200, out, extra)
	}
	name := strings.Join(s, "/")
	oid, ok := rt.forge.branchTip(r, name)
	if !ok {
		return errorJSON(404, "Branch not found", nil)
	}
	return jsonResp(200, v.branchFull(r, name, oid), "")
}

func (rt *Rest) commitsRoute(req *Request, r *Repo, s []string, params *Params) *Response {
	if req.Method != "GET" && req.Method != "HEAD" {
		return notFound()
	}
	f := rt.forge
	v := rt.views
	if len(s) == 0 || (len(s) == 1 && s[0] == "") {
		if r.Refs.Len() == 0 {
			return errorJSON(409, "Git Repository is empty.", nil)
		}
		shaParam := params.GetOr("sha", "")
		start := f.resolveCommitish(r, shaParam)
		if start == "" {
			return errorJSON(404, "No commit found for SHA: "+shaParam, nil)
		}
		perPage := intParam(params, "per_page", 30, 1, 100)
		page := intParam(params, "page", 1, 1, 100000)
		path, hasPath := params.Get("path")
		limit := 100000
		if !hasPath {
			limit = page*perPage + 1
		}
		oids := logFrom(f.store, start, limit, nil)
		if hasPath && path != "" {
			kept := oids[:0:0]
			for _, oid := range oids {
				c := f.store.Commit(oid)
				mine := ""
				if e := lookupPath(f.store, c.Tree, path); e != nil {
					mine = e.Oid
				}
				if len(c.Parents) == 0 {
					if mine != "" {
						kept = append(kept, oid)
					}
					continue
				}
				for _, p := range c.Parents {
					other := ""
					if pc := f.store.Commit(p); pc != nil {
						if e := lookupPath(f.store, pc.Tree, path); e != nil {
							other = e.Oid
						}
					}
					if other != mine {
						kept = append(kept, oid)
						break
					}
				}
			}
			oids = kept
		}
		st, end, extra := rt.paginate(len(oids), req, params)
		out := []any{}
		for _, oid := range oids[st:end] {
			out = append(out, v.commit(r, oid))
		}
		return jsonResp(200, out, extra)
	}
	ref := strings.Join(s, "/")
	oid := f.resolveCommitish(r, ref)
	if oid == "" {
		return errorJSON(422, "No commit found for SHA: "+ref, nil)
	}
	body := v.commit(r, oid)
	c := f.store.Commit(oid)
	first := ""
	if len(c.Parents) > 0 {
		first = c.Parents[0]
	}
	before := f.filesAt(first)
	after := f.filesAt(oid)
	files := []any{}
	seen := map[string]bool{}
	for _, list := range [][]string{before.Keys, after.Keys} {
		for _, path := range list {
			if seen[path] {
				continue
			}
			seen[path] = true
			a, okA := before.Get(path)
			b, okB := after.Get(path)
			if okA == okB && a.Oid == b.Oid {
				continue
			}
			sha := b.Oid
			if !okB {
				sha = a.Oid
			}
			status := "modified"
			if !okA {
				status = "added"
			} else if !okB {
				status = "removed"
			}
			files = append(files, O("sha", sha, "filename", path, "status", status, "additions", 0, "deletions", 0, "changes", 0))
		}
	}
	body.Set("files", files)
	body.Set("stats", O("total", 0, "additions", 0, "deletions", 0))
	return jsonResp(200, body, "")
}

// ---------------------------------------------------------------- contents

var refsHeadsRe = regexp.MustCompile(`^refs/heads/`)

func wrap60(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	for i := 0; i < len(s); i += 60 {
		j := i + 60
		if j > len(s) {
			b.WriteString(s[i:])
			b.WriteByte('\n')
			break
		}
		b.WriteString(s[i:j])
		b.WriteByte('\n')
	}
	return b.String()
}

func (rt *Rest) contentsRoute(req *Request, r *Repo, rawPath string, params *Params) *Response {
	f := rt.forge
	var parts []string
	for _, p := range strings.Split(rawPath, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	path := strings.Join(parts, "/")
	method := req.Method
	if method == "GET" || method == "HEAD" {
		if r.Refs.Len() == 0 {
			return errorJSON(404, "This repository is empty.", nil)
		}
		refName := params.GetOr("ref", r.DefaultBranch)
		tip := f.resolveCommitish(r, refName)
		if tip == "" {
			return errorJSON(404, "No commit found for the ref "+refName, nil)
		}
		c := f.store.Commit(tip)
		entry := lookupPath(f.store, c.Tree, path)
		if entry == nil {
			return notFound()
		}
		if entry.Mode == "40000" {
			entries, _ := f.store.Tree(entry.Oid)
			out := []any{}
			for _, child := range entries {
				p := child.Name
				if path != "" {
					p = path + "/" + child.Name
				}
				out = append(out, rt.views.contentEntry(r, p, child.Mode, child.Oid, refName))
			}
			return jsonResp(200, out, "")
		}
		var data []byte
		if blob := f.store.Get(entry.Oid); blob != nil {
			data = blob.Data
		}
		base := rt.views.contentEntry(r, path, entry.Mode, entry.Oid, refName)
		base.Set("content", wrap60(base64.StdEncoding.EncodeToString(data)))
		base.Set("encoding", "base64")
		return jsonResp(200, base, "")
	}
	if method == "PUT" || method == "DELETE" {
		if path == "" {
			panic(ferr(422, "Invalid request.\n\n\"path\" wasn't supplied."))
		}
		body := parseBody(req)
		message, ok := body["message"].(string)
		if !ok || message == "" {
			panic(ferr(422, "Invalid request.\n\n\"message\" wasn't supplied."))
		}
		branch := r.DefaultBranch
		if b, ok := body["branch"].(string); ok && b != "" {
			branch = refsHeadsRe.ReplaceAllString(b, "")
		}
		tip, hasTip := f.branchTip(r, branch)
		startFrom := ""
		if hasTip {
			startFrom = tip
		}
		if !hasTip && r.Refs.Len() > 0 {
			def, ok := f.branchTip(r, r.DefaultBranch)
			if !ok {
				return errorJSON(404, "Branch "+branch+" not found", nil)
			}
			startFrom = def
		}
		files := f.filesAt(startFrom)
		existing, hasExisting := files.Get(path)
		author := rt.signatureFrom(body["author"])
		if author == nil {
			author = rt.signatureFrom(body["committer"])
		}
		var parents []string
		if startFrom == "" {
			parents = []string{}
		} else {
			parents = []string{startFrom}
		}
		if method == "DELETE" {
			if !hasExisting {
				return notFound()
			}
			sha, ok := body["sha"].(string)
			if !ok {
				panic(ferr(422, "Invalid request.\n\n\"sha\" wasn't supplied."))
			}
			if sha != existing.Oid {
				return errorJSON(409, path+" does not match "+sha, nil)
			}
			files.Delete(path)
			oid := f.commitTree(r, branch, files, message, parents, author)
			return jsonResp(200, O("content", nil, "commit", rt.views.gitCommit(r, oid)), "")
		}
		content, ok := body["content"].(string)
		if !ok {
			panic(ferr(422, "Invalid request.\n\n\"content\" wasn't supplied."))
		}
		if hasExisting {
			sha, ok := body["sha"].(string)
			if !ok || sha == "" {
				panic(ferr(422, "Invalid request.\n\n\"sha\" wasn't supplied."))
			}
			if sha != existing.Oid {
				return errorJSON(409, path+" does not match "+sha, nil)
			}
		} else {
			treeOid := ""
			if c := f.store.Commit(startFrom); c != nil {
				treeOid = c.Tree
			}
			if e := lookupPath(f.store, treeOid, path); e != nil && e.Mode == "40000" {
				panic(ferr(422, "Invalid request.\n\nA directory exists at this path."))
			}
		}
		blob := f.store.Put(ObjBlob, nodeBase64Decode(content))
		segs := strings.Split(path, "/")
		for i := 1; i < len(segs); i++ {
			files.Delete(strings.Join(segs[:i], "/"))
		}
		for _, key := range append([]string(nil), files.Keys...) {
			if strings.HasPrefix(key, path+"/") {
				files.Delete(key)
			}
		}
		mode := "100644"
		if hasExisting {
			mode = existing.Mode
		}
		files.Set(path, FileEntry{Mode: mode, Oid: blob})
		oid := f.commitTree(r, branch, files, message, parents, author)
		entry := rt.views.contentEntry(r, path, "100644", blob, branch)
		entry.Set("encoding", "base64")
		status := 200
		if !hasExisting {
			status = 201
		}
		return jsonResp(status, O("content", entry, "commit", rt.views.gitCommit(r, oid)), "")
	}
	return notFound()
}

func (rt *Rest) signatureFrom(value any) *Signature {
	m, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	name, ok1 := m["name"].(string)
	email, ok2 := m["email"].(string)
	if !ok1 || !ok2 {
		return nil
	}
	sig := rt.forge.signature()
	return &Signature{Name: name, Email: email, Time: sig.Time, TZ: "+0000"}
}

// ---------------------------------------------------------------- labels

func (rt *Rest) labelsRoute(req *Request, r *Repo, s []string, params *Params) *Response {
	method := req.Method
	v := rt.views
	if len(s) == 0 || (len(s) == 1 && s[0] == "") {
		if method == "GET" {
			labels := r.Labels.Values()
			start, end, extra := rt.paginate(len(labels), req, params)
			out := []any{}
			for _, l := range labels[start:end] {
				out = append(out, v.label(r, l))
			}
			return jsonResp(200, out, extra)
		}
		if method == "POST" {
			body := parseBody(req)
			var color any = Undefined
			if c, ok := body["color"].(string); ok {
				color = c
			}
			var desc any
			if d, ok := body["description"].(string); ok {
				desc = d
			}
			name, ok := body["name"]
			if !ok {
				name = Undefined
			}
			l, err := rt.forge.createLabel(r, name, color, desc)
			must(err)
			return jsonResp(201, v.label(r, l), "")
		}
		return notFound()
	}
	name := strings.Join(s, "/")
	l := r.Labels.Get(strings.ToLower(name))
	if l == nil {
		return notFound()
	}
	switch method {
	case "GET":
		return jsonResp(200, v.label(r, l), "")
	case "PATCH":
		body := parseBody(req)
		if nn, ok := body["new_name"].(string); ok && nn != "" {
			r.Labels.Delete(strings.ToLower(l.Name))
			l.Name = nn
			r.Labels.Set(strings.ToLower(l.Name), l)
		}
		if c, ok := body["color"].(string); ok {
			l.Color = hashPrefixRe.ReplaceAllString(c, "")
		}
		if d, ok := body["description"]; ok {
			l.Description = strOrNil(d)
		}
		rt.forge.touchRepo(r)
		return jsonResp(200, v.label(r, l), "")
	case "DELETE":
		r.Labels.Delete(strings.ToLower(l.Name))
		for _, issue := range r.IssueList {
			kept := issue.Labels[:0:0]
			for _, other := range issue.Labels {
				if other != l {
					kept = append(kept, other)
				}
			}
			issue.Labels = kept
		}
		rt.forge.touchRepo(r)
		return noContent()
	}
	return notFound()
}

// ---------------------------------------------------------------- issues

func sortIssues(list []*Issue, sortKey, direction string) []*Issue {
	key := func(issue *Issue) int64 {
		switch sortKey {
		case "updated":
			return issue.UpdatedAt
		case "comments":
			return int64(len(issue.Comments))
		}
		return issue.CreatedAt
	}
	sorted := append([]*Issue(nil), list...)
	sort.SliceStable(sorted, func(i, j int) bool {
		ki, kj := key(sorted[i]), key(sorted[j])
		if ki != kj {
			return ki < kj
		}
		return sorted[i].Number < sorted[j].Number
	})
	if direction != "asc" {
		for i, j := 0, len(sorted)-1; i < j; i, j = i+1, j-1 {
			sorted[i], sorted[j] = sorted[j], sorted[i]
		}
	}
	return sorted
}

func filterState(list []*Issue, params *Params) []*Issue {
	state := strings.ToLower(params.GetOr("state", "open"))
	if state == "all" {
		return list
	}
	want := "open"
	if state == "closed" {
		want = "closed"
	}
	out := make([]*Issue, 0, len(list))
	for _, issue := range list {
		if issue.State == want {
			out = append(out, issue)
		}
	}
	return out
}

func parseDate(s string) (int64, bool) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli(), true
		}
	}
	return 0, false
}

func (rt *Rest) issuesRoute(req *Request, r *Repo, s []string, params *Params) *Response {
	method := req.Method
	f := rt.forge
	v := rt.views
	if len(s) == 0 || (len(s) == 1 && s[0] == "") {
		if method == "GET" || method == "HEAD" {
			list := filterState(r.IssueList, params)
			if labels, ok := params.Get("labels"); ok && labels != "" {
				var wanted []string
				for _, name := range strings.Split(labels, ",") {
					wanted = append(wanted, strings.ToLower(strings.TrimSpace(name)))
				}
				kept := list[:0:0]
				for _, issue := range list {
					all := true
					for _, w := range wanted {
						found := false
						for _, l := range issue.Labels {
							if strings.ToLower(l.Name) == w {
								found = true
								break
							}
						}
						if !found {
							all = false
							break
						}
					}
					if all {
						kept = append(kept, issue)
					}
				}
				list = kept
			}
			if creator, ok := params.Get("creator"); ok && creator != "" {
				kept := list[:0:0]
				for _, issue := range list {
					if strings.EqualFold(issue.User.Login, creator) {
						kept = append(kept, issue)
					}
				}
				list = kept
			}
			if since, ok := params.Get("since"); ok && since != "" {
				if t, ok := parseDate(since); ok {
					kept := list[:0:0]
					for _, issue := range list {
						if issue.UpdatedAt >= t {
							kept = append(kept, issue)
						}
					}
					list = kept
				}
			}
			sorted := sortIssues(list, params.GetOr("sort", "created"), params.GetOr("direction", "desc"))
			start, end, extra := rt.paginate(len(sorted), req, params)
			items := make([][]byte, 0, end-start)
			for _, issue := range sorted[start:end] {
				items = append(items, v.issue(issue))
			}
			return jsonResp(200, joinJSON(items), extra)
		}
		if method == "POST" {
			if !r.HasIssues {
				return errorJSON(410, "Issues are disabled for this repo", nil)
			}
			body := parseBody(req)
			issue, err := f.createIssue(r, field(body, "title"), field(body, "body"), field(body, "labels"))
			must(err)
			return jsonResp(201, v.issue(issue), "Location: "+v.api+"/repos/"+r.Owner.Login+"/"+r.Name+"/issues/"+strconv.Itoa(issue.Number)+"\r\n")
		}
		return notFound()
	}
	if s[0] == "comments" {
		if len(s) == 1 && method == "GET" {
			var comments []*Comment
			for _, issue := range r.IssueList {
				comments = append(comments, issue.Comments...)
			}
			start, end, extra := rt.paginate(len(comments), req, params)
			out := []any{}
			for _, c := range comments[start:end] {
				out = append(out, v.comment(c))
			}
			return jsonResp(200, out, extra)
		}
		if len(s) == 2 {
			id, _ := jsParseInt(s[1])
			c := f.commentsByID[id]
			if c == nil || c.Issue.Repo != r {
				return notFound()
			}
			switch method {
			case "GET":
				return jsonResp(200, v.comment(c), "")
			case "PATCH":
				body := parseBody(req)
				text, ok := body["body"].(string)
				if !ok || text == "" {
					panic(ferr(422, "Validation Failed", map[string]any{"resource": "IssueComment", "code": "missing_field", "field": "body"}))
				}
				c.Body = text
				c.UpdatedAt = f.now()
				f.touchIssue(c.Issue)
				return jsonResp(200, v.comment(c), "")
			case "DELETE":
				kept := c.Issue.Comments[:0:0]
				for _, other := range c.Issue.Comments {
					if other != c {
						kept = append(kept, other)
					}
				}
				c.Issue.Comments = kept
				delete(f.commentsByID, c.ID)
				f.touchIssue(c.Issue)
				return noContent()
			}
		}
		return notFound()
	}
	number, ok := jsNumberInt(s[0])
	if !ok {
		return notFound()
	}
	issue := r.Issues[number]
	if issue == nil {
		return notFound()
	}
	if len(s) == 1 {
		if method == "GET" || method == "HEAD" {
			return jsonResp(200, v.issue(issue), "")
		}
		if method == "PATCH" {
			body := parseBody(req)
			must(f.updateIssue(issue, issuePatch{Title: field(body, "title"), Body: field(body, "body"), State: field(body, "state"), StateReason: field(body, "state_reason"), Labels: field(body, "labels")}))
			return jsonResp(200, v.issue(issue), "")
		}
		return notFound()
	}
	sub := s[1]
	if sub == "comments" && len(s) == 2 {
		if method == "GET" {
			start, end, extra := rt.paginate(len(issue.Comments), req, params)
			out := []any{}
			for _, c := range issue.Comments[start:end] {
				out = append(out, v.comment(c))
			}
			return jsonResp(200, out, extra)
		}
		if method == "POST" {
			body := parseBody(req)
			c, err := f.addComment(issue, field(body, "body"))
			must(err)
			return jsonResp(201, v.comment(c), "Location: "+v.api+"/repos/"+r.Owner.Login+"/"+r.Name+"/issues/comments/"+strconv.Itoa(c.ID)+"\r\n")
		}
		return notFound()
	}
	if sub == "labels" {
		labelList := func() []any { return v.labelsOf(r, issue.Labels) }
		if len(s) == 2 {
			switch method {
			case "GET":
				return jsonResp(200, labelList(), "")
			case "POST", "PUT":
				body := parseBody(req)
				names, isArr := body["labels"].([]any)
				if !isArr {
					names = []any{}
				}
				labels, err := f.resolveLabels(r, names)
				must(err)
				if method == "PUT" {
					issue.Labels = labels
				} else {
					merged := append([]*Label(nil), issue.Labels...)
					for _, l := range labels {
						dup := false
						for _, x := range issue.Labels {
							if x == l {
								dup = true
							}
						}
						if !dup {
							merged = append(merged, l)
						}
					}
					issue.Labels = merged
				}
				f.touchIssue(issue)
				return jsonResp(200, labelList(), "")
			case "DELETE":
				issue.Labels = []*Label{}
				f.touchIssue(issue)
				return noContent()
			}
		}
		if len(s) == 3 && method == "DELETE" {
			name := strings.ToLower(s[2])
			before := len(issue.Labels)
			kept := issue.Labels[:0:0]
			for _, l := range issue.Labels {
				if strings.ToLower(l.Name) != name {
					kept = append(kept, l)
				}
			}
			issue.Labels = kept
			if len(issue.Labels) == before {
				return errorJSON(404, "Label does not exist", nil)
			}
			f.touchIssue(issue)
			return jsonResp(200, labelList(), "")
		}
		return notFound()
	}
	if sub == "events" || sub == "timeline" {
		if method == "GET" {
			return jsonResp(200, []any{}, "")
		}
		return notFound()
	}
	if sub == "lock" {
		return noContent()
	}
	return notFound()
}

// ---------------------------------------------------------------- pull requests

func (rt *Rest) pullsRoute(req *Request, r *Repo, s []string, params *Params) *Response {
	method := req.Method
	f := rt.forge
	v := rt.views
	if len(s) == 0 || (len(s) == 1 && s[0] == "") {
		if method == "GET" || method == "HEAD" {
			var pulls []*Issue
			for _, issue := range r.IssueList {
				if issue.Pull != nil {
					pulls = append(pulls, issue)
				}
			}
			list := filterState(pulls, params)
			if headFilter, ok := params.Get("head"); ok && headFilter != "" {
				branch := afterColon(headFilter)
				kept := list[:0:0]
				for _, issue := range list {
					if issue.Pull.Head == branch {
						kept = append(kept, issue)
					}
				}
				list = kept
			}
			if baseFilter, ok := params.Get("base"); ok && baseFilter != "" {
				kept := list[:0:0]
				for _, issue := range list {
					if issue.Pull.Base == baseFilter {
						kept = append(kept, issue)
					}
				}
				list = kept
			}
			sortKey := params.GetOr("sort", "created")
			if sortKey == "popularity" || sortKey == "long-running" {
				sortKey = "created"
			}
			sorted := sortIssues(list, sortKey, params.GetOr("direction", "desc"))
			start, end, extra := rt.paginate(len(sorted), req, params)
			items := make([][]byte, 0, end-start)
			for _, issue := range sorted[start:end] {
				items = append(items, v.pull(issue))
			}
			return jsonResp(200, joinJSON(items), extra)
		}
		if method == "POST" {
			body := parseBody(req)
			title := field(body, "title")
			text := field(body, "body")
			if num, ok := body["issue"].(float64); ok {
				if existing := r.Issues[int(num)]; existing != nil && float64(int(num)) == num {
					title = existing.Title
					if existing.Body != nil {
						text = *existing.Body
					} else {
						text = nil
					}
				}
			}
			issue, err := f.createPull(r, title, text, field(body, "head"), field(body, "base"), field(body, "draft"))
			must(err)
			return jsonResp(201, v.pull(issue), "Location: "+v.api+"/repos/"+r.Owner.Login+"/"+r.Name+"/pulls/"+strconv.Itoa(issue.Number)+"\r\n")
		}
		return notFound()
	}
	number, ok := jsNumberInt(s[0])
	if !ok {
		return notFound()
	}
	issue := r.Issues[number]
	if issue == nil || issue.Pull == nil {
		return notFound()
	}
	if len(s) == 1 {
		if method == "GET" || method == "HEAD" {
			return jsonResp(200, v.pull(issue), "")
		}
		if method == "PATCH" {
			body := parseBody(req)
			p := newIssuePatch()
			p.Title, p.Body, p.State = field(body, "title"), field(body, "body"), field(body, "state")
			must(f.updateIssue(issue, p))
			if base, ok := body["base"].(string); ok && base != "" && issue.State == "open" {
				if _, exists := f.branchTip(r, base); !exists {
					panic(ferr(422, "Validation Failed", map[string]any{"resource": "PullRequest", "code": "invalid", "field": "base"}))
				}
				issue.Pull.Base = base
				f.touchIssue(issue)
			}
			return jsonResp(200, v.pull(issue), "")
		}
		return notFound()
	}
	sub := s[1]
	if sub == "files" && method == "GET" {
		repoURL := v.api + "/repos/" + r.Owner.Login + "/" + r.Name
		headSha := f.pullHeadSha(issue)
		files := f.pullFiles(issue)
		start, end, extra := rt.paginate(len(files), req, params)
		out := []any{}
		for _, file := range files[start:end] {
			out = append(out, O(
				"sha", file.Sha,
				"filename", file.Filename,
				"status", file.Status,
				"additions", file.Additions,
				"deletions", file.Deletions,
				"changes", file.Changes,
				"blob_url", v.origin+"/"+r.Owner.Login+"/"+r.Name+"/blob/"+headSha+"/"+file.Filename,
				"raw_url", v.origin+"/"+r.Owner.Login+"/"+r.Name+"/raw/"+headSha+"/"+file.Filename,
				"contents_url", repoURL+"/contents/"+file.Filename+"?ref="+headSha,
			))
		}
		return jsonResp(200, out, extra)
	}
	if sub == "commits" && method == "GET" {
		oids := f.pullCommits(issue)
		start, end, extra := rt.paginate(len(oids), req, params)
		out := []any{}
		for _, oid := range oids[start:end] {
			out = append(out, v.commit(r, oid))
		}
		return jsonResp(200, out, extra)
	}
	if sub == "merge" {
		if method == "GET" {
			if issue.Pull.Merged {
				return noContent()
			}
			return notFound()
		}
		if method == "PUT" || method == "POST" {
			body := parseBody(req)
			sha, err := f.mergePull(issue, field(body, "merge_method"), field(body, "commit_title"), field(body, "commit_message"), field(body, "sha"))
			must(err)
			return jsonResp(200, O("sha", sha, "merged", true, "message", "Pull Request successfully merged"), "")
		}
		return notFound()
	}
	if (sub == "reviews" || sub == "comments" || sub == "requested_reviewers") && method == "GET" {
		if sub == "requested_reviewers" {
			return jsonResp(200, O("users", []any{}, "teams", []any{}), "")
		}
		return jsonResp(200, []any{}, "")
	}
	return notFound()
}
