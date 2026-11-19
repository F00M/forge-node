// GraphQL v4 at /api/graphql: a schema-driven executor over the forge (queries, mutations, Relay node
// ids, connections with cursors, introspection through __type/__schema, batching). Unknown fields fail
// validation with GitHub's "Field 'x' doesn't exist on type 'Y'" error before anything executes.
package main

import (
	"encoding/base64"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------- schema model

type gqlCtx struct {
	forge  *Forge
	origin string
}

type Resolver func(parent any, args map[string]any, ctx *gqlCtx) any

type kv struct{ k, v string }

type FieldDef struct {
	Type        string
	Args        []kv
	Resolve     Resolver
	Description string
}

type FieldMap struct {
	order []string
	m     map[string]*FieldDef
}

func (fm *FieldMap) set(name string, d *FieldDef) {
	if _, ok := fm.m[name]; !ok {
		fm.order = append(fm.order, name)
	}
	fm.m[name] = d
}

func (fm *FieldMap) get(name string) *FieldDef {
	if fm == nil {
		return nil
	}
	return fm.m[name]
}

// fields builds a FieldMap from alternating names and *FieldDef values (later names replace in place).
func fields(pairs ...any) *FieldMap {
	fm := &FieldMap{m: map[string]*FieldDef{}}
	for i := 0; i+1 < len(pairs); i += 2 {
		fm.set(pairs[i].(string), pairs[i+1].(*FieldDef))
	}
	return fm
}

func (fm *FieldMap) merge(other *FieldMap) *FieldMap {
	for _, name := range other.order {
		fm.set(name, other.m[name])
	}
	return fm
}

func F(typ string, resolve Resolver) *FieldDef { return &FieldDef{Type: typ, Resolve: resolve} }

func FA(typ string, args []kv, resolve Resolver) *FieldDef {
	return &FieldDef{Type: typ, Args: args, Resolve: resolve}
}

func T(typ string) *FieldDef { return &FieldDef{Type: typ} }

type TypeDef struct {
	Kind          string
	Name          string
	Description   string
	Fields        *FieldMap
	Interfaces    []string
	PossibleTypes []string
	Values        []string
	InputFields   []kv
}

type typeWrap struct {
	kind   string
	ofType any // *TypeDef, *typeWrap or nil
}

type typedVal struct {
	typ   string
	value any
}

type ResolveError struct {
	Type    string
	Message string
}

func resolveErr(typ, msg string) *ResolveError { return &ResolveError{Type: typ, Message: msg} }

type commitVal struct {
	oid  string
	repo *Repo
}

type refVal struct {
	repo *Repo
	name string
	oid  string
}

type fieldIntro struct {
	name string
	def  *FieldDef
}

type inputValue struct{ name, typ string }

type directiveDef struct {
	name      string
	locations []string
	args      []kv
}

func b64(text string) string { return base64.StdEncoding.EncodeToString([]byte(text)) }

func cursorFor(offset int) string { return b64("cursor:v2:" + strconv.Itoa(offset)) }

var cursorRe = regexp.MustCompile(`^cursor:v2:(\d+)$`)

func offsetOf(cursor any) (float64, bool) {
	s, ok := cursor.(string)
	if !ok || s == "" {
		return 0, false
	}
	m := cursorRe.FindStringSubmatch(string(nodeBase64Decode(s)))
	if m == nil {
		return 0, false
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// connection is a Relay connection over an ordered list, honouring first/last/after/before.
func connection(items []any, args map[string]any) map[string]any {
	n := float64(len(items))
	start, end := 0.0, n
	if after, ok := offsetOf(args["after"]); ok {
		start = math.Min(n, math.Max(0, after))
	}
	if before, ok := offsetOf(args["before"]); ok {
		end = math.Max(start, math.Min(end, before-1))
	}
	if first, ok := args["first"].(float64); ok {
		if first < 0 {
			panic(resolveErr("INVALID_ARGUMENT", "`first` must be a non-negative integer"))
		}
		end = math.Min(end, start+math.Min(first, 100))
	} else if last, ok := args["last"].(float64); ok {
		if last < 0 {
			panic(resolveErr("INVALID_ARGUMENT", "`last` must be a non-negative integer"))
		}
		start = math.Max(start, end-math.Min(last, 100))
	} else {
		end = math.Min(end, start+100)
	}
	s, e := int(start), int(end)
	if e < s {
		e = s
	}
	page := items[s:e]
	nodes := make([]any, len(page))
	edges := make([]any, len(page))
	for i, node := range page {
		nodes[i] = node
		edges[i] = map[string]any{"cursor": cursorFor(s + i + 1), "node": node}
	}
	var startCursor, endCursor any
	if len(edges) > 0 {
		startCursor = edges[0].(map[string]any)["cursor"]
		endCursor = edges[len(edges)-1].(map[string]any)["cursor"]
	}
	return map[string]any{
		"nodes":      nodes,
		"edges":      edges,
		"totalCount": len(items),
		"pageInfo": map[string]any{
			"hasNextPage":     e < len(items),
			"hasPreviousPage": s > 0,
			"startCursor":     startCursor,
			"endCursor":       endCursor,
		},
	}
}

// ---------------------------------------------------------------- domain helpers

func repoURL(ctx *gqlCtx, r *Repo) string { return ctx.origin + "/" + r.Owner.Login + "/" + r.Name }

func ownerTyped(u *User) any {
	if u.Type == "Organization" {
		return &typedVal{"Organization", u}
	}
	return &typedVal{"User", u}
}

func issueState(issue *Issue) string {
	if issue.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func pullState(issue *Issue) string {
	if issue.Pull.Merged {
		return "MERGED"
	}
	return issueState(issue)
}

func sortedIssues(list []*Issue, orderBy any) []any {
	fieldName, direction := "CREATED_AT", "ASC"
	if m, ok := orderBy.(map[string]any); ok {
		if v, ok := m["field"]; ok && v != nil {
			fieldName = jsString(v)
		}
		if v, ok := m["direction"]; ok && v != nil {
			direction = jsString(v)
		}
	}
	key := func(issue *Issue) int64 {
		switch fieldName {
		case "UPDATED_AT":
			return issue.UpdatedAt
		case "COMMENTS":
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
	if direction == "DESC" {
		for i, j := 0, len(sorted)-1; i < j; i, j = i+1, j-1 {
			sorted[i], sorted[j] = sorted[j], sorted[i]
		}
	}
	out := make([]any, len(sorted))
	for i, issue := range sorted {
		out[i] = issue
	}
	return out
}

func lookupNode(ctx *gqlCtx, id any) *typedVal {
	s, ok := id.(string)
	if !ok {
		return nil
	}
	ref := decodeNodeID(s)
	if ref == nil {
		return nil
	}
	f := ctx.forge
	switch ref.Kind {
	case "Repository":
		if r := f.reposByID[ref.ID]; r != nil {
			return &typedVal{"Repository", r}
		}
	case "Issue":
		if issue := f.issuesByID[ref.ID]; issue != nil && !issue.Repo.Deleted {
			return &typedVal{"Issue", issue}
		}
	case "PullRequest":
		if issue := f.pullsByID[ref.ID]; issue != nil && !issue.Repo.Deleted {
			return &typedVal{"PullRequest", issue}
		}
	case "IssueComment":
		if c := f.commentsByID[ref.ID]; c != nil {
			return &typedVal{"IssueComment", c}
		}
	case "User", "Organization":
		for _, u := range f.userOrder {
			if u.ID == ref.ID && u.Type == ref.Kind {
				return &typedVal{ref.Kind, u}
			}
		}
	case "Label":
		if l := f.labelsByID[ref.ID]; l != nil {
			return &typedVal{"Label", l}
		}
	}
	return nil
}

func requireNode(ctx *gqlCtx, id any, kinds ...string) *typedVal {
	node := lookupNode(ctx, id)
	if node == nil || !containsStr(kinds, node.typ) {
		panic(resolveErr("NOT_FOUND", "Could not resolve to a node with the global id of '"+jsString(id)+"'."))
	}
	return node
}

func wrapForge(err error) {
	if err == nil {
		return
	}
	if fe, ok := err.(*ForgeError); ok {
		detail := ""
		if len(fe.Errors) > 0 {
			detail = " " + toJSONString(fe.Errors)
		}
		typ := "UNPROCESSABLE"
		if fe.Status == 404 {
			typ = "NOT_FOUND"
		}
		panic(resolveErr(typ, fe.Message+detail))
	}
	panic(err)
}

func refValue(r *Repo, name string) any {
	oid, ok := r.Refs.Get(name)
	if !ok {
		return nil
	}
	return &refVal{repo: r, name: name, oid: oid}
}

func jsToNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		t := strings.TrimSpace(x)
		if t == "" {
			return 0, true
		}
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case nil:
		return 0, true
	}
	return 0, false
}

func issueByNumber(r *Repo, v any) *Issue {
	n, ok := jsToNumber(v)
	if !ok || n != math.Trunc(n) {
		return nil
	}
	return r.Issues[int(n)]
}

func inputOf(a map[string]any) map[string]any {
	if m, ok := a["input"].(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func orNull(v any) any {
	if isUndef(v) {
		return nil
	}
	return v
}

func nullToUndef(v any) any {
	if v == nil {
		return Undefined
	}
	return v
}

func toAnySlice[T any](list []T) []any {
	out := make([]any, len(list))
	for i, x := range list {
		out[i] = x
	}
	return out
}

func strPtrOrNil(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

var trailingNL = regexp.MustCompile(`\n+$`)

func commitMessage(ctx *gqlCtx, oid string) string {
	if c := ctx.forge.store.Commit(oid); c != nil {
		return c.Message
	}
	return ""
}

// ---------------------------------------------------------------- the schema

var connectionArgs = []kv{{"first", "Int"}, {"last", "Int"}, {"after", "String"}, {"before", "String"}}

func withConn(extra ...kv) []kv { return append(append([]kv(nil), connectionArgs...), extra...) }

func connectionTypes(name, node string) []*TypeDef {
	return []*TypeDef{
		{Kind: "OBJECT", Name: name + "Connection", Fields: fields(
			"nodes", T("["+node+"]"),
			"edges", T("["+name+"Edge]"),
			"pageInfo", T("PageInfo!"),
			"totalCount", T("Int!"),
		)},
		{Kind: "OBJECT", Name: name + "Edge", Fields: fields("cursor", T("String!"), "node", T(node))},
	}
}

func constant(v any) Resolver { return func(any, map[string]any, *gqlCtx) any { return v } }

var (
	schema      map[string]*TypeDef
	schemaOrder []*TypeDef
	directives  []any
)

func init() {
	directives = []any{
		&directiveDef{"include", []string{"FIELD", "FRAGMENT_SPREAD", "INLINE_FRAGMENT"}, []kv{{"if", "Boolean!"}}},
		&directiveDef{"skip", []string{"FIELD", "FRAGMENT_SPREAD", "INLINE_FRAGMENT"}, []kv{{"if", "Boolean!"}}},
		&directiveDef{"deprecated", []string{"FIELD_DEFINITION", "ENUM_VALUE"}, []kv{{"reason", "String"}}},
	}
	buildSchema()
}

func buildSchema() {
	var types []*TypeDef
	add := func(defs ...*TypeDef) { types = append(types, defs...) }

	for _, scalar := range []string{"String", "Int", "Float", "Boolean", "ID", "DateTime", "URI", "GitObjectID", "HTML", "GitTimestamp"} {
		add(&TypeDef{Kind: "SCALAR", Name: scalar})
	}
	enum := func(name string, values ...string) *TypeDef {
		return &TypeDef{Kind: "ENUM", Name: name, Values: values}
	}
	add(
		enum("IssueState", "OPEN", "CLOSED"),
		enum("IssueStateReason", "COMPLETED", "NOT_PLANNED", "REOPENED", "DUPLICATE"),
		enum("IssueClosedStateReason", "COMPLETED", "NOT_PLANNED", "DUPLICATE"),
		enum("PullRequestState", "OPEN", "CLOSED", "MERGED"),
		enum("MergeableState", "MERGEABLE", "CONFLICTING", "UNKNOWN"),
		enum("MergeStateStatus", "BEHIND", "BLOCKED", "CLEAN", "DIRTY", "DRAFT", "HAS_HOOKS", "UNKNOWN", "UNSTABLE"),
		enum("PullRequestMergeMethod", "MERGE", "SQUASH", "REBASE"),
		enum("RepositoryPermission", "ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"),
		enum("RepositoryVisibility", "PRIVATE", "PUBLIC", "INTERNAL"),
		enum("SearchType", "ISSUE", "ISSUE_ADVANCED", "REPOSITORY", "USER", "DISCUSSION"),
		enum("IssueOrderField", "CREATED_AT", "UPDATED_AT", "COMMENTS"),
		enum("IssueCommentOrderField", "UPDATED_AT"),
		enum("OrderDirection", "ASC", "DESC"),
		enum("PatchStatus", "ADDED", "DELETED", "RENAMED", "COPIED", "MODIFIED", "CHANGED"),
		enum("CommentAuthorAssociation", "MEMBER", "OWNER", "MANNEQUIN", "COLLABORATOR", "CONTRIBUTOR", "FIRST_TIME_CONTRIBUTOR", "FIRST_TIMER", "NONE"),
		enum("PullRequestReviewDecision", "CHANGES_REQUESTED", "APPROVED", "REVIEW_REQUIRED"),
		enum("RepositoryAffiliation", "OWNER", "COLLABORATOR", "ORGANIZATION_MEMBER"),
		enum("__TypeKind", "SCALAR", "OBJECT", "INTERFACE", "UNION", "ENUM", "INPUT_OBJECT", "LIST", "NON_NULL"),
	)

	input := func(name string, fields ...kv) *TypeDef {
		return &TypeDef{Kind: "INPUT_OBJECT", Name: name, InputFields: fields}
	}
	add(
		input("IssueOrder", kv{"field", "IssueOrderField!"}, kv{"direction", "OrderDirection!"}),
		input("IssueCommentOrder", kv{"field", "IssueCommentOrderField!"}, kv{"direction", "OrderDirection!"}),
		input("IssueFilters", kv{"states", "[IssueState!]"}, kv{"labels", "[String!]"}, kv{"createdBy", "String"}, kv{"since", "DateTime"}),
		input("CreateIssueInput", kv{"repositoryId", "ID!"}, kv{"title", "String!"}, kv{"body", "String"}, kv{"labelIds", "[ID!]"}, kv{"assigneeIds", "[ID!]"}, kv{"milestoneId", "ID"}, kv{"clientMutationId", "String"}),
		input("CloseIssueInput", kv{"issueId", "ID!"}, kv{"stateReason", "IssueClosedStateReason"}, kv{"clientMutationId", "String"}),
		input("ReopenIssueInput", kv{"issueId", "ID!"}, kv{"clientMutationId", "String"}),
		input("UpdateIssueInput", kv{"id", "ID!"}, kv{"title", "String"}, kv{"body", "String"}, kv{"state", "IssueState"}, kv{"labelIds", "[ID!]"}, kv{"clientMutationId", "String"}),
		input("CreatePullRequestInput", kv{"repositoryId", "ID!"}, kv{"baseRefName", "String!"}, kv{"headRefName", "String!"}, kv{"title", "String!"}, kv{"body", "String"}, kv{"draft", "Boolean"}, kv{"maintainerCanModify", "Boolean"}, kv{"clientMutationId", "String"}),
		input("MergePullRequestInput", kv{"pullRequestId", "ID!"}, kv{"mergeMethod", "PullRequestMergeMethod"}, kv{"commitHeadline", "String"}, kv{"commitBody", "String"}, kv{"expectedHeadOid", "GitObjectID"}, kv{"authorEmail", "String"}, kv{"clientMutationId", "String"}),
		input("ClosePullRequestInput", kv{"pullRequestId", "ID!"}, kv{"clientMutationId", "String"}),
		input("UpdatePullRequestInput", kv{"pullRequestId", "ID!"}, kv{"title", "String"}, kv{"body", "String"}, kv{"baseRefName", "String"}, kv{"clientMutationId", "String"}),
		input("AddCommentInput", kv{"subjectId", "ID!"}, kv{"body", "String!"}, kv{"clientMutationId", "String"}),
	)

	sizeArg := []kv{{"size", "Int"}}
	add(
		&TypeDef{Kind: "INTERFACE", Name: "Node", Fields: fields("id", T("ID!")), PossibleTypes: []string{"Repository", "Issue", "PullRequest", "IssueComment", "User", "Organization", "Label"}},
		&TypeDef{Kind: "INTERFACE", Name: "Actor", Fields: fields("login", T("String!"), "avatarUrl", FA("URI!", sizeArg, nil), "url", T("URI!"), "resourcePath", T("URI!")), PossibleTypes: []string{"User", "Organization"}},
		&TypeDef{Kind: "INTERFACE", Name: "RepositoryOwner", Fields: fields("id", T("ID!"), "login", T("String!"), "avatarUrl", FA("URI!", sizeArg, nil), "url", T("URI!"), "resourcePath", T("URI!")), PossibleTypes: []string{"User", "Organization"}},
		&TypeDef{Kind: "INTERFACE", Name: "GitObject", Fields: fields("id", T("ID!"), "oid", T("GitObjectID!"), "abbreviatedOid", T("String!"), "commitUrl", T("URI!")), PossibleTypes: []string{"Commit"}},
		&TypeDef{Kind: "UNION", Name: "IssueOrPullRequest", PossibleTypes: []string{"Issue", "PullRequest"}},
	)

	actorFields := func(kind string) *FieldMap {
		emailType := "String"
		if kind == "User" {
			emailType = "String!"
		}
		return fields(
			"id", F("ID!", func(p any, _ map[string]any, _ *gqlCtx) any { return p.(*User).NodeID }),
			"databaseId", F("Int", func(p any, _ map[string]any, _ *gqlCtx) any { return p.(*User).ID }),
			"login", F("String!", func(p any, _ map[string]any, _ *gqlCtx) any { return p.(*User).Login }),
			"name", F("String", func(p any, _ map[string]any, _ *gqlCtx) any { return p.(*User).Name }),
			"email", F(emailType, func(p any, _ map[string]any, _ *gqlCtx) any { return p.(*User).Email }),
			"avatarUrl", FA("URI!", sizeArg, func(p any, _ map[string]any, c *gqlCtx) any { return c.origin + "/avatars/" + p.(*User).Login }),
			"url", F("URI!", func(p any, _ map[string]any, c *gqlCtx) any { return c.origin + "/" + p.(*User).Login }),
			"resourcePath", F("URI!", func(p any, _ map[string]any, _ *gqlCtx) any { return "/" + p.(*User).Login }),
			"createdAt", F("DateTime!", constant("2026-01-01T00:00:00Z")),
			"bio", F("String", constant(nil)),
			"company", F("String", constant(nil)),
			"location", F("String", constant(nil)),
			"isViewer", F("Boolean!", func(p any, _ map[string]any, c *gqlCtx) any { return p.(*User) == c.forge.viewer }),
			"repository", FA("Repository", []kv{{"name", "String!"}}, func(p any, a map[string]any, c *gqlCtx) any {
				if r := c.forge.repo(p.(*User).Login, jsString(orUndef(a, "name"))); r != nil {
					return r
				}
				return nil
			}),
			"repositories", FA("RepositoryConnection!", withConn(kv{"privacy", "RepositoryPrivacy"}, kv{"ownerAffiliations", "[RepositoryAffiliation]"}, kv{"orderBy", "RepositoryOrder"}), func(p any, a map[string]any, c *gqlCtx) any {
				var list []any
				for _, r := range c.forge.liveRepos() {
					if r.Owner == p.(*User) {
						list = append(list, r)
					}
				}
				return connection(list, a)
			}),
		)
	}

	commitOf := func(p any) *commitVal { return p.(*commitVal) }
	add(
		&TypeDef{Kind: "OBJECT", Name: "User", Interfaces: []string{"Node", "Actor", "RepositoryOwner"}, Fields: actorFields("User")},
		&TypeDef{Kind: "OBJECT", Name: "Organization", Interfaces: []string{"Node", "Actor", "RepositoryOwner"}, Fields: actorFields("Organization")},
		enum("RepositoryPrivacy", "PUBLIC", "PRIVATE"),
		input("RepositoryOrder", kv{"field", "String!"}, kv{"direction", "OrderDirection!"}),
		&TypeDef{Kind: "OBJECT", Name: "PageInfo", Fields: fields("hasNextPage", T("Boolean!"), "hasPreviousPage", T("Boolean!"), "startCursor", T("String"), "endCursor", T("String"))},
		&TypeDef{Kind: "OBJECT", Name: "RateLimit", Fields: fields("limit", T("Int!"), "cost", T("Int!"), "remaining", T("Int!"), "resetAt", T("DateTime!"), "used", T("Int!"), "nodeCount", T("Int!"))},
		&TypeDef{Kind: "OBJECT", Name: "Language", Fields: fields("name", T("String!"), "color", T("String"))},
		&TypeDef{Kind: "OBJECT", Name: "License", Fields: fields("name", T("String!"), "key", T("String!"), "spdxId", T("String"))},
		&TypeDef{Kind: "OBJECT", Name: "Label", Interfaces: []string{"Node"}, Fields: fields(
			"id", F("ID!", func(p any, _ map[string]any, _ *gqlCtx) any { return p.(*Label).NodeID }),
			"name", F("String!", func(p any, _ map[string]any, _ *gqlCtx) any { return p.(*Label).Name }),
			"color", F("String!", func(p any, _ map[string]any, _ *gqlCtx) any { return p.(*Label).Color }),
			"description", F("String", func(p any, _ map[string]any, _ *gqlCtx) any { return strPtrOrNil(p.(*Label).Description) }),
			"isDefault", F("Boolean!", constant(false)),
		)},
		&TypeDef{Kind: "OBJECT", Name: "Commit", Interfaces: []string{"Node", "GitObject"}, Fields: fields(
			"id", F("ID!", func(p any, _ map[string]any, _ *gqlCtx) any {
				return "C_" + base64.RawURLEncoding.EncodeToString([]byte("0:Commit:"+commitOf(p).oid))
			}),
			"oid", F("GitObjectID!", func(p any, _ map[string]any, _ *gqlCtx) any { return commitOf(p).oid }),
			"abbreviatedOid", F("String!", func(p any, _ map[string]any, _ *gqlCtx) any { return slice(commitOf(p).oid, 0, 7) }),
			"commitUrl", F("URI!", func(p any, _ map[string]any, c *gqlCtx) any { return repoURL(c, commitOf(p).repo) + "/commit/" + commitOf(p).oid }),
			"url", F("URI!", func(p any, _ map[string]any, c *gqlCtx) any { return repoURL(c, commitOf(p).repo) + "/commit/" + commitOf(p).oid }),
			"message", F("String!", func(p any, _ map[string]any, c *gqlCtx) any {
				return trailingNL.ReplaceAllString(commitMessage(c, commitOf(p).oid), "")
			}),
			"messageHeadline", F("String!", func(p any, _ map[string]any, c *gqlCtx) any {
				return strings.SplitN(commitMessage(c, commitOf(p).oid), "\n", 2)[0]
			}),
			"messageBody", F("String!", func(p any, _ map[string]any, c *gqlCtx) any {
				lines := strings.Split(commitMessage(c, commitOf(p).oid), "\n")
				if len(lines) <= 2 {
					return ""
				}
				return trailingNL.ReplaceAllString(strings.Join(lines[2:], "\n"), "")
			}),
			"committedDate", F("DateTime!", func(p any, _ map[string]any, c *gqlCtx) any {
				var t int64
				if cm := c.forge.store.Commit(commitOf(p).oid); cm != nil {
					t = cm.Committer.Time
				}
				return iso(t * 1000)
			}),
			"authoredDate", F("DateTime!", func(p any, _ map[string]any, c *gqlCtx) any {
				var t int64
				if cm := c.forge.store.Commit(commitOf(p).oid); cm != nil {
					t = cm.Author.Time
				}
				return iso(t * 1000)
			}),
			"parents", FA("CommitConnection!", connectionArgs, func(p any, a map[string]any, c *gqlCtx) any {
				cv := commitOf(p)
				var list []any
				if cm := c.forge.store.Commit(cv.oid); cm != nil {
					for _, oid := range cm.Parents {
						list = append(list, &commitVal{oid: oid, repo: cv.repo})
					}
				}
				return connection(list, a)
			}),
		)},
	)
	add(connectionTypes("Commit", "Commit")...)
	refOf := func(p any) *refVal { return p.(*refVal) }
	add(&TypeDef{Kind: "OBJECT", Name: "Ref", Interfaces: []string{"Node"}, Fields: fields(
		"id", F("ID!", func(p any, _ map[string]any, _ *gqlCtx) any {
			r := refOf(p)
			return "REF_" + base64.RawURLEncoding.EncodeToString([]byte("0:Ref:"+strconv.Itoa(r.repo.ID)+":"+r.name))
		}),
		"name", F("String!", func(p any, _ map[string]any, _ *gqlCtx) any {
			name := refOf(p).name
			if strings.HasPrefix(name, "refs/heads/") {
				return name[11:]
			}
			if strings.HasPrefix(name, "refs/tags/") {
				return name[10:]
			}
			return name
		}),
		"prefix", F("String!", func(p any, _ map[string]any, _ *gqlCtx) any {
			if strings.HasPrefix(refOf(p).name, "refs/tags/") {
				return "refs/tags/"
			}
			return "refs/heads/"
		}),
		"target", F("GitObject", func(p any, _ map[string]any, c *gqlCtx) any {
			r := refOf(p)
			return &typedVal{"Commit", &commitVal{oid: c.forge.store.Peel(r.oid), repo: r.repo}}
		}),
		"repository", F("Repository!", func(p any, _ map[string]any, _ *gqlCtx) any { return refOf(p).repo }),
	)})
	add(connectionTypes("Ref", "Ref")...)

	iss := func(p any) *Issue { return p.(*Issue) }
	issueLike := fields(
		"databaseId", F("Int", func(p any, _ map[string]any, _ *gqlCtx) any { return iss(p).ID }),
		"number", F("Int!", func(p any, _ map[string]any, _ *gqlCtx) any { return iss(p).Number }),
		"title", F("String!", func(p any, _ map[string]any, _ *gqlCtx) any { return iss(p).Title }),
		"body", F("String!", func(p any, _ map[string]any, _ *gqlCtx) any { return derefOr(iss(p).Body, "") }),
		"bodyText", F("String!", func(p any, _ map[string]any, _ *gqlCtx) any { return derefOr(iss(p).Body, "") }),
		"closed", F("Boolean!", func(p any, _ map[string]any, _ *gqlCtx) any { return iss(p).State == "closed" }),
		"closedAt", F("DateTime", func(p any, _ map[string]any, _ *gqlCtx) any { return isoOrNull(iss(p).ClosedAt) }),
		"createdAt", F("DateTime!", func(p any, _ map[string]any, _ *gqlCtx) any { return iso(iss(p).CreatedAt) }),
		"updatedAt", F("DateTime!", func(p any, _ map[string]any, _ *gqlCtx) any { return iso(iss(p).UpdatedAt) }),
		"author", F("Actor", func(p any, _ map[string]any, _ *gqlCtx) any { return ownerTyped(iss(p).User) }),
		"authorAssociation", F("CommentAuthorAssociation!", constant("OWNER")),
		"locked", F("Boolean!", constant(false)),
		"repository", F("Repository!", func(p any, _ map[string]any, _ *gqlCtx) any { return iss(p).Repo }),
		"labels", FA("LabelConnection", connectionArgs, func(p any, a map[string]any, _ *gqlCtx) any {
			return connection(toAnySlice(iss(p).Labels), a)
		}),
		"comments", FA("IssueCommentConnection!", withConn(kv{"orderBy", "IssueCommentOrder"}), func(p any, a map[string]any, _ *gqlCtx) any {
			return connection(toAnySlice(iss(p).Comments), a)
		}),
		"assignees", FA("UserConnection!", connectionArgs, func(_ any, a map[string]any, _ *gqlCtx) any { return connection(nil, a) }),
		"viewerCanUpdate", F("Boolean!", constant(true)),
		"viewerDidAuthor", F("Boolean!", func(p any, _ map[string]any, c *gqlCtx) any { return iss(p).User == c.forge.viewer }),
	)

	issueFields := fields("id", F("ID!", func(p any, _ map[string]any, _ *gqlCtx) any { return iss(p).NodeID })).merge(issueLike).merge(fields(
		"state", F("IssueState!", func(p any, _ map[string]any, _ *gqlCtx) any { return issueState(iss(p)) }),
		"stateReason", F("IssueStateReason", func(p any, _ map[string]any, _ *gqlCtx) any {
			if r := iss(p).StateReason; r != nil {
				return strings.ToUpper(*r)
			}
			return nil
		}),
		"url", F("URI!", func(p any, _ map[string]any, c *gqlCtx) any {
			return repoURL(c, iss(p).Repo) + "/issues/" + strconv.Itoa(iss(p).Number)
		}),
		"isPinned", F("Boolean", constant(false)),
	))
	pullFiles := func(c *gqlCtx, p any) []FileChange { return c.forge.pullFiles(iss(p)) }
	pullFields := fields("id", F("ID!", func(p any, _ map[string]any, _ *gqlCtx) any { return iss(p).Pull.NodeID })).merge(issueLike).merge(fields(
		"databaseId", F("Int", func(p any, _ map[string]any, _ *gqlCtx) any { return iss(p).Pull.ID }),
		"state", F("PullRequestState!", func(p any, _ map[string]any, _ *gqlCtx) any { return pullState(iss(p)) }),
		"url", F("URI!", func(p any, _ map[string]any, c *gqlCtx) any {
			return repoURL(c, iss(p).Repo) + "/pull/" + strconv.Itoa(iss(p).Number)
		}),
		"permalink", F("URI!", func(p any, _ map[string]any, c *gqlCtx) any {
			return repoURL(c, iss(p).Repo) + "/pull/" + strconv.Itoa(iss(p).Number)
		}),
		"merged", F("Boolean!", func(p any, _ map[string]any, _ *gqlCtx) any { return iss(p).Pull.Merged }),
		"mergedAt", F("DateTime", func(p any, _ map[string]any, _ *gqlCtx) any { return isoOrNull(iss(p).Pull.MergedAt) }),
		"mergedBy", F("Actor", func(p any, _ map[string]any, _ *gqlCtx) any {
			if u := iss(p).Pull.MergedBy; u != nil {
				return ownerTyped(u)
			}
			return nil
		}),
		"mergeCommit", F("Commit", func(p any, _ map[string]any, _ *gqlCtx) any {
			if sha := iss(p).Pull.MergeCommitSha; sha != "" {
				return &commitVal{oid: sha, repo: iss(p).Repo}
			}
			return nil
		}),
		"isDraft", F("Boolean!", func(p any, _ map[string]any, _ *gqlCtx) any { return iss(p).Pull.Draft }),
		"isCrossRepository", F("Boolean!", constant(false)),
		"maintainerCanModify", F("Boolean!", constant(false)),
		"headRefName", F("String!", func(p any, _ map[string]any, _ *gqlCtx) any { return iss(p).Pull.Head }),
		"baseRefName", F("String!", func(p any, _ map[string]any, _ *gqlCtx) any { return iss(p).Pull.Base }),
		"headRefOid", F("GitObjectID!", func(p any, _ map[string]any, c *gqlCtx) any { return c.forge.pullHeadSha(iss(p)) }),
		"baseRefOid", F("GitObjectID!", func(p any, _ map[string]any, c *gqlCtx) any { return c.forge.pullBaseSha(iss(p)) }),
		"headRef", F("Ref", func(p any, _ map[string]any, _ *gqlCtx) any { return refValue(iss(p).Repo, "refs/heads/"+iss(p).Pull.Head) }),
		"baseRef", F("Ref", func(p any, _ map[string]any, _ *gqlCtx) any { return refValue(iss(p).Repo, "refs/heads/"+iss(p).Pull.Base) }),
		"headRepository", F("Repository", func(p any, _ map[string]any, _ *gqlCtx) any { return iss(p).Repo }),
		"baseRepository", F("Repository", func(p any, _ map[string]any, _ *gqlCtx) any { return iss(p).Repo }),
		"mergeable", F("MergeableState!", func(p any, _ map[string]any, c *gqlCtx) any {
			i := iss(p)
			if i.Pull.Merged || i.State != "open" {
				return "UNKNOWN"
			}
			if c.forge.mergeable(i) {
				return "MERGEABLE"
			}
			return "CONFLICTING"
		}),
		"mergeStateStatus", F("MergeStateStatus!", func(p any, _ map[string]any, c *gqlCtx) any {
			i := iss(p)
			if i.Pull.Merged || i.State != "open" {
				return "UNKNOWN"
			}
			if c.forge.mergeable(i) {
				return "CLEAN"
			}
			return "DIRTY"
		}),
		"reviewDecision", F("PullRequestReviewDecision", constant(nil)),
		"additions", F("Int!", func(p any, _ map[string]any, c *gqlCtx) any {
			sum := 0
			for _, f := range pullFiles(c, p) {
				sum += f.Additions
			}
			return sum
		}),
		"deletions", F("Int!", func(p any, _ map[string]any, c *gqlCtx) any {
			sum := 0
			for _, f := range pullFiles(c, p) {
				sum += f.Deletions
			}
			return sum
		}),
		"changedFiles", F("Int!", func(p any, _ map[string]any, c *gqlCtx) any { return len(pullFiles(c, p)) }),
		"files", FA("PullRequestChangedFileConnection", connectionArgs, func(p any, a map[string]any, c *gqlCtx) any {
			var list []any
			for _, f := range pullFiles(c, p) {
				change := "MODIFIED"
				if f.Status == "added" {
					change = "ADDED"
				} else if f.Status == "removed" {
					change = "DELETED"
				}
				list = append(list, map[string]any{"path": f.Filename, "additions": f.Additions, "deletions": f.Deletions, "changeType": change})
			}
			return connection(list, a)
		}),
		"commits", FA("PullRequestCommitConnection!", connectionArgs, func(p any, a map[string]any, c *gqlCtx) any {
			var list []any
			for _, oid := range c.forge.pullCommits(iss(p)) {
				list = append(list, map[string]any{"commit": &commitVal{oid: oid, repo: iss(p).Repo}, "id": "PRC_" + oid})
			}
			return connection(list, a)
		}),
	))
	cmt := func(p any) *Comment { return p.(*Comment) }
	add(
		&TypeDef{Kind: "OBJECT", Name: "Issue", Interfaces: []string{"Node"}, Fields: issueFields},
		&TypeDef{Kind: "OBJECT", Name: "PullRequest", Interfaces: []string{"Node"}, Fields: pullFields},
		&TypeDef{Kind: "OBJECT", Name: "PullRequestChangedFile", Fields: fields("path", T("String!"), "additions", T("Int!"), "deletions", T("Int!"), "changeType", T("PatchStatus!"))},
		&TypeDef{Kind: "OBJECT", Name: "PullRequestCommit", Fields: fields("id", T("ID!"), "commit", T("Commit!"))},
		&TypeDef{Kind: "OBJECT", Name: "IssueComment", Interfaces: []string{"Node"}, Fields: fields(
			"id", F("ID!", func(p any, _ map[string]any, _ *gqlCtx) any { return cmt(p).NodeID }),
			"databaseId", F("Int", func(p any, _ map[string]any, _ *gqlCtx) any { return cmt(p).ID }),
			"body", F("String!", func(p any, _ map[string]any, _ *gqlCtx) any { return cmt(p).Body }),
			"bodyText", F("String!", func(p any, _ map[string]any, _ *gqlCtx) any { return cmt(p).Body }),
			"createdAt", F("DateTime!", func(p any, _ map[string]any, _ *gqlCtx) any { return iso(cmt(p).CreatedAt) }),
			"updatedAt", F("DateTime!", func(p any, _ map[string]any, _ *gqlCtx) any { return iso(cmt(p).UpdatedAt) }),
			"author", F("Actor", func(p any, _ map[string]any, _ *gqlCtx) any { return ownerTyped(cmt(p).User) }),
			"authorAssociation", F("CommentAuthorAssociation!", constant("OWNER")),
			"url", F("URI!", func(p any, _ map[string]any, c *gqlCtx) any {
				cm := cmt(p)
				return repoURL(c, cm.Issue.Repo) + "/issues/" + strconv.Itoa(cm.Issue.Number) + "#issuecomment-" + strconv.Itoa(cm.ID)
			}),
			"issue", F("Issue!", func(p any, _ map[string]any, _ *gqlCtx) any { return cmt(p).Issue }),
			"viewerDidAuthor", F("Boolean!", func(p any, _ map[string]any, c *gqlCtx) any { return cmt(p).User == c.forge.viewer }),
		)},
	)
	for _, name := range []string{"Issue", "PullRequest", "IssueComment", "Label", "Repository", "User", "PullRequestChangedFile", "PullRequestCommit"} {
		add(connectionTypes(name, name)...)
	}

	repo := func(p any) *Repo { return p.(*Repo) }
	add(&TypeDef{Kind: "OBJECT", Name: "Repository", Interfaces: []string{"Node"}, Fields: fields(
		"id", F("ID!", func(p any, _ map[string]any, _ *gqlCtx) any { return repo(p).NodeID }),
		"databaseId", F("Int", func(p any, _ map[string]any, _ *gqlCtx) any { return repo(p).ID }),
		"name", F("String!", func(p any, _ map[string]any, _ *gqlCtx) any { return repo(p).Name }),
		"nameWithOwner", F("String!", func(p any, _ map[string]any, _ *gqlCtx) any { return repo(p).Owner.Login + "/" + repo(p).Name }),
		"owner", F("RepositoryOwner!", func(p any, _ map[string]any, _ *gqlCtx) any { return ownerTyped(repo(p).Owner) }),
		"description", F("String", func(p any, _ map[string]any, _ *gqlCtx) any { return strPtrOrNil(repo(p).Description) }),
		"homepageUrl", F("URI", func(p any, _ map[string]any, _ *gqlCtx) any { return strPtrOrNil(repo(p).Homepage) }),
		"url", F("URI!", func(p any, _ map[string]any, c *gqlCtx) any { return repoURL(c, repo(p)) }),
		"sshUrl", F("String!", func(p any, _ map[string]any, _ *gqlCtx) any {
			return "git@localhost:" + repo(p).Owner.Login + "/" + repo(p).Name + ".git"
		}),
		"isPrivate", F("Boolean!", func(p any, _ map[string]any, _ *gqlCtx) any { return repo(p).Private }),
		"visibility", F("RepositoryVisibility!", func(p any, _ map[string]any, _ *gqlCtx) any {
			if repo(p).Private {
				return "PRIVATE"
			}
			return "PUBLIC"
		}),
		"isFork", F("Boolean!", constant(false)),
		"isArchived", F("Boolean!", func(p any, _ map[string]any, _ *gqlCtx) any { return repo(p).Archived }),
		"isDisabled", F("Boolean!", constant(false)),
		"isTemplate", F("Boolean!", constant(false)),
		"isMirror", F("Boolean!", constant(false)),
		"isLocked", F("Boolean!", constant(false)),
		"isEmpty", F("Boolean!", func(p any, _ map[string]any, _ *gqlCtx) any { return repo(p).Refs.Len() == 0 }),
		"createdAt", F("DateTime!", func(p any, _ map[string]any, _ *gqlCtx) any { return iso(repo(p).CreatedAt) }),
		"updatedAt", F("DateTime!", func(p any, _ map[string]any, _ *gqlCtx) any { return iso(repo(p).UpdatedAt) }),
		"pushedAt", F("DateTime", func(p any, _ map[string]any, _ *gqlCtx) any { return iso(repo(p).PushedAt) }),
		"defaultBranchRef", F("Ref", func(p any, _ map[string]any, _ *gqlCtx) any {
			return refValue(repo(p), "refs/heads/"+repo(p).DefaultBranch)
		}),
		"viewerPermission", F("RepositoryPermission", constant("ADMIN")),
		"viewerCanAdminister", F("Boolean!", constant(true)),
		"viewerHasStarred", F("Boolean!", constant(false)),
		"hasIssuesEnabled", F("Boolean!", func(p any, _ map[string]any, _ *gqlCtx) any { return repo(p).HasIssues }),
		"hasWikiEnabled", F("Boolean!", func(p any, _ map[string]any, _ *gqlCtx) any { return repo(p).HasWiki }),
		"hasProjectsEnabled", F("Boolean!", func(p any, _ map[string]any, _ *gqlCtx) any { return repo(p).HasProjects }),
		"hasDiscussionsEnabled", F("Boolean!", constant(false)),
		"mergeCommitAllowed", F("Boolean!", constant(true)),
		"squashMergeAllowed", F("Boolean!", constant(true)),
		"rebaseMergeAllowed", F("Boolean!", constant(true)),
		"deleteBranchOnMerge", F("Boolean!", constant(false)),
		"stargazerCount", F("Int!", constant(0)),
		"forkCount", F("Int!", constant(0)),
		"diskUsage", F("Int", constant(0)),
		"primaryLanguage", F("Language", constant(nil)),
		"licenseInfo", F("License", constant(nil)),
		"parent", F("Repository", constant(nil)),
		"issue", FA("Issue", []kv{{"number", "Int!"}}, func(p any, a map[string]any, _ *gqlCtx) any {
			issue := issueByNumber(repo(p), orUndef(a, "number"))
			if issue == nil || issue.Pull != nil {
				return nil
			}
			return issue
		}),
		"issues", FA("IssueConnection!", withConn(kv{"states", "[IssueState!]"}, kv{"orderBy", "IssueOrder"}, kv{"labels", "[String!]"}, kv{"filterBy", "IssueFilters"}), func(p any, a map[string]any, _ *gqlCtx) any {
			var list []*Issue
			for _, issue := range repo(p).IssueList {
				if issue.Pull == nil {
					list = append(list, issue)
				}
			}
			filterBy, _ := a["filterBy"].(map[string]any)
			pickList := func(name string) []any {
				if l, ok := a[name].([]any); ok {
					return l
				}
				if filterBy != nil {
					if l, ok := filterBy[name].([]any); ok {
						return l
					}
				}
				return nil
			}
			if states := pickList("states"); len(states) > 0 {
				kept := list[:0:0]
				for _, issue := range list {
					if containsAny(states, issueState(issue)) {
						kept = append(kept, issue)
					}
				}
				list = kept
			}
			if labels := pickList("labels"); len(labels) > 0 {
				kept := list[:0:0]
				for _, issue := range list {
					for _, l := range issue.Labels {
						if containsAny(labels, l.Name) {
							kept = append(kept, issue)
							break
						}
					}
				}
				list = kept
			}
			return connection(sortedIssues(list, a["orderBy"]), a)
		}),
		"pullRequest", FA("PullRequest", []kv{{"number", "Int!"}}, func(p any, a map[string]any, _ *gqlCtx) any {
			issue := issueByNumber(repo(p), orUndef(a, "number"))
			if issue == nil || issue.Pull == nil {
				return nil
			}
			return issue
		}),
		"pullRequests", FA("PullRequestConnection!", withConn(kv{"states", "[PullRequestState!]"}, kv{"orderBy", "IssueOrder"}, kv{"headRefName", "String"}, kv{"baseRefName", "String"}, kv{"labels", "[String!]"}), func(p any, a map[string]any, _ *gqlCtx) any {
			var list []*Issue
			for _, issue := range repo(p).IssueList {
				if issue.Pull != nil {
					list = append(list, issue)
				}
			}
			if states, ok := a["states"].([]any); ok && len(states) > 0 {
				kept := list[:0:0]
				for _, issue := range list {
					if containsAny(states, pullState(issue)) {
						kept = append(kept, issue)
					}
				}
				list = kept
			}
			if head, ok := a["headRefName"].(string); ok {
				kept := list[:0:0]
				for _, issue := range list {
					if issue.Pull.Head == head {
						kept = append(kept, issue)
					}
				}
				list = kept
			}
			if base, ok := a["baseRefName"].(string); ok {
				kept := list[:0:0]
				for _, issue := range list {
					if issue.Pull.Base == base {
						kept = append(kept, issue)
					}
				}
				list = kept
			}
			return connection(sortedIssues(list, a["orderBy"]), a)
		}),
		"issueOrPullRequest", FA("IssueOrPullRequest", []kv{{"number", "Int!"}}, func(p any, a map[string]any, _ *gqlCtx) any {
			issue := issueByNumber(repo(p), orUndef(a, "number"))
			if issue == nil {
				return nil
			}
			if issue.Pull == nil {
				return &typedVal{"Issue", issue}
			}
			return &typedVal{"PullRequest", issue}
		}),
		"ref", FA("Ref", []kv{{"qualifiedName", "String!"}}, func(p any, a map[string]any, _ *gqlCtx) any {
			r := repo(p)
			name := jsString(orUndef(a, "qualifiedName"))
			full := name
			if !strings.HasPrefix(name, "refs/") {
				if r.Refs.Has("refs/heads/" + name) {
					full = "refs/heads/" + name
				} else {
					full = "refs/tags/" + name
				}
			}
			return refValue(r, full)
		}),
		"refs", FA("RefConnection", withConn(kv{"refPrefix", "String!"}, kv{"query", "String"}), func(p any, a map[string]any, _ *gqlCtx) any {
			r := repo(p)
			prefix := "refs/"
			if v, ok := a["refPrefix"]; ok && v != nil {
				prefix = jsString(v)
			}
			var list []any
			for _, name := range sortedRefNames(r) {
				if strings.HasPrefix(name, prefix) {
					oid, _ := r.Refs.Get(name)
					list = append(list, &refVal{repo: r, name: name, oid: oid})
				}
			}
			return connection(list, a)
		}),
		"labels", FA("LabelConnection", withConn(kv{"query", "String"}), func(p any, a map[string]any, _ *gqlCtx) any {
			return connection(toAnySlice(repo(p).Labels.Values()), a)
		}),
		"label", FA("Label", []kv{{"name", "String!"}}, func(p any, a map[string]any, _ *gqlCtx) any {
			if l := repo(p).Labels.Get(strings.ToLower(jsString(orUndef(a, "name")))); l != nil {
				return l
			}
			return nil
		}),
		"assignableUsers", FA("UserConnection!", connectionArgs, func(p any, a map[string]any, _ *gqlCtx) any {
			return connection([]any{repo(p).Owner}, a)
		}),
	)})

	payload := func(name string, pairs ...any) *TypeDef {
		return &TypeDef{Kind: "OBJECT", Name: name, Fields: fields(pairs...)}
	}
	add(
		payload("CreateIssuePayload", "issue", T("Issue"), "clientMutationId", T("String")),
		payload("CloseIssuePayload", "issue", T("Issue"), "clientMutationId", T("String")),
		payload("ReopenIssuePayload", "issue", T("Issue"), "clientMutationId", T("String")),
		payload("UpdateIssuePayload", "issue", T("Issue"), "clientMutationId", T("String")),
		payload("CreatePullRequestPayload", "pullRequest", T("PullRequest"), "clientMutationId", T("String")),
		payload("MergePullRequestPayload", "pullRequest", T("PullRequest"), "actor", T("Actor"), "clientMutationId", T("String")),
		payload("ClosePullRequestPayload", "pullRequest", T("PullRequest"), "clientMutationId", T("String")),
		payload("UpdatePullRequestPayload", "pullRequest", T("PullRequest"), "clientMutationId", T("String")),
		payload("AddCommentPayload", "commentEdge", T("IssueCommentEdge"), "subject", T("Node"), "timelineEdge", T("IssueCommentEdge"), "clientMutationId", T("String")),
	)

	userByType := func(c *gqlCtx, a map[string]any, typ string) any {
		u := c.forge.user(jsString(orUndef(a, "login")))
		if u != nil && u.Type == typ {
			return u
		}
		return nil
	}
	add(&TypeDef{Kind: "OBJECT", Name: "Query", Fields: fields(
		"viewer", F("User!", func(_ any, _ map[string]any, c *gqlCtx) any { return c.forge.viewer }),
		"repository", FA("Repository", []kv{{"owner", "String!"}, {"name", "String!"}, {"followRenames", "Boolean"}}, func(_ any, a map[string]any, c *gqlCtx) any {
			owner, name := "", ""
			if v, ok := a["owner"]; ok && v != nil {
				owner = jsString(v)
			}
			if v, ok := a["name"]; ok && v != nil {
				name = jsString(v)
			}
			if r := c.forge.visibleRepo(owner, name); r != nil {
				return r
			}
			return nil
		}),
		"user", FA("User", []kv{{"login", "String!"}}, func(_ any, a map[string]any, c *gqlCtx) any { return userByType(c, a, "User") }),
		"organization", FA("Organization", []kv{{"login", "String!"}}, func(_ any, a map[string]any, c *gqlCtx) any {
			return userByType(c, a, "Organization")
		}),
		"repositoryOwner", FA("RepositoryOwner", []kv{{"login", "String!"}}, func(_ any, a map[string]any, c *gqlCtx) any {
			if u := c.forge.user(jsString(orUndef(a, "login"))); u != nil {
				return ownerTyped(u)
			}
			return nil
		}),
		"node", FA("Node", []kv{{"id", "ID!"}}, func(_ any, a map[string]any, c *gqlCtx) any {
			if n := lookupNode(c, a["id"]); n != nil {
				return n
			}
			return nil
		}),
		"nodes", FA("[Node]!", []kv{{"ids", "[ID!]!"}}, func(_ any, a map[string]any, c *gqlCtx) any {
			ids, ok := a["ids"].([]any)
			if !ok {
				return []any{}
			}
			out := make([]any, len(ids))
			for i, id := range ids {
				if n := lookupNode(c, id); n != nil {
					out[i] = n
				}
			}
			return out
		}),
		"rateLimit", FA("RateLimit", []kv{{"dryRun", "Boolean"}}, func(any, map[string]any, *gqlCtx) any {
			return map[string]any{"limit": 5000, "cost": 1, "remaining": 4999, "used": 1, "nodeCount": 1, "resetAt": iso(time.Now().UnixMilli() + 3600000)}
		}),
		"__type", FA("__Type", []kv{{"name", "String!"}}, func(_ any, a map[string]any, _ *gqlCtx) any {
			if t := schema[jsString(orUndef(a, "name"))]; t != nil {
				return t
			}
			return nil
		}),
		"__schema", F("__Schema!", func(any, map[string]any, *gqlCtx) any { return map[string]any{} }),
	)})

	mutation := func(typ, input string, fn Resolver) *FieldDef {
		return FA(typ, []kv{{"input", input}}, fn)
	}
	labelNames := func(c *gqlCtx, ids any) any {
		list, ok := ids.([]any)
		if !ok {
			return Undefined
		}
		out := make([]any, len(list))
		for i, id := range list {
			out[i] = requireNode(c, id, "Label").value.(*Label).Name
		}
		return out
	}
	clientID := func(input map[string]any) any {
		if v, ok := input["clientMutationId"]; ok {
			return v
		}
		return nil
	}
	add(&TypeDef{Kind: "OBJECT", Name: "Mutation", Fields: fields(
		"createIssue", mutation("CreateIssuePayload", "CreateIssueInput!", func(_ any, a map[string]any, c *gqlCtx) any {
			in := inputOf(a)
			r := requireNode(c, field(in, "repositoryId"), "Repository").value.(*Repo)
			labels := labelNames(c, in["labelIds"])
			issue, err := c.forge.createIssue(r, field(in, "title"), field(in, "body"), labels)
			wrapForge(err)
			return map[string]any{"issue": issue, "clientMutationId": clientID(in)}
		}),
		"closeIssue", mutation("CloseIssuePayload", "CloseIssueInput!", func(_ any, a map[string]any, c *gqlCtx) any {
			in := inputOf(a)
			issue := requireNode(c, field(in, "issueId"), "Issue").value.(*Issue)
			reason := "completed"
			if s, ok := in["stateReason"].(string); ok {
				reason = strings.ToLower(s)
			}
			p := newIssuePatch()
			p.State, p.StateReason = "closed", reason
			wrapForge(c.forge.updateIssue(issue, p))
			return map[string]any{"issue": issue, "clientMutationId": clientID(in)}
		}),
		"reopenIssue", mutation("ReopenIssuePayload", "ReopenIssueInput!", func(_ any, a map[string]any, c *gqlCtx) any {
			in := inputOf(a)
			issue := requireNode(c, field(in, "issueId"), "Issue").value.(*Issue)
			p := newIssuePatch()
			p.State = "open"
			wrapForge(c.forge.updateIssue(issue, p))
			return map[string]any{"issue": issue, "clientMutationId": clientID(in)}
		}),
		"updateIssue", mutation("UpdateIssuePayload", "UpdateIssueInput!", func(_ any, a map[string]any, c *gqlCtx) any {
			in := inputOf(a)
			issue := requireNode(c, field(in, "id"), "Issue").value.(*Issue)
			p := newIssuePatch()
			p.Labels = labelNames(c, in["labelIds"])
			p.Title = nullToUndef(field(in, "title"))
			p.Body = nullToUndef(field(in, "body"))
			if s, ok := in["state"].(string); ok {
				p.State = strings.ToLower(s)
			}
			wrapForge(c.forge.updateIssue(issue, p))
			return map[string]any{"issue": issue, "clientMutationId": clientID(in)}
		}),
		"createPullRequest", mutation("CreatePullRequestPayload", "CreatePullRequestInput!", func(_ any, a map[string]any, c *gqlCtx) any {
			in := inputOf(a)
			r := requireNode(c, field(in, "repositoryId"), "Repository").value.(*Repo)
			pull, err := c.forge.createPull(r, field(in, "title"), field(in, "body"), field(in, "headRefName"), field(in, "baseRefName"), field(in, "draft"))
			wrapForge(err)
			return map[string]any{"pullRequest": pull, "clientMutationId": clientID(in)}
		}),
		"mergePullRequest", mutation("MergePullRequestPayload", "MergePullRequestInput!", func(_ any, a map[string]any, c *gqlCtx) any {
			in := inputOf(a)
			pull := requireNode(c, field(in, "pullRequestId"), "PullRequest").value.(*Issue)
			method := any("merge")
			if s, ok := in["mergeMethod"].(string); ok {
				method = strings.ToLower(s)
			}
			_, err := c.forge.mergePull(pull, method, field(in, "commitHeadline"), field(in, "commitBody"), field(in, "expectedHeadOid"))
			wrapForge(err)
			return map[string]any{"pullRequest": pull, "actor": ownerTyped(c.forge.viewer), "clientMutationId": clientID(in)}
		}),
		"closePullRequest", mutation("ClosePullRequestPayload", "ClosePullRequestInput!", func(_ any, a map[string]any, c *gqlCtx) any {
			in := inputOf(a)
			pull := requireNode(c, field(in, "pullRequestId"), "PullRequest").value.(*Issue)
			p := newIssuePatch()
			p.State = "closed"
			wrapForge(c.forge.updateIssue(pull, p))
			return map[string]any{"pullRequest": pull, "clientMutationId": clientID(in)}
		}),
		"updatePullRequest", mutation("UpdatePullRequestPayload", "UpdatePullRequestInput!", func(_ any, a map[string]any, c *gqlCtx) any {
			in := inputOf(a)
			pull := requireNode(c, field(in, "pullRequestId"), "PullRequest").value.(*Issue)
			p := newIssuePatch()
			p.Title = nullToUndef(field(in, "title"))
			p.Body = nullToUndef(field(in, "body"))
			wrapForge(c.forge.updateIssue(pull, p))
			if base, ok := in["baseRefName"].(string); ok {
				if _, exists := c.forge.branchTip(pull.Repo, base); exists {
					pull.Pull.Base = base
					c.forge.touchIssue(pull)
				}
			}
			return map[string]any{"pullRequest": pull, "clientMutationId": clientID(in)}
		}),
		"addComment", mutation("AddCommentPayload", "AddCommentInput!", func(_ any, a map[string]any, c *gqlCtx) any {
			in := inputOf(a)
			subject := requireNode(c, field(in, "subjectId"), "Issue", "PullRequest")
			issue := subject.value.(*Issue)
			comment, err := c.forge.addComment(issue, field(in, "body"))
			wrapForge(err)
			edge := map[string]any{"cursor": cursorFor(len(issue.Comments)), "node": comment}
			return map[string]any{"commentEdge": edge, "subject": subject, "timelineEdge": edge, "clientMutationId": clientID(in)}
		}),
	)})

	// Introspection types.
	add(
		&TypeDef{Kind: "OBJECT", Name: "__Schema", Fields: fields(
			"description", F("String", constant(nil)),
			"queryType", F("__Type!", func(any, map[string]any, *gqlCtx) any { return schema["Query"] }),
			"mutationType", F("__Type", func(any, map[string]any, *gqlCtx) any { return schema["Mutation"] }),
			"subscriptionType", F("__Type", constant(nil)),
			"types", F("[__Type!]!", func(any, map[string]any, *gqlCtx) any { return toAnySlice(schemaOrder) }),
			"directives", F("[__Directive!]!", func(any, map[string]any, *gqlCtx) any { return directives }),
		)},
		&TypeDef{Kind: "OBJECT", Name: "__Type", Fields: fields(
			"kind", F("__TypeKind!", func(p any, _ map[string]any, _ *gqlCtx) any { return introKind(p) }),
			"name", F("String", func(p any, _ map[string]any, _ *gqlCtx) any {
				if t, ok := p.(*TypeDef); ok {
					return t.Name
				}
				return nil
			}),
			"description", F("String", func(p any, _ map[string]any, _ *gqlCtx) any {
				if t, ok := p.(*TypeDef); ok && t.Description != "" {
					return t.Description
				}
				return nil
			}),
			"specifiedByURL", F("String", constant(nil)),
			"fields", FA("[__Field!]", []kv{{"includeDeprecated", "Boolean"}}, func(p any, _ map[string]any, _ *gqlCtx) any {
				t, ok := p.(*TypeDef)
				if !ok || (t.Kind != "OBJECT" && t.Kind != "INTERFACE") {
					return nil
				}
				out := []any{}
				if t.Fields != nil {
					for _, name := range t.Fields.order {
						if !strings.HasPrefix(name, "__") {
							out = append(out, &fieldIntro{name: name, def: t.Fields.m[name]})
						}
					}
				}
				return out
			}),
			"interfaces", F("[__Type!]", func(p any, _ map[string]any, _ *gqlCtx) any {
				t, ok := p.(*TypeDef)
				if !ok {
					return nil
				}
				if t.Kind == "OBJECT" {
					return namedTypes(t.Interfaces)
				}
				if t.Kind == "INTERFACE" {
					return []any{}
				}
				return nil
			}),
			"possibleTypes", F("[__Type!]", func(p any, _ map[string]any, _ *gqlCtx) any {
				t, ok := p.(*TypeDef)
				if !ok || (t.Kind != "INTERFACE" && t.Kind != "UNION") {
					return nil
				}
				return namedTypes(t.PossibleTypes)
			}),
			"enumValues", FA("[__EnumValue!]", []kv{{"includeDeprecated", "Boolean"}}, func(p any, _ map[string]any, _ *gqlCtx) any {
				t, ok := p.(*TypeDef)
				if !ok || t.Kind != "ENUM" {
					return nil
				}
				out := make([]any, len(t.Values))
				for i, v := range t.Values {
					out[i] = map[string]any{"name": v}
				}
				return out
			}),
			"inputFields", FA("[__InputValue!]", []kv{{"includeDeprecated", "Boolean"}}, func(p any, _ map[string]any, _ *gqlCtx) any {
				t, ok := p.(*TypeDef)
				if !ok || t.Kind != "INPUT_OBJECT" {
					return nil
				}
				return inputValues(t.InputFields)
			}),
			"ofType", F("__Type", func(p any, _ map[string]any, _ *gqlCtx) any {
				if w, ok := p.(*typeWrap); ok {
					return w.ofType
				}
				return nil
			}),
			"isOneOf", F("Boolean", constant(false)),
		)},
		&TypeDef{Kind: "OBJECT", Name: "__Field", Fields: fields(
			"name", F("String!", func(p any, _ map[string]any, _ *gqlCtx) any { return p.(*fieldIntro).name }),
			"description", F("String", func(p any, _ map[string]any, _ *gqlCtx) any {
				if d := p.(*fieldIntro).def.Description; d != "" {
					return d
				}
				return nil
			}),
			"args", FA("[__InputValue!]!", []kv{{"includeDeprecated", "Boolean"}}, func(p any, _ map[string]any, _ *gqlCtx) any {
				return inputValues(p.(*fieldIntro).def.Args)
			}),
			"type", F("__Type!", func(p any, _ map[string]any, _ *gqlCtx) any { return typeRef(p.(*fieldIntro).def.Type) }),
			"isDeprecated", F("Boolean!", constant(false)),
			"deprecationReason", F("String", constant(nil)),
		)},
		&TypeDef{Kind: "OBJECT", Name: "__InputValue", Fields: fields(
			"name", F("String!", func(p any, _ map[string]any, _ *gqlCtx) any { return p.(*inputValue).name }),
			"description", F("String", constant(nil)),
			"type", F("__Type!", func(p any, _ map[string]any, _ *gqlCtx) any { return typeRef(p.(*inputValue).typ) }),
			"defaultValue", F("String", constant(nil)),
			"isDeprecated", F("Boolean!", constant(false)),
			"deprecationReason", F("String", constant(nil)),
		)},
		&TypeDef{Kind: "OBJECT", Name: "__EnumValue", Fields: fields(
			"name", F("String!", func(p any, _ map[string]any, _ *gqlCtx) any { return p.(map[string]any)["name"] }),
			"description", F("String", constant(nil)),
			"isDeprecated", F("Boolean!", constant(false)),
			"deprecationReason", F("String", constant(nil)),
		)},
		&TypeDef{Kind: "OBJECT", Name: "__Directive", Fields: fields(
			"name", F("String!", func(p any, _ map[string]any, _ *gqlCtx) any { return p.(*directiveDef).name }),
			"description", F("String", constant(nil)),
			"locations", F("[String!]!", func(p any, _ map[string]any, _ *gqlCtx) any { return toAnySlice(p.(*directiveDef).locations) }),
			"args", FA("[__InputValue!]!", []kv{{"includeDeprecated", "Boolean"}}, func(p any, _ map[string]any, _ *gqlCtx) any {
				return inputValues(p.(*directiveDef).args)
			}),
			"isRepeatable", F("Boolean!", constant(false)),
		)},
	)

	schema = map[string]*TypeDef{}
	for _, t := range types {
		if _, ok := schema[t.Name]; !ok {
			schemaOrder = append(schemaOrder, t)
		} else {
			for i, x := range schemaOrder {
				if x.Name == t.Name {
					schemaOrder[i] = t
				}
			}
		}
		schema[t.Name] = t
	}
}

func orUndef(a map[string]any, key string) any {
	if v, ok := a[key]; ok {
		return v
	}
	return Undefined
}

func derefOr(s *string, fallback string) string {
	if s == nil {
		return fallback
	}
	return *s
}

func containsAny(list []any, s string) bool {
	for _, x := range list {
		if v, ok := x.(string); ok && v == s {
			return true
		}
	}
	return false
}

func introKind(p any) string {
	switch t := p.(type) {
	case *TypeDef:
		return t.Kind
	case *typeWrap:
		return t.kind
	}
	return ""
}

func namedTypes(names []string) []any {
	out := make([]any, len(names))
	for i, name := range names {
		if t := schema[name]; t != nil {
			out[i] = t
		}
	}
	return out
}

func inputValues(list []kv) []any {
	out := make([]any, len(list))
	for i, e := range list {
		out[i] = &inputValue{name: e.k, typ: e.v}
	}
	return out
}

func typeRef(typ string) any {
	if strings.HasSuffix(typ, "!") {
		return &typeWrap{kind: "NON_NULL", ofType: typeRef(typ[:len(typ)-1])}
	}
	if strings.HasPrefix(typ, "[") {
		return &typeWrap{kind: "LIST", ofType: typeRef(typ[1 : len(typ)-1])}
	}
	if t := schema[typ]; t != nil {
		return t
	}
	return nil
}

func namedType(typ string) string {
	return strings.Map(func(r rune) rune {
		if r == '[' || r == ']' || r == '!' {
			return -1
		}
		return r
	}, typ)
}

// ---------------------------------------------------------------- validation

func fieldsOf(typeName string) (*FieldMap, bool) {
	t := schema[typeName]
	if t == nil {
		return nil, false
	}
	switch t.Kind {
	case "OBJECT", "INTERFACE":
		if t.Fields == nil {
			return &FieldMap{m: map[string]*FieldDef{}}, true
		}
		return t.Fields, true
	case "UNION":
		return &FieldMap{m: map[string]*FieldDef{}}, true
	}
	return nil, false
}

func loc(line, column int) []any { return []any{O("line", line, "column", column)} }

func pathOf(path []string, extra ...string) []any {
	out := make([]any, 0, len(path)+len(extra))
	for _, p := range path {
		out = append(out, p)
	}
	for _, p := range extra {
		out = append(out, p)
	}
	return out
}

func validate(doc *Document, op *Operation) []any {
	var errs []any
	opName := op.Kind + " anonymous"
	if op.HasName {
		opName = op.Kind + " " + op.Name
	}
	visited := map[string]bool{}
	var walk func(typeName string, selections []*Selection, path []string)
	walk = func(typeName string, selections []*Selection, path []string) {
		fm, _ := fieldsOf(typeName)
		for _, sel := range selections {
			switch sel.Kind {
			case selField:
				if sel.Name == "__typename" {
					continue
				}
				def := fm.get(sel.Name)
				if def == nil {
					errs = append(errs, O(
						"path", pathOf(path, sel.Name),
						"extensions", O("code", "undefinedField", "typeName", typeName, "fieldName", sel.Name),
						"locations", loc(sel.Line, sel.Column),
						"message", "Field '"+sel.Name+"' doesn't exist on type '"+typeName+"'",
					))
					continue
				}
				target := namedType(def.Type)
				tt := schema[target]
				composite := tt != nil && (tt.Kind == "OBJECT" || tt.Kind == "INTERFACE" || tt.Kind == "UNION")
				if composite && sel.Selections == nil {
					errs = append(errs, O(
						"path", pathOf(path, sel.Name),
						"extensions", O("code", "selectionMismatch", "nodeName", "field '"+sel.Name+"'", "typeName", target),
						"locations", loc(sel.Line, sel.Column),
						"message", "Field must have selections (field '"+sel.Name+"' returns "+target+" but has no selections. Did you mean '"+sel.Name+" { ... }'?)",
					))
					continue
				}
				if !composite && sel.Selections != nil {
					names := make([]string, len(sel.Selections))
					for i, s := range sel.Selections {
						if s.Kind == selField {
							names[i] = s.Name
						} else {
							names[i] = "..."
						}
					}
					errs = append(errs, O(
						"path", pathOf(path, sel.Name),
						"extensions", O("code", "selectionMismatch", "nodeName", "field '"+sel.Name+"'", "typeName", target),
						"locations", loc(sel.Line, sel.Column),
						"message", "Selections can't be made on scalars (field '"+sel.Name+"' returns "+target+" but has selections ["+strings.Join(names, ", ")+"])",
					))
					continue
				}
				if sel.Selections != nil {
					key := sel.Name
					if sel.Alias != "" {
						key = sel.Alias
					}
					walk(target, sel.Selections, append(append([]string(nil), path...), key))
				}
			case selInline:
				condition := sel.TypeCondition
				if condition == "" {
					condition = typeName
				}
				if schema[condition] == nil {
					errs = append(errs, O("path", pathOf(path), "extensions", O("code", "undefinedType", "typeName", condition), "message", "No such type "+condition+", so it can't be a fragment condition"))
					continue
				}
				walk(condition, sel.Selections, append(append([]string(nil), path...), "... on "+condition))
			default:
				frag := doc.Fragments[sel.Name]
				if frag == nil {
					errs = append(errs, O("path", pathOf(path), "extensions", O("code", "useAndDefineFragment", "fragmentName", sel.Name), "locations", loc(sel.Line, sel.Column), "message", "Fragment "+sel.Name+" was used, but not defined"))
					continue
				}
				if visited[sel.Name] {
					continue
				}
				visited[sel.Name] = true
				if schema[frag.TypeCondition] == nil {
					errs = append(errs, O("path", pathOf(path), "extensions", O("code", "undefinedType", "typeName", frag.TypeCondition), "message", "No such type "+frag.TypeCondition+", so it can't be a fragment condition"))
					continue
				}
				walk(frag.TypeCondition, frag.Selections, append(append([]string(nil), path...), "..."+sel.Name))
			}
		}
	}
	if op.Kind == "subscription" {
		return []any{O("message", "Schema is not configured for subscriptions")}
	}
	root := "Query"
	if op.Kind == "mutation" {
		root = "Mutation"
	}
	walk(root, op.Selections, []string{opName})
	return errs
}

// ---------------------------------------------------------------- execution

func valueOf(v *GValue, variables map[string]any) any {
	switch v.Kind {
	case gvVar:
		if x, ok := variables[v.Str]; ok && !isUndef(x) {
			return x
		}
		return nil
	case gvInt, gvFloat:
		return v.Num
	case gvString, gvEnum:
		return v.Str
	case gvBool:
		return v.Bool
	case gvNull:
		return nil
	case gvList:
		out := make([]any, len(v.List))
		for i, item := range v.List {
			out[i] = valueOf(item, variables)
		}
		return out
	case gvObject:
		out := make(map[string]any, len(v.Fields))
		for _, f := range v.Fields {
			out[f.Name] = valueOf(f.Value, variables)
		}
		return out
	}
	return nil
}

func skipped(dirs []Directive, variables map[string]any) bool {
	for _, d := range dirs {
		condition := true
		for _, a := range d.Args {
			if a.Name == "if" {
				condition = valueOf(a.Value, variables) == true
				break
			}
		}
		if d.Name == "skip" && condition {
			return true
		}
		if d.Name == "include" && !condition {
			return true
		}
	}
	return false
}

func implementsType(runtime, condition string) bool {
	if runtime == condition {
		return true
	}
	if t := schema[runtime]; t != nil && containsStr(t.Interfaces, condition) {
		return true
	}
	c := schema[condition]
	return c != nil && c.Kind == "UNION" && containsStr(c.PossibleTypes, runtime)
}

type executor struct {
	errors    []any
	doc       *Document
	variables map[string]any
	ctx       *gqlCtx
}

func (e *executor) collect(runtime string, selections []*Selection, keys *[]string, groups map[string][]*Selection, visited map[string]bool) {
	for _, sel := range selections {
		if skipped(sel.Directives, e.variables) {
			continue
		}
		switch sel.Kind {
		case selField:
			key := sel.Name
			if sel.Alias != "" {
				key = sel.Alias
			}
			if _, ok := groups[key]; !ok {
				*keys = append(*keys, key)
			}
			groups[key] = append(groups[key], sel)
		case selInline:
			if sel.TypeCondition == "" || implementsType(runtime, sel.TypeCondition) {
				e.collect(runtime, sel.Selections, keys, groups, visited)
			}
		default:
			if visited[sel.Name] {
				continue
			}
			frag := e.doc.Fragments[sel.Name]
			if frag == nil || !implementsType(runtime, frag.TypeCondition) {
				continue
			}
			visited[sel.Name] = true
			e.collect(runtime, frag.Selections, keys, groups, visited)
		}
	}
}

func appendPath(path []any, key any) []any {
	out := make([]any, len(path)+1)
	copy(out, path)
	out[len(path)] = key
	return out
}

func (e *executor) resolve(def *FieldDef, value any, args map[string]any, field *Selection, path []any, key string) (raw any, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			typ, msg := "INTERNAL", fmt.Sprint(r)
			switch x := r.(type) {
			case *ResolveError:
				typ, msg = x.Type, x.Message
			case error:
				msg = x.Error()
			}
			e.errors = append(e.errors, O("type", typ, "path", appendPath(path, key), "locations", loc(field.Line, field.Column), "message", msg))
			raw, ok = nil, false
		}
	}()
	if def.Resolve != nil {
		return def.Resolve(value, args, e.ctx), true
	}
	if m, isMap := value.(map[string]any); isMap {
		return m[field.Name], true
	}
	return nil, true
}

func (e *executor) executeSelections(typeName string, value any, selections []*Selection, path []any) *Obj {
	var keys []string
	groups := map[string][]*Selection{}
	e.collect(typeName, selections, &keys, groups, map[string]bool{})
	fm, _ := fieldsOf(typeName)
	out := &Obj{k: make([]string, 0, len(keys)), v: make([]any, 0, len(keys))}
	for _, key := range keys {
		group := groups[key]
		field := group[0]
		if field.Name == "__typename" {
			out.Set(key, typeName)
			continue
		}
		def := fm.get(field.Name)
		if def == nil {
			out.Set(key, nil)
			continue
		}
		args := make(map[string]any, len(field.Args))
		for _, a := range field.Args {
			args[a.Name] = valueOf(a.Value, e.variables)
		}
		raw, ok := e.resolve(def, value, args, field, path, key)
		if !ok {
			out.Set(key, nil)
			continue
		}
		var sub []*Selection
		for _, item := range group {
			if item.Selections != nil {
				sub = append(sub, item.Selections...)
			}
		}
		out.Set(key, e.complete(def.Type, raw, sub, appendPath(path, key)))
	}
	return out
}

func isNilish(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case undefT:
		return true
	case *Repo:
		return x == nil
	case *Issue:
		return x == nil
	case *User:
		return x == nil
	case *Label:
		return x == nil
	case *Comment:
		return x == nil
	case *TypeDef:
		return x == nil
	case *typeWrap:
		return x == nil
	case *typedVal:
		return x == nil
	case *commitVal:
		return x == nil
	case *refVal:
		return x == nil
	case *string:
		return x == nil
	case []any:
		return x == nil && false
	}
	return false
}

func (e *executor) complete(typ string, raw any, selections []*Selection, path []any) any {
	for strings.HasSuffix(typ, "!") {
		typ = typ[:len(typ)-1]
	}
	if isNilish(raw) {
		return nil
	}
	if strings.HasPrefix(typ, "[") {
		list, ok := raw.([]any)
		if !ok {
			return nil
		}
		inner := typ[1 : len(typ)-1]
		out := make([]any, len(list))
		for i, item := range list {
			out[i] = e.complete(inner, item, selections, appendPath(path, i))
		}
		return out
	}
	def := schema[typ]
	if def == nil || def.Kind == "SCALAR" || def.Kind == "ENUM" {
		return raw
	}
	if def.Kind == "OBJECT" {
		value := raw
		if tv, ok := raw.(*typedVal); ok {
			value = tv.value
		}
		return e.executeSelections(typ, value, selections, path)
	}
	if tv, ok := raw.(*typedVal); ok {
		return e.executeSelections(tv.typ, tv.value, selections, path)
	}
	return nil
}

func coerceVariables(op *Operation, given any) map[string]any {
	out := map[string]any{}
	if m, ok := given.(map[string]any); ok {
		for k, v := range m {
			out[k] = v
		}
	}
	for _, d := range op.Variables {
		if _, ok := out[d.Name]; !ok && d.DefaultValue != nil {
			out[d.Name] = valueOf(d.DefaultValue, map[string]any{})
		}
	}
	return out
}

type parsedDoc struct {
	doc *Document
	err *GraphQLSyntaxError
}

type GraphQL struct {
	forge    *Forge
	docCache map[string]parsedDoc
}

func NewGraphQL(f *Forge) *GraphQL { return &GraphQL{forge: f, docCache: map[string]parsedDoc{}} }

func (g *GraphQL) parse(source string) parsedDoc {
	if cached, ok := g.docCache[source]; ok {
		return cached
	}
	doc, err := parseGraphQL(source)
	var parsed parsedDoc
	if err != nil {
		parsed.err = err.(*GraphQLSyntaxError)
	} else {
		parsed.doc = doc
	}
	if len(g.docCache) > 500 {
		g.docCache = map[string]parsedDoc{}
	}
	g.docCache[source] = parsed
	return parsed
}

func (g *GraphQL) execute(request map[string]any, origin string) *Obj {
	query, ok := request["query"].(string)
	if !ok || strings.TrimSpace(query) == "" {
		return O("errors", []any{O("message", "A query attribute must be specified and must be a string.")})
	}
	parsed := g.parse(query)
	if parsed.err != nil {
		return O("errors", []any{O("message", parsed.err.Message, "locations", loc(parsed.err.Line, parsed.err.Column))})
	}
	doc := parsed.doc
	var op *Operation
	if name, ok := request["operationName"].(string); ok && name != "" {
		for _, o := range doc.Operations {
			if o.HasName && o.Name == name {
				op = o
				break
			}
		}
		if op == nil {
			return O("errors", []any{O("message", "No operation named \""+name+"\"")})
		}
	} else if len(doc.Operations) == 1 {
		op = doc.Operations[0]
	} else if len(doc.Operations) == 0 {
		return O("errors", []any{O("message", "No operations in query document")})
	} else {
		return O("errors", []any{O("message", "An operation name is required")})
	}
	if errs := validate(doc, op); len(errs) > 0 {
		return O("errors", errs)
	}
	variables := coerceVariables(op, request["variables"])
	for _, d := range op.Variables {
		if strings.HasSuffix(d.Type, "!") {
			if v, ok := variables[d.Name]; !ok || v == nil {
				return O("errors", []any{O(
					"extensions", O("value", nil, "problems", []any{O("path", []any{}, "explanation", "Expected value to not be null")}),
					"locations", []any{},
					"message", "Variable $"+d.Name+" of type "+d.Type+" was provided invalid value",
				)})
			}
		}
	}
	ex := &executor{doc: doc, variables: variables, ctx: &gqlCtx{forge: g.forge, origin: origin}}
	root := "Query"
	if op.Kind == "mutation" {
		root = "Mutation"
	}
	data := ex.executeSelections(root, nil, op.Selections, []any{})
	if len(ex.errors) > 0 {
		return O("data", data, "errors", ex.errors)
	}
	return O("data", data)
}
