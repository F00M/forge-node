// The forge's domain: users, repositories with refs, labels, issues, pull requests and comments, plus the
// commit-producing operations. Every surface (REST, GraphQL, MCP, git) reads and writes the same objects.
package main

import (
	"bytes"
	"encoding/base64"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type User struct {
	ID     int
	NodeID string
	Login  string
	Name   string
	Email  string
	Type   string // "User" or "Organization"
}

type Label struct {
	ID          int
	NodeID      string
	Name        string
	Color       string
	Description *string
}

type Comment struct {
	ID        int
	NodeID    string
	Body      string
	User      *User
	CreatedAt int64
	UpdatedAt int64
	Issue     *Issue
}

type PullData struct {
	ID             int
	NodeID         string
	Head           string
	Base           string
	HeadSha        string
	BaseSha        string
	Merged         bool
	MergedAt       int64 // 0: null
	MergeCommitSha string
	MergedBy       *User
	Draft          bool
	json           []byte
	jsonVersion    int
	files          []FileChange
	filesKey       string
}

type Issue struct {
	ID          int
	NodeID      string
	Repo        *Repo
	Number      int
	Title       string
	Body        *string
	State       string
	StateReason *string
	Labels      []*Label
	Comments    []*Comment
	User        *User
	CreatedAt   int64
	UpdatedAt   int64
	ClosedAt    int64 // 0: null
	Pull        *PullData
	json        []byte
}

// RefMap is an insertion-ordered ref name -> oid map (a JS Map's semantics).
type RefMap struct {
	keys []string
	m    map[string]string
}

func newRefMap() *RefMap { return &RefMap{m: map[string]string{}} }

func (r *RefMap) Get(k string) (string, bool) { v, ok := r.m[k]; return v, ok }

func (r *RefMap) Has(k string) bool { _, ok := r.m[k]; return ok }

func (r *RefMap) Set(k, v string) {
	if _, ok := r.m[k]; !ok {
		r.keys = append(r.keys, k)
	}
	r.m[k] = v
}

func (r *RefMap) Delete(k string) {
	if _, ok := r.m[k]; !ok {
		return
	}
	delete(r.m, k)
	for i, key := range r.keys {
		if key == k {
			r.keys = append(r.keys[:i:i], r.keys[i+1:]...)
			break
		}
	}
}

func (r *RefMap) Keys() []string { return r.keys }

func (r *RefMap) Len() int { return len(r.keys) }

// LabelMap is an insertion-ordered lowercased name -> label map.
type LabelMap struct {
	keys []string
	m    map[string]*Label
}

func newLabelMap() *LabelMap { return &LabelMap{m: map[string]*Label{}} }

func (l *LabelMap) Get(k string) *Label { return l.m[k] }

func (l *LabelMap) Set(k string, v *Label) {
	if _, ok := l.m[k]; !ok {
		l.keys = append(l.keys, k)
	}
	l.m[k] = v
}

func (l *LabelMap) Delete(k string) {
	if _, ok := l.m[k]; !ok {
		return
	}
	delete(l.m, k)
	for i, key := range l.keys {
		if key == k {
			l.keys = append(l.keys[:i:i], l.keys[i+1:]...)
			break
		}
	}
}

func (l *LabelMap) Values() []*Label {
	out := make([]*Label, 0, len(l.keys))
	for _, k := range l.keys {
		out = append(out, l.m[k])
	}
	return out
}

type Repo struct {
	ID            int
	NodeID        string
	Owner         *User
	Name          string
	Description   *string
	Homepage      *string
	Private       bool
	DefaultBranch string
	HasIssues     bool
	HasWiki       bool
	HasProjects   bool
	Archived      bool
	Refs          *RefMap
	Version       int
	CreatedAt     int64
	UpdatedAt     int64
	PushedAt      int64
	Issues        map[int]*Issue
	IssueList     []*Issue
	NextNumber    int
	Labels        *LabelMap
	json          []byte
	Deleted       bool
}

type FileChange struct {
	Filename    string
	Status      string
	Additions   int
	Deletions   int
	Changes     int
	Sha         string
	PreviousSha string // "": null
}

type ForgeError struct {
	Status  int
	Message string
	Errors  []any
}

func (e *ForgeError) Error() string { return e.Message }

func ferr(status int, message string, errs ...map[string]any) *ForgeError {
	var list []any
	for _, e := range errs {
		list = append(list, orderedErr(e))
	}
	return &ForgeError{Status: status, Message: message, Errors: list}
}

func encodeNodeID(prefix, kind string, id int) string {
	return prefix + "_" + base64.RawURLEncoding.EncodeToString([]byte("0:"+kind+":"+strconv.Itoa(id)))
}

type nodeRef struct {
	Kind string
	ID   int
}

func decodeNodeID(nodeID string) *nodeRef {
	us := strings.IndexByte(nodeID, '_')
	if us < 0 {
		return nil
	}
	raw := strings.TrimRight(nodeID[us+1:], "=")
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(raw)
		if err != nil {
			return nil
		}
	}
	parts := strings.Split(string(data), ":")
	if len(parts) != 3 || parts[0] != "0" {
		return nil
	}
	id, err := strconv.Atoi(parts[2])
	if err != nil {
		return nil
	}
	return &nodeRef{Kind: parts[1], ID: id}
}

