// REST v3 JSON views (GitHub's shapes). Repository, issue and pull views are cached as bytes on the
// objects and dropped whenever the object (or, for a pull, its branches) changes.
package main

import (
	"encoding/base64"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

func isoOrNull(ms int64) any {
	if ms == 0 {
		return nil
	}
	return iso(ms)
}

func signatureJSON(sig Signature) *Obj {
	return O("name", sig.Name, "email", sig.Email, "date", iso(sig.Time*1000))
}

type Views struct {
	forge  *Forge
	origin string
	api    string
}

func NewViews(f *Forge) *Views {
	return &Views{forge: f, origin: "http://localhost", api: "http://localhost/api/v3"}
}

// setHost adopts the request's Host as the URL origin; a change drops every cached view.
func (v *Views) setHost(host string) {
	if host == "" {
		return
	}
	if len(v.origin) == 7+len(host) && v.origin[7:] == host {
		return
	}
	v.origin = "http://" + host
	v.api = v.origin + "/api/v3"
	for _, r := range v.forge.repoOrder {
		if !r.Deleted {
			v.forge.touchRepo(r)
		}
	}
}

func (v *Views) user(u *User) *Obj {
	return O(
		"login", u.Login,
		"id", u.ID,
		"node_id", u.NodeID,
		"avatar_url", v.origin+"/avatars/"+u.Login,
		"gravatar_id", "",
		"url", v.api+"/users/"+u.Login,
		"html_url", v.origin+"/"+u.Login,
		"repos_url", v.api+"/users/"+u.Login+"/repos",
		"type", u.Type,
		"user_view_type", "public",
		"site_admin", false,
	)
}

func (v *Views) userFull(u *User) *Obj {
	public, private := 0, 0
	for _, r := range v.forge.repoOrder {
		if r.Deleted || v.forge.reposByID[r.ID] != r || r.Owner != u {
			continue
		}
		if r.Private {
			private++
		} else {
			public++
		}
	}
	o := v.user(u)
	o.Set("name", u.Name).Set("company", nil).Set("blog", "").Set("location", nil).Set("email", u.Email).
		Set("hireable", nil).Set("bio", nil).Set("twitter_username", nil).Set("public_repos", public).
		Set("public_gists", 0).Set("followers", 0).Set("following", 0).
		Set("created_at", "2026-01-01T00:00:00Z").Set("updated_at", "2026-01-01T00:00:00Z")
	if u == v.forge.viewer {
		o.Set("total_private_repos", private).Set("owned_private_repos", private).Set("two_factor_authentication", false).
			Set("plan", O("name", "free", "space", 976562499, "collaborators", 0, "private_repos", 10000))
	}
	return o
}

var schemeRe = regexp.MustCompile(`^https?://`)

func (v *Views) repoObject(r *Repo) *Obj {
	full := r.Owner.Login + "/" + r.Name
	u := v.api + "/repos/" + full
	open := 0
	for _, issue := range r.IssueList {
		if issue.State == "open" {
			open++
		}
	}
	visibility := "public"
	if r.Private {
		visibility = "private"
	}
	return O(
		"id", r.ID,
		"node_id", r.NodeID,
		"name", r.Name,
		"full_name", full,
		"private", r.Private,
		"owner", v.user(r.Owner),
		"html_url", v.origin+"/"+full,
		"description", r.Description,
		"fork", false,
		"url", u,
		"issues_url", u+"/issues{/number}",
		"pulls_url", u+"/pulls{/number}",
		"contents_url", u+"/contents/{+path}",
		"branches_url", u+"/branches{/branch}",
		"commits_url", u+"/commits{/sha}",
		"labels_url", u+"/labels{/name}",
		"git_refs_url", u+"/git/refs{/sha}",
		"created_at", iso(r.CreatedAt),
		"updated_at", iso(r.UpdatedAt),
		"pushed_at", iso(r.PushedAt),
		"git_url", "git://"+schemeRe.ReplaceAllString(v.origin, "")+"/"+full+".git",
		"ssh_url", "git@localhost:"+full+".git",
		"clone_url", v.origin+"/"+full+".git",
		"homepage", r.Homepage,
		"size", 0,
		"stargazers_count", 0,
		"watchers_count", 0,
		"language", nil,
		"has_issues", r.HasIssues,
		"has_projects", r.HasProjects,
		"has_downloads", true,
		"has_wiki", r.HasWiki,
		"has_pages", false,
		"has_discussions", false,
		"forks_count", 0,
		"mirror_url", nil,
		"archived", r.Archived,
		"disabled", false,
		"open_issues_count", open,
		"license", nil,
		"allow_forking", true,
		"is_template", false,
		"web_commit_signoff_required", false,
		"topics", []any{},
		"visibility", visibility,
		"forks", 0,
		"open_issues", open,
		"watchers", 0,
		"default_branch", r.DefaultBranch,
		"permissions", O("admin", true, "maintain", true, "push", true, "triage", true, "pull", true),
		"allow_squash_merge", true,
		"allow_merge_commit", true,
		"allow_rebase_merge", true,
		"delete_branch_on_merge", false,
		"network_count", 0,
		"subscribers_count", 1,
	)
}

func (v *Views) repo(r *Repo) []byte {
	if r.json == nil {
		r.json = toJSON(v.repoObject(r))
	}
	return r.json
}

// encodeURIComponent escapes like JavaScript's encodeURIComponent.
func encodeURIComponent(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte("0123456789ABCDEF"[c>>4])
			b.WriteByte("0123456789ABCDEF"[c&15])
		}
	}
	return b.String()
}