func iso(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05Z")
}

// splitLines counts the lines of a text blob ("a\nb" and "a\nb\n" are both two lines); nil for binary.
func splitLines(data []byte) ([]string, bool) {
	if bytes.IndexByte(data, 0) >= 0 {
		return nil, false
	}
	text := string(data)
	if text == "" {
		return []string{}, true
	}
	lines := strings.Split(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines, true
}

// editDistance is the Myers edit distance between two line arrays (insertions + deletions).
func editDistance(a, b []string) int {
	n, m := len(a), len(b)
	max := n + m
	if max == 0 {
		return 0
	}
	offset := max + 1
	v := make([]int, 2*max+3)
	for d := 0; d <= max; d++ {
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				x = v[offset+k+1]
			} else {
				x = v[offset+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[offset+k] = x
			if x >= n && y >= m {
				return d
			}
		}
		if d > 20000 {
			break
		}
	}
	return n + m
}

func (f *Forge) lineChanges(before, after string) (int, int) {
	var a, b []string
	okA, okB := true, true
	if before != "" {
		var data []byte
		if o := f.store.Get(before); o != nil {
			data = o.Data
		}
		a, okA = splitLines(data)
	}
	if after != "" {
		var data []byte
		if o := f.store.Get(after); o != nil {
			data = o.Data
		}
		b, okB = splitLines(data)
	}
	if !okA || !okB {
		return 0, 0
	}
	if before == "" {
		return len(b), 0
	}
	if after == "" {
		return 0, len(a)
	}
	d := editDistance(a, b)
	common := (len(a) + len(b) - d) / 2
	return len(b) - common, len(a) - common
}

type Forge struct {
	store         *ObjectStore
	viewer        *User
	users         map[string]*User
	userOrder     []*User
	repos         map[string]*Repo
	reposByID     map[int]*Repo
	repoOrder     []*Repo
	issuesByID    map[int]*Issue
	pullsByID     map[int]*Issue
	commentsByID  map[int]*Comment
	labelsByID    map[int]*Label
	nextRepoID    int
	nextIssueID   int
	nextPullID    int
	nextCommentID int
	nextLabelID   int
	lastNow       int64
	lastCommitSec int64
}

type accountSpec struct {
	Login string
	Type  string
}

func NewForge(viewerLogin, viewerName, viewerEmail string, others []accountSpec) *Forge {
	f := &Forge{
		store:         NewObjectStore(),
		users:         map[string]*User{},
		repos:         map[string]*Repo{},
		reposByID:     map[int]*Repo{},
		issuesByID:    map[int]*Issue{},
		pullsByID:     map[int]*Issue{},
		commentsByID:  map[int]*Comment{},
		labelsByID:    map[int]*Label{},
		nextRepoID:    700001,
		nextIssueID:   2100001,
		nextPullID:    3100001,
		nextCommentID: 4100001,
		nextLabelID:   5100001,
	}
	nextUserID := 1000
	make_ := func(login, typ, name, email string) *User {
		id := nextUserID
		nextUserID++
		prefix := "U"
		if typ != "User" {
			prefix = "O"
		}
		u := &User{ID: id, NodeID: encodeNodeID(prefix, typ, id), Login: login, Name: name, Email: email, Type: typ}
		f.users[strings.ToLower(login)] = u
		f.userOrder = append(f.userOrder, u)
		return u
	}
	f.viewer = make_(viewerLogin, "User", viewerName, viewerEmail)
	for _, o := range others {
		make_(o.Login, o.Type, o.Login, o.Login+"@example.invalid")
	}
	return f
}

func (f *Forge) now() int64 {
	t := time.Now().UnixMilli()
	if t <= f.lastNow {
		t = f.lastNow + 1
	}
	f.lastNow = t
	return t
}

func (f *Forge) user(login string) *User { return f.users[strings.ToLower(login)] }

// ---------------------------------------------------------------- repositories

func (f *Forge) repo(owner, name string) *Repo {
	r := f.repos[strings.ToLower(owner)+"/"+strings.ToLower(name)]
	if r == nil || r.Deleted {
		return nil
	}
	return r
}

// visibleRepo is a repository the viewer may see (the viewer's own or an organization's).
func (f *Forge) visibleRepo(owner, name string) *Repo {
	r := f.repo(owner, name)
	if r == nil {
		return nil
	}
	if r.Owner != f.viewer && r.Owner.Type != "Organization" {
		return nil
	}
	return r
}

var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

type createRepoOptions struct {
	Name          any
	Description   *string
	Private       bool
	AutoInit      bool
	DefaultBranch string
	Homepage      *string
	HasIssues     bool
}

func (f *Forge) createRepo(owner *User, opts createRepoOptions) (*Repo, error) {
	name, ok := opts.Name.(string)
	if !ok || strings.TrimSpace(name) == "" || !repoNameRe.MatchString(name) || name == "." || name == ".." || len(name) > 100 {
		return nil, ferr(422, "Repository creation failed.", map[string]any{"resource": "Repository", "code": "custom", "field": "name", "message": "name is invalid"})
	}
	key := strings.ToLower(owner.Login) + "/" + strings.ToLower(name)
	if existing := f.repos[key]; existing != nil && !existing.Deleted {
		return nil, ferr(422, "Repository creation failed.", map[string]any{"resource": "Repository", "code": "custom", "field": "name", "message": "name already exists on this account"})
	}
	id := f.nextRepoID
	f.nextRepoID++
	now := f.now()
	branch := opts.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	r := &Repo{
		ID: id, NodeID: encodeNodeID("R", "Repository", id), Owner: owner, Name: name,
		Description: opts.Description, Homepage: opts.Homepage, Private: opts.Private, DefaultBranch: branch,
		HasIssues: opts.HasIssues, HasWiki: true, HasProjects: true, Refs: newRefMap(),
		CreatedAt: now, UpdatedAt: now, PushedAt: now, Issues: map[int]*Issue{}, NextNumber: 1, Labels: newLabelMap(),
	}
	f.repos[key] = r
	f.reposByID[id] = r
	f.repoOrder = append(f.repoOrder, r)
	if opts.AutoInit {
		text := "# " + name + "\n"
		if r.Description != nil && *r.Description != "" {
			text += "\n" + *r.Description + "\n"
		}
		readme := f.store.Put(ObjBlob, []byte(text))
		files := NewFlatFiles()
		files.Set("README.md", FileEntry{Mode: "100644", Oid: readme})
		f.commitTree(r, r.DefaultBranch, files, "Initial commit", nil, nil)
	}
	return r, nil
}

func (f *Forge) deleteRepo(r *Repo) {
	r.Deleted = true
	delete(f.repos, strings.ToLower(r.Owner.Login)+"/"+strings.ToLower(r.Name))
	delete(f.reposByID, r.ID)
	for _, issue := range r.IssueList {
		delete(f.issuesByID, issue.ID)
		if issue.Pull != nil {
			delete(f.pullsByID, issue.Pull.ID)
		}
		for _, c := range issue.Comments {
			delete(f.commentsByID, c.ID)
		}
	}
	for _, l := range r.Labels.Values() {
		delete(f.labelsByID, l.ID)
	}
}

func (f *Forge) renameRepo(r *Repo, name string) error {
	if name == r.Name {
		return nil
	}
	if !repoNameRe.MatchString(name) {
		return ferr(422, "Validation Failed", map[string]any{"resource": "Repository", "code": "invalid", "field": "name"})
	}
	key := strings.ToLower(r.Owner.Login) + "/" + strings.ToLower(name)
	if clash := f.repos[key]; clash != nil && clash != r {
		return ferr(422, "Repository creation failed.", map[string]any{"resource": "Repository", "code": "custom", "field": "name", "message": "name already exists on this account"})
	}
	delete(f.repos, strings.ToLower(r.Owner.Login)+"/"+strings.ToLower(r.Name))
	r.Name = name
	f.repos[key] = r
	f.touchRepo(r)
	return nil
}

func (f *Forge) touchRepo(r *Repo) {
	r.UpdatedAt = f.now()
	r.json = nil
	for _, issue := range r.IssueList {
		issue.json = nil
		if issue.Pull != nil {
			issue.Pull.json = nil
		}
	}
}

// viewerRepos lists every repository the viewer owns, in creation order.
func (f *Forge) viewerRepos() []*Repo {
	var out []*Repo
	for _, r := range f.repoOrder {
		if !r.Deleted && f.reposByID[r.ID] == r && r.Owner == f.viewer {
			out = append(out, r)
		}
	}
	return out
}

// ---------------------------------------------------------------- refs and commits

func (f *Forge) setRef(r *Repo, ref string, oid string) {
	if oid == "" || oid == ZeroOid {
		r.Refs.Delete(ref)
	} else {
		r.Refs.Set(ref, oid)
	}
	r.Version++
	r.PushedAt = f.now()
	r.UpdatedAt = r.PushedAt
	r.json = nil
	if oid != "" && oid != ZeroOid && strings.HasPrefix(ref, "refs/heads/") && !r.Refs.Has("refs/heads/"+r.DefaultBranch) {
		branches := 0
		for _, name := range r.Refs.Keys() {
			if strings.HasPrefix(name, "refs/heads/") {
				branches++
			}
		}
		if branches == 1 {
			r.DefaultBranch = ref[len("refs/heads/"):]
		}
	}
	for _, issue := range r.IssueList {
		if issue.Pull != nil && issue.Pull.json != nil && issue.State == "open" {
			issue.Pull.json = nil
		}
	}
}

func (f *Forge) branchTip(r *Repo, branch string) (string, bool) {
	return r.Refs.Get("refs/heads/" + branch)
}

var fullShaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)
var shortShaRe = regexp.MustCompile(`^[0-9a-f]{4,39}$`)

// resolveCommitish resolves a branch name, tag name, full ref or commit sha to a commit id ("" when none).
func (f *Forge) resolveCommitish(r *Repo, ref string) string {
	if ref == "" || ref == "HEAD" {
		tip, _ := f.branchTip(r, r.DefaultBranch)
		return tip
	}
	if v, ok := r.Refs.Get(ref); ok {
		return f.store.Peel(v)
	}
	if v, ok := r.Refs.Get("refs/heads/" + ref); ok {
		return f.store.Peel(v)
	}
	if v, ok := r.Refs.Get("refs/tags/" + ref); ok {
		return f.store.Peel(v)
	}
	if fullShaRe.MatchString(ref) && f.store.Commit(ref) != nil {
		return ref
	}
	if shortShaRe.MatchString(ref) {
		for _, k := range r.Refs.Keys() {
			oid, _ := r.Refs.Get(k)
			if strings.HasPrefix(oid, ref) {
				return f.store.Peel(oid)
			}
		}
	}
	return ""
}

func (f *Forge) signature() Signature {
	sec := time.Now().Unix()
	if sec <= f.lastCommitSec {
		sec = f.lastCommitSec + 1
	}
	f.lastCommitSec = sec
	return Signature{Name: f.viewer.Name, Email: f.viewer.Email, Time: sec, TZ: "+0000"}
}