var _ = url.PathEscape

func (v *Views) label(r *Repo, l *Label) *Obj {
	return O(
		"id", l.ID,
		"node_id", l.NodeID,
		"url", v.api+"/repos/"+r.Owner.Login+"/"+r.Name+"/labels/"+encodeURIComponent(l.Name),
		"name", l.Name,
		"color", l.Color,
		"default", false,
		"description", l.Description,
	)
}

func reactions(u string) *Obj {
	return O("url", u, "total_count", 0, "+1", 0, "-1", 0, "laugh", 0, "hooray", 0, "confused", 0, "heart", 0, "rocket", 0, "eyes", 0)
}

func (v *Views) labelsOf(r *Repo, labels []*Label) []any {
	out := make([]any, 0, len(labels))
	for _, l := range labels {
		out = append(out, v.label(r, l))
	}
	return out
}

func (v *Views) issueObject(issue *Issue) *Obj {
	r := issue.Repo
	repoURL := v.api + "/repos/" + r.Owner.Login + "/" + r.Name
	num := strconv.Itoa(issue.Number)
	u := repoURL + "/issues/" + num
	kind := "issues"
	if issue.Pull != nil {
		kind = "pull"
	}
	html := v.origin + "/" + r.Owner.Login + "/" + r.Name + "/" + kind + "/" + num
	var closedBy any
	if issue.State == "closed" {
		closedBy = v.user(v.forge.viewer)
	}
	out := O(
		"url", u,
		"repository_url", repoURL,
		"labels_url", u+"/labels{/name}",
		"comments_url", u+"/comments",
		"events_url", u+"/events",
		"html_url", html,
		"id", issue.ID,
		"node_id", issue.NodeID,
		"number", issue.Number,
		"title", issue.Title,
		"user", v.user(issue.User),
		"labels", v.labelsOf(r, issue.Labels),
		"state", issue.State,
		"locked", false,
		"assignee", nil,
		"assignees", []any{},
		"milestone", nil,
		"comments", len(issue.Comments),
		"created_at", iso(issue.CreatedAt),
		"updated_at", iso(issue.UpdatedAt),
		"closed_at", isoOrNull(issue.ClosedAt),
		"author_association", "OWNER",
		"type", nil,
		"active_lock_reason", nil,
		"sub_issues_summary", O("total", 0, "completed", 0, "percent_completed", 0),
		"body", issue.Body,
		"closed_by", closedBy,
		"reactions", reactions(u+"/reactions"),
		"timeline_url", u+"/timeline",
		"performed_via_github_app", nil,
		"state_reason", issue.StateReason,
	)
	if issue.Pull != nil {
		out.Set("draft", issue.Pull.Draft)
		out.Set("pull_request", O(
			"url", repoURL+"/pulls/"+num,
			"html_url", html,
			"diff_url", html+".diff",
			"patch_url", html+".patch",
			"merged_at", isoOrNull(issue.Pull.MergedAt),
		))
	}
	return out
}