// commitTree writes a commit with files as its whole tree on branch (parent: the branch tip unless given).
func (f *Forge) commitTree(r *Repo, branch string, files *FlatFiles, message string, parents []string, author *Signature) string {
	tree := buildTree(f.store, files)
	tip, hasTip := f.branchTip(r, branch)
	sig := f.signature()
	if parents == nil {
		parents = []string{}
		if hasTip {
			parents = []string{tip}
		}
	}
	a := sig
	if author != nil {
		a = *author
	}
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	c := &Commit{Tree: tree, Parents: parents, Author: a, Committer: sig, Message: message}
	oid := f.store.Put(ObjCommit, serializeCommit(c))
	f.setRef(r, "refs/heads/"+branch, oid)
	return oid
}

func (f *Forge) filesAt(commitOid string) *FlatFiles {
	if commitOid == "" {
		return NewFlatFiles()
	}
	c := f.store.Commit(commitOid)
	if c == nil {
		return NewFlatFiles()
	}
	return flattenTree(f.store, c.Tree, "", nil)
}

// ---------------------------------------------------------------- labels

var hashPrefixRe = regexp.MustCompile(`^#`)

func (f *Forge) createLabel(r *Repo, name any, color any, description any) (*Label, error) {
	n, ok := name.(string)
	if !ok || strings.TrimSpace(n) == "" {
		return nil, ferr(422, "Validation Failed", map[string]any{"resource": "Label", "code": "missing_field", "field": "name"})
	}
	if r.Labels.Get(strings.ToLower(n)) != nil {
		return nil, ferr(422, "Validation Failed", map[string]any{"resource": "Label", "code": "already_exists", "field": "name"})
	}
	id := f.nextLabelID
	f.nextLabelID++
	c := "ededed"
	if s, ok := color.(string); ok {
		c = s
	} else if !isUndef(color) && color != nil {
		c = jsString(color)
	}
	var desc *string
	if s, ok := description.(string); ok {
		desc = &s
	}
	l := &Label{ID: id, NodeID: encodeNodeID("LA", "Label", id), Name: n, Color: hashPrefixRe.ReplaceAllString(c, ""), Description: desc}
	r.Labels.Set(strings.ToLower(n), l)
	f.labelsByID[id] = l
	return l, nil
}

func (f *Forge) resolveLabels(r *Repo, names any) ([]*Label, error) {
	if isUndef(names) || names == nil {
		return []*Label{}, nil
	}
	list, ok := names.([]any)
	if !ok {
		return nil, ferr(422, "Invalid request.\n\nFor 'properties/labels', it is not an array.")
	}
	out := []*Label{}
	for _, raw := range list {
		name := ""
		ok := false
		if s, isStr := raw.(string); isStr {
			name, ok = s, true
		} else if m, isObj := raw.(map[string]any); isObj {
			if s, isStr := m["name"].(string); isStr {
				name, ok = s, true
			}
		}
		if !ok {
			return nil, ferr(422, "Validation Failed", map[string]any{"resource": "Label", "code": "invalid", "field": "labels"})
		}
		l := r.Labels.Get(strings.ToLower(name))
		if l == nil {
			return nil, ferr(422, "Validation Failed", map[string]any{"value": name, "resource": "Label", "field": "labels", "code": "invalid", "message": "Label '" + name + "' does not exist"})
		}
		dup := false
		for _, x := range out {
			if x == l {
				dup = true
			}
		}
		if !dup {
			out = append(out, l)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- issues

func (f *Forge) createIssue(r *Repo, title, body, labels any) (*Issue, error) {
	var t string
	switch v := title.(type) {
	case string:
		t = v
	case float64:
		t = jsString(v)
	default:
		return nil, ferr(422, "Validation Failed", map[string]any{"resource": "Issue", "code": "missing_field", "field": "title"})
	}
	if strings.TrimSpace(t) == "" {
		return nil, ferr(422, "Validation Failed", map[string]any{"resource": "Issue", "code": "missing_field", "field": "title"})
	}
	if !isUndef(body) && body != nil {
		if _, ok := body.(string); !ok {
			return nil, ferr(422, "Invalid request.\n\nFor 'properties/body', it is not a string.")
		}
	}
	ls, err := f.resolveLabels(r, labels)
	if err != nil {
		return nil, err
	}
	now := f.now()
	id := f.nextIssueID
	f.nextIssueID++
	var b *string
	if s, ok := body.(string); ok {
		b = &s
	}
	issue := &Issue{
		ID: id, NodeID: encodeNodeID("I", "Issue", id), Repo: r, Number: r.NextNumber, Title: t, Body: b,
		State: "open", Labels: ls, Comments: []*Comment{}, User: f.viewer, CreatedAt: now, UpdatedAt: now,
	}
	r.NextNumber++
	r.Issues[issue.Number] = issue
	r.IssueList = append(r.IssueList, issue)
	f.issuesByID[id] = issue
	r.json = nil
	return issue, nil
}

type issuePatch struct {
	Title, Body, State, StateReason, Labels any
}

func newIssuePatch() issuePatch {
	return issuePatch{Title: Undefined, Body: Undefined, State: Undefined, StateReason: Undefined, Labels: Undefined}
}

func (f *Forge) updateIssue(issue *Issue, p issuePatch) error {
	if !isUndef(p.Title) {
		s, ok := p.Title.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return ferr(422, "Validation Failed", map[string]any{"resource": "Issue", "code": "missing_field", "field": "title"})
		}
		issue.Title = s
	}
	if !isUndef(p.Body) {
		if p.Body == nil {
			issue.Body = nil
		} else if s, ok := p.Body.(string); ok {
			issue.Body = &s
		} else {
			return ferr(422, "Invalid request.\n\nFor 'properties/body', it is not a string.")
		}
	}
	if !isUndef(p.Labels) {
		ls, err := f.resolveLabels(issue.Repo, p.Labels)
		if err != nil {
			return err
		}
		issue.Labels = ls
	}
	if !isUndef(p.State) {
		state := ""
		if s, ok := p.State.(string); ok {
			state = strings.ToLower(s)
		}
		if state != "open" && state != "closed" {
			return ferr(422, "Validation Failed", map[string]any{"resource": "Issue", "code": "invalid", "field": "state"})
		}
		if state == "closed" && issue.State != "closed" {
			issue.State = "closed"
			issue.ClosedAt = f.now()
			reason := "completed"
			if s, ok := p.StateReason.(string); ok {
				reason = s
			}
			issue.StateReason = &reason
			if issue.Pull != nil {
				f.freezePull(issue)
			}
		} else if state == "open" && issue.State != "open" {
			if issue.Pull != nil && issue.Pull.Merged {
				return ferr(422, "Validation Failed", map[string]any{"resource": "PullRequest", "code": "custom", "field": "state", "message": "state cannot be changed. The pull request has been merged."})
			}
			issue.State = "open"
			issue.ClosedAt = 0
			reason := "reopened"
			issue.StateReason = &reason
		}
	} else if s, ok := p.StateReason.(string); ok && issue.State == "closed" {
		issue.StateReason = &s
	}
	f.touchIssue(issue)
	return nil
}

func (f *Forge) touchIssue(issue *Issue) {
	issue.UpdatedAt = f.now()
	issue.json = nil
	if issue.Pull != nil {
		issue.Pull.json = nil
	}
	issue.Repo.json = nil
}

func (f *Forge) addComment(issue *Issue, body any) (*Comment, error) {
	s, ok := body.(string)
	if !ok || s == "" {
		return nil, ferr(422, "Validation Failed", map[string]any{"resource": "IssueComment", "code": "missing_field", "field": "body"})
	}
	now := f.now()
	id := f.nextCommentID
	f.nextCommentID++
	c := &Comment{ID: id, NodeID: encodeNodeID("IC", "IssueComment", id), Body: s, User: f.viewer, CreatedAt: now, UpdatedAt: now, Issue: issue}
	issue.Comments = append(issue.Comments, c)
	f.commentsByID[id] = c
	f.touchIssue(issue)
	return c, nil
}

// ---------------------------------------------------------------- pull requests

func afterColon(s string) string {
	if i := strings.IndexByte(s, ':'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func (f *Forge) createPull(r *Repo, title, body, headIn, baseIn, draft any) (*Issue, error) {
	headSpec, _ := headIn.(string)
	baseSpec, _ := baseIn.(string)
	head := afterColon(headSpec)
	base := afterColon(baseSpec)
	if t, ok := title.(string); !ok || strings.TrimSpace(t) == "" {
		return nil, ferr(422, "Validation Failed", map[string]any{"resource": "PullRequest", "code": "missing_field", "field": "title"})
	}
	if head == "" {
		return nil, ferr(422, "Validation Failed", map[string]any{"resource": "PullRequest", "code": "missing_field", "field": "head"})
	}
	if base == "" {
		return nil, ferr(422, "Validation Failed", map[string]any{"resource": "PullRequest", "code": "missing_field", "field": "base"})
	}
	headSha, okH := f.branchTip(r, head)
	baseSha, okB := f.branchTip(r, base)
	if !okH {
		return nil, ferr(422, "Validation Failed", map[string]any{"resource": "PullRequest", "code": "invalid", "field": "head"})
	}
	if !okB {
		return nil, ferr(422, "Validation Failed", map[string]any{"resource": "PullRequest", "code": "invalid", "field": "base"})
	}
	if head == base {
		return nil, ferr(422, "Validation Failed", map[string]any{"resource": "PullRequest", "code": "custom", "message": "No commits between " + base + " and " + head})
	}
	issue, err := f.createIssue(r, title, body, Undefined)
	if err != nil {
		return nil, err
	}
	id := f.nextPullID
	f.nextPullID++
	d, _ := draft.(bool)
	issue.Pull = &PullData{ID: id, NodeID: encodeNodeID("PR", "PullRequest", id), Head: head, Base: base, HeadSha: headSha, BaseSha: baseSha, Draft: d}
	f.pullsByID[id] = issue
	return issue, nil
}

func (f *Forge) pullHeadSha(issue *Issue) string {
	p := issue.Pull
	if issue.State == "open" {
		if tip, ok := f.branchTip(issue.Repo, p.Head); ok {
			return tip
		}
	}
	return p.HeadSha
}

func (f *Forge) pullBaseSha(issue *Issue) string {
	p := issue.Pull
	if issue.State == "open" {
		if tip, ok := f.branchTip(issue.Repo, p.Base); ok {
			return tip
		}
	}
	return p.BaseSha
}

func (f *Forge) freezePull(issue *Issue) {
	p := issue.Pull
	if tip, ok := f.branchTip(issue.Repo, p.Head); ok {
		p.HeadSha = tip
	}
	if tip, ok := f.branchTip(issue.Repo, p.Base); ok {
		p.BaseSha = tip
	}
}

func (f *Forge) pullBaseCommit(issue *Issue, baseSha string) string {
	p := issue.Pull
	if p.Merged {
		if c := f.store.Commit(p.MergeCommitSha); c != nil && len(c.Parents) > 0 {
			return c.Parents[0]
		}
	}
	return baseSha
}

func (f *Forge) pullFiles(issue *Issue) []FileChange {
	p := issue.Pull
	headSha := f.pullHeadSha(issue)
	baseSha := f.pullBaseSha(issue)
	key := headSha + ":" + baseSha
	if p.files != nil && p.filesKey == key {
		return p.files
	}
	baseCommit := f.pullBaseCommit(issue, baseSha)
	base := mergeBase(f.store, baseCommit, headSha)
	if base == "" {
		base = baseCommit
	}
	before := f.filesAt(base)
	after := f.filesAt(headSha)
	set := map[string]bool{}
	for _, k := range before.Keys {
		set[k] = true
	}
	for _, k := range after.Keys {
		set[k] = true
	}
	paths := make([]string, 0, len(set))
	for k := range set {
		paths = append(paths, k)
	}
	sort.Strings(paths)
	changes := []FileChange{}
	for _, path := range paths {
		a, okA := before.Get(path)
		b, okB := after.Get(path)
		if okA && okB && a.Oid == b.Oid && a.Mode == b.Mode {
			continue
		}
		var aOid, bOid string
		if okA {
			aOid = a.Oid
		}
		if okB {
			bOid = b.Oid
		}
		add, del := f.lineChanges(aOid, bOid)
		status := "modified"
		if !okA {
			status = "added"
		} else if !okB {
			status = "removed"
		}
		sha := bOid
		if !okB {
			sha = aOid
		}
		changes = append(changes, FileChange{Filename: path, Status: status, Additions: add, Deletions: del, Changes: add + del, Sha: sha, PreviousSha: aOid})
	}
	p.files = changes
	p.filesKey = key
	return changes
}

// pullCommits lists the commits a pull carries (head's history not reachable from base), oldest first.
func (f *Forge) pullCommits(issue *Issue) []string {
	headSha := f.pullHeadSha(issue)
	baseSha := f.pullBaseSha(issue)
	baseCommit := f.pullBaseCommit(issue, baseSha)
	exclude := map[string]bool{}
	stack := []string{baseCommit}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if exclude[oid] {
			continue
		}
		exclude[oid] = true
		if c := f.store.Commit(oid); c != nil {
			stack = append(stack, c.Parents...)
		}
	}
	var out []string
	seen := map[string]bool{}
	walk := []string{headSha}
	for len(walk) > 0 {
		oid := walk[len(walk)-1]
		walk = walk[:len(walk)-1]
		if seen[oid] || exclude[oid] {
			continue
		}
		seen[oid] = true
		c := f.store.Commit(oid)
		if c == nil {
			continue
		}
		out = append(out, oid)
		walk = append(walk, c.Parents...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return f.store.Commit(out[i]).Committer.Time < f.store.Commit(out[j]).Committer.Time
	})
	return out
}

func sameEntry(x FileEntry, okX bool, y FileEntry, okY bool) bool {
	return (!okX && !okY) || (okX && okY && x.Oid == y.Oid && x.Mode == y.Mode)
}

// mergeTree is the three-way tree merge of a pull: nil when a path changed differently on both sides.
func (f *Forge) mergeTree(issue *Issue) *FlatFiles {
	headSha := f.pullHeadSha(issue)
	baseSha := f.pullBaseSha(issue)
	if isAncestor(f.store, baseSha, headSha) {
		return f.filesAt(headSha)
	}
	ancestor := mergeBase(f.store, baseSha, headSha)
	origin := f.filesAt(ancestor)
	ours := f.filesAt(baseSha)
	theirs := f.filesAt(headSha)
	result := ours.Clone()
	var paths []string
	seen := map[string]bool{}
	for _, list := range [][]string{origin.Keys, ours.Keys, theirs.Keys} {
		for _, k := range list {
			if !seen[k] {
				seen[k] = true
				paths = append(paths, k)
			}
		}
	}
	for _, path := range paths {
		o, okO := origin.Get(path)
		a, okA := ours.Get(path)
		b, okB := theirs.Get(path)
		if sameEntry(a, okA, b, okB) {
			continue
		}
		if sameEntry(o, okO, b, okB) {
			continue
		}
		if sameEntry(o, okO, a, okA) {
			if !okB {
				result.Delete(path)
			} else {
				result.Set(path, b)
			}
			continue
		}
		return nil
	}
	return result
}

func (f *Forge) mergeable(issue *Issue) bool {
	if issue.Pull == nil || issue.Pull.Merged || issue.State != "open" {
		return false
	}
	return f.mergeTree(issue) != nil
}

func (f *Forge) mergePull(issue *Issue, methodIn, titleIn, messageIn, shaIn any) (string, error) {
	p := issue.Pull
	if p.Merged || issue.State != "open" {
		return "", ferr(405, "Pull Request is not mergeable")
	}
	method := "merge"
	if s, ok := methodIn.(string); ok && s != "" {
		method = strings.ToLower(s)
	}
	if method != "merge" && method != "squash" && method != "rebase" {
		return "", ferr(422, "Validation Failed", map[string]any{"resource": "PullRequest", "code": "invalid", "field": "merge_method"})
	}
	headSha := f.pullHeadSha(issue)
	baseSha := f.pullBaseSha(issue)
	if s, ok := shaIn.(string); ok && s != "" && s != headSha {
		return "", ferr(409, "Head branch was modified. Review and try the merge again.")
	}
	files := f.mergeTree(issue)
	if files == nil {
		return "", ferr(405, "Pull Request is not mergeable")
	}
	title := ""
	if s, ok := titleIn.(string); ok && s != "" {
		title = s
	} else if method == "squash" {
		title = issue.Title + " (#" + strconv.Itoa(issue.Number) + ")"
	} else {
		title = "Merge pull request #" + strconv.Itoa(issue.Number) + " from " + issue.Repo.Owner.Login + "/" + p.Head
	}
	extra := ""
	if s, ok := messageIn.(string); ok && s != "" {
		extra = s
	} else if method != "squash" {
		extra = issue.Title
	}
	message := title + "\n"
	if extra != "" {
		message = title + "\n\n" + extra + "\n"
	}
	var sha string
	switch method {
	case "merge":
		sha = f.commitTree(issue.Repo, p.Base, files, message, []string{baseSha, headSha}, nil)
	case "squash":
		sha = f.commitTree(issue.Repo, p.Base, files, message, []string{baseSha}, nil)
	default:
		parent := baseSha
		current := f.filesAt(baseSha)
		sha = baseSha
		for _, oid := range f.pullCommits(issue) {
			c := f.store.Commit(oid)
			first := ""
			if len(c.Parents) > 0 {
				first = c.Parents[0]
			}
			before := f.filesAt(first)
			after := f.filesAt(oid)
			next := current.Clone()
			for _, path := range after.Keys {
				e := after.M[path]
				if be, ok := before.Get(path); !ok || be.Oid != e.Oid {
					next.Set(path, e)
				}
			}
			for _, path := range before.Keys {
				if _, ok := after.Get(path); !ok {
					next.Delete(path)
				}
			}
			author := c.Author
			sha = f.commitTree(issue.Repo, p.Base, next, c.Message, []string{parent}, &author)
			parent = sha
			current = next
		}
	}
	p.HeadSha = headSha
	p.BaseSha = baseSha
	p.Merged = true
	p.MergedAt = f.now()
	p.MergeCommitSha = sha
	p.MergedBy = f.viewer
	issue.State = "closed"
	issue.ClosedAt = p.MergedAt
	issue.StateReason = nil
	f.touchIssue(issue)
	return sha, nil
}