func (v *Views) issue(issue *Issue) []byte {
	if issue.json == nil {
		issue.json = toJSON(v.issueObject(issue))
	}
	return issue.json
}

func (v *Views) comment(c *Comment) *Obj {
	r := c.Issue.Repo
	repoURL := v.api + "/repos/" + r.Owner.Login + "/" + r.Name
	id := strconv.Itoa(c.ID)
	num := strconv.Itoa(c.Issue.Number)
	return O(
		"url", repoURL+"/issues/comments/"+id,
		"html_url", v.origin+"/"+r.Owner.Login+"/"+r.Name+"/issues/"+num+"#issuecomment-"+id,
		"issue_url", repoURL+"/issues/"+num,
		"id", c.ID,
		"node_id", c.NodeID,
		"user", v.user(c.User),
		"created_at", iso(c.CreatedAt),
		"updated_at", iso(c.UpdatedAt),
		"author_association", "OWNER",
		"body", c.Body,
		"reactions", reactions(repoURL+"/issues/comments/"+id+"/reactions"),
		"performed_via_github_app", nil,
	)
}

func (v *Views) pullObject(issue *Issue) *Obj {
	f := v.forge
	p := issue.Pull
	r := issue.Repo
	repoURL := v.api + "/repos/" + r.Owner.Login + "/" + r.Name
	num := strconv.Itoa(issue.Number)
	u := repoURL + "/pulls/" + num
	html := v.origin + "/" + r.Owner.Login + "/" + r.Name + "/pull/" + num
	headSha := f.pullHeadSha(issue)
	baseSha := f.pullBaseSha(issue)
	files := f.pullFiles(issue)
	additions, deletions := 0, 0
	for _, file := range files {
		additions += file.Additions
		deletions += file.Deletions
	}
	repoRaw := Raw(v.repo(r))
	var mergeable any
	if issue.State == "open" {
		mergeable = f.mergeable(issue)
	} else if p.Merged {
		mergeable = nil
	} else {
		mergeable = f.mergeTree(issue) != nil
	}
	state := "unknown"
	if !p.Merged {
		if b, ok := mergeable.(bool); ok {
			if b {
				state = "clean"
			} else {
				state = "dirty"
			}
		}
	}
	var mergedBy any
	if p.MergedBy != nil {
		mergedBy = v.user(p.MergedBy)
	}
	var mergeSha any
	if p.MergeCommitSha != "" {
		mergeSha = p.MergeCommitSha
	}
	return O(
		"url", u,
		"id", p.ID,
		"node_id", p.NodeID,
		"html_url", html,
		"diff_url", html+".diff",
		"patch_url", html+".patch",
		"issue_url", repoURL+"/issues/"+num,
		"number", issue.Number,
		"state", issue.State,
		"locked", false,
		"title", issue.Title,
		"user", v.user(issue.User),
		"body", issue.Body,
		"created_at", iso(issue.CreatedAt),
		"updated_at", iso(issue.UpdatedAt),
		"closed_at", isoOrNull(issue.ClosedAt),
		"merged_at", isoOrNull(p.MergedAt),
		"merge_commit_sha", mergeSha,
		"assignee", nil,
		"assignees", []any{},
		"requested_reviewers", []any{},
		"requested_teams", []any{},
		"labels", v.labelsOf(r, issue.Labels),
		"milestone", nil,
		"draft", p.Draft,
		"commits_url", u+"/commits",
		"review_comments_url", u+"/comments",
		"review_comment_url", repoURL+"/pulls/comments{/number}",
		"comments_url", repoURL+"/issues/"+num+"/comments",
		"statuses_url", repoURL+"/statuses/"+headSha,
		"head", O("label", r.Owner.Login+":"+p.Head, "ref", p.Head, "sha", headSha, "user", v.user(r.Owner), "repo", repoRaw),
		"base", O("label", r.Owner.Login+":"+p.Base, "ref", p.Base, "sha", baseSha, "user", v.user(r.Owner), "repo", repoRaw),
		"_links", O(
			"self", O("href", u),
			"html", O("href", html),
			"issue", O("href", repoURL+"/issues/"+num),
			"comments", O("href", repoURL+"/issues/"+num+"/comments"),
			"commits", O("href", u+"/commits"),
			"statuses", O("href", repoURL+"/statuses/"+headSha),
		),
		"author_association", "OWNER",
		"auto_merge", nil,
		"active_lock_reason", nil,
		"merged", p.Merged,
		"mergeable", mergeable,
		"rebaseable", mergeable,
		"mergeable_state", state,
		"merged_by", mergedBy,
		"comments", len(issue.Comments),
		"review_comments", 0,
		"maintainer_can_modify", false,
		"commits", len(f.pullCommits(issue)),
		"additions", additions,
		"deletions", deletions,
		"changed_files", len(files),
	)
}

func (v *Views) pull(issue *Issue) []byte {
	p := issue.Pull
	if p.json == nil || p.jsonVersion != issue.Repo.Version {
		p.json = toJSON(v.pullObject(issue))
		p.jsonVersion = issue.Repo.Version
	}
	return p.json
}

var trailingNewlines = regexp.MustCompile(`\n+$`)

func commitNodeID(oid string) string {
	return "C_" + base64.RawURLEncoding.EncodeToString([]byte("0:Commit:"+oid))
}

func verification() *Obj {
	return O("verified", false, "reason", "unsigned", "signature", nil, "payload", nil, "verified_at", nil)
}

func (v *Views) commit(r *Repo, oid string) *Obj {
	c := v.forge.store.Commit(oid)
	if c == nil {
		return nil
	}
	repoURL := v.api + "/repos/" + r.Owner.Login + "/" + r.Name
	var authorUser, committerUser any
	if c.Author.Email == v.forge.viewer.Email {
		authorUser = v.user(v.forge.viewer)
	}
	if c.Committer.Email == v.forge.viewer.Email {
		committerUser = v.user(v.forge.viewer)
	}
	parents := make([]any, 0, len(c.Parents))
	for _, p := range c.Parents {
		parents = append(parents, O("sha", p, "url", repoURL+"/commits/"+p, "html_url", v.origin+"/"+r.Owner.Login+"/"+r.Name+"/commit/"+p))
	}
	return O(
		"sha", oid,
		"node_id", commitNodeID(oid),
		"commit", O(
			"author", signatureJSON(c.Author),
			"committer", signatureJSON(c.Committer),
			"message", strings.TrimRight(c.Message, "\n"),
			"tree", O("sha", c.Tree, "url", repoURL+"/git/trees/"+c.Tree),
			"url", repoURL+"/git/commits/"+oid,
			"comment_count", 0,
			"verification", verification(),
		),
		"url", repoURL+"/commits/"+oid,
		"html_url", v.origin+"/"+r.Owner.Login+"/"+r.Name+"/commit/"+oid,
		"comments_url", repoURL+"/commits/"+oid+"/comments",
		"author", authorUser,
		"committer", committerUser,
		"parents", parents,
	)
}

func (v *Views) gitCommit(r *Repo, oid string) *Obj {
	c := v.forge.store.Commit(oid)
	if c == nil {
		return nil
	}
	repoURL := v.api + "/repos/" + r.Owner.Login + "/" + r.Name
	parents := make([]any, 0, len(c.Parents))
	for _, p := range c.Parents {
		parents = append(parents, O("sha", p, "url", repoURL+"/git/commits/"+p, "html_url", v.origin+"/"+r.Owner.Login+"/"+r.Name+"/commit/"+p))
	}
	return O(
		"sha", oid,
		"node_id", commitNodeID(oid),
		"url", repoURL+"/git/commits/"+oid,
		"html_url", v.origin+"/"+r.Owner.Login+"/"+r.Name+"/commit/"+oid,
		"author", signatureJSON(c.Author),
		"committer", signatureJSON(c.Committer),
		"tree", O("sha", c.Tree, "url", repoURL+"/git/trees/"+c.Tree),
		"message", strings.TrimRight(c.Message, "\n"),
		"parents", parents,
		"verification", verification(),
	)
}

func (v *Views) ref(r *Repo, name, oid string) *Obj {
	repoURL := v.api + "/repos/" + r.Owner.Login + "/" + r.Name
	typ := "commit"
	if o := v.forge.store.Get(oid); o != nil && o.Type == ObjTag {
		typ = "tag"
	}
	kind := "commits"
	if typ == "tag" {
		kind = "tags"
	}
	return O(
		"ref", name,
		"node_id", "REF_"+base64.RawURLEncoding.EncodeToString([]byte("0:Ref:"+strconv.Itoa(r.ID)+":"+name)),
		"url", repoURL+"/git/"+name,
		"object", O("sha", oid, "type", typ, "url", repoURL+"/git/"+kind+"/"+oid),
	)
}

func (v *Views) branchShort(r *Repo, name, oid string) *Obj {
	return O("name", name, "commit", O("sha", oid, "url", v.api+"/repos/"+r.Owner.Login+"/"+r.Name+"/commits/"+oid), "protected", false)
}

func (v *Views) branchFull(r *Repo, name, oid string) *Obj {
	repoURL := v.api + "/repos/" + r.Owner.Login + "/" + r.Name
	var commit any
	if c := v.commit(r, oid); c != nil {
		commit = c
	}
	return O(
		"name", name,
		"commit", commit,
		"_links", O("self", repoURL+"/branches/"+name, "html", v.origin+"/"+r.Owner.Login+"/"+r.Name+"/tree/"+name),
		"protected", false,
		"protection", O("enabled", false, "required_status_checks", O("enforcement_level", "off", "contexts", []any{}, "checks", []any{})),
		"protection_url", repoURL+"/branches/"+name+"/protection",
	)
}

func (v *Views) contentEntry(r *Repo, path, mode, oid, ref string) *Obj {
	repoURL := v.api + "/repos/" + r.Owner.Login + "/" + r.Name
	typ := "file"
	switch mode {
	case "40000":
		typ = "dir"
	case "120000":
		typ = "symlink"
	case "160000":
		typ = "submodule"
	}
	name := path[strings.LastIndexByte(path, '/')+1:]
	size := 0
	if typ == "file" || typ == "symlink" {
		if o := v.forge.store.Get(oid); o != nil {
			size = len(o.Data)
		}
	}
	self := repoURL + "/contents/" + path + "?ref=" + encodeURIComponent(ref)
	gitKind, htmlKind := "blobs", "blob"
	if typ == "dir" {
		gitKind, htmlKind = "trees", "tree"
	}
	git := repoURL + "/git/" + gitKind + "/" + oid
	html := v.origin + "/" + r.Owner.Login + "/" + r.Name + "/" + htmlKind + "/" + ref + "/" + path
	var download any
	if typ == "file" {
		download = v.origin + "/" + r.Owner.Login + "/" + r.Name + "/raw/" + ref + "/" + path
	}
	return O(
		"name", name,
		"path", path,
		"sha", oid,
		"size", size,
		"url", self,
		"html_url", html,
		"git_url", git,
		"download_url", download,
		"type", typ,
		"_links", O("self", self, "git", git, "html", html),
	)
}
