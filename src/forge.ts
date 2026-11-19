// The forge's domain: users, repositories with refs, labels, issues, pull requests and comments, plus the
// commit-producing operations (contents writes, squash/merge/rebase merges). Every surface (REST, GraphQL,
// MCP, git) reads and writes the same objects here, so an object created on one is the same on the others.

import {
  ObjectStore,
  OBJ_BLOB,
  OBJ_COMMIT,
  ZERO_OID,
  buildTree,
  flattenTree,
  isAncestor,
  mergeBase,
  serializeCommit,
  type Commit,
  type Signature,
} from './git.ts';

export interface User {
  id: number;
  nodeId: string;
  login: string;
  name: string;
  email: string;
  type: 'User' | 'Organization';
}

export interface Label {
  id: number;
  nodeId: string;
  name: string;
  color: string;
  description: string | null;
}

export interface Comment {
  id: number;
  nodeId: string;
  body: string;
  user: User;
  createdAt: number;
  updatedAt: number;
  issue: Issue;
}

export interface PullData {
  id: number;
  nodeId: string;
  head: string;
  base: string;
  /** The tips recorded when the pull closed (open pulls read the live branch tips). */
  headSha: string;
  baseSha: string;
  merged: boolean;
  mergedAt: number | null;
  mergeCommitSha: string | null;
  mergedBy: User | null;
  draft: boolean;
  json?: string;
  jsonVersion?: number;
  files?: FileChange[];
  filesKey?: string;
}

export interface Issue {
  id: number;
  nodeId: string;
  repo: Repo;
  number: number;
  title: string;
  body: string | null;
  state: 'open' | 'closed';
  stateReason: string | null;
  labels: Label[];
  comments: Comment[];
  user: User;
  createdAt: number;
  updatedAt: number;
  closedAt: number | null;
  pull: PullData | null;
  /** Cached REST JSON (issue view); dropped on every change. */
  json?: string;
}

export interface Repo {
  id: number;
  nodeId: string;
  owner: User;
  name: string;
  description: string | null;
  homepage: string | null;
  private: boolean;
  defaultBranch: string;
  hasIssues: boolean;
  hasWiki: boolean;
  hasProjects: boolean;
  archived: boolean;
  refs: Map<string, string>;
  /** Bumped on every ref change: keys the pack and listing caches. */
  version: number;
  createdAt: number;
  updatedAt: number;
  pushedAt: number;
  issues: Map<number, Issue>;
  issueList: Issue[];
  nextNumber: number;
  labels: Map<string, Label>;
  json?: string;
  deleted: boolean;
}

export interface FileChange {
  filename: string;
  status: 'added' | 'removed' | 'modified';
  additions: number;
  deletions: number;
  changes: number;
  sha: string;
  previousSha: string | null;
}

export class ForgeError extends Error {
  readonly status: number;
  readonly errors?: unknown[];
  constructor(status: number, message: string, errors?: unknown[]) {
    super(message);
    this.status = status;
    this.errors = errors;
  }
}

export function encodeNodeId(prefix: string, kind: string, id: number): string {
  return `${prefix}_${Buffer.from(`0:${kind}:${id}`).toString('base64url')}`;
}

export function decodeNodeId(nodeId: string): { kind: string; id: number } | null {
  const underscore = nodeId.indexOf('_');
  if (underscore < 0) return null;
  try {
    const text = Buffer.from(nodeId.slice(underscore + 1), 'base64url').toString('utf8');
    const parts = text.split(':');
    if (parts.length !== 3 || parts[0] !== '0') return null;
    const id = Number.parseInt(parts[2]!, 10);
    if (!Number.isFinite(id)) return null;
    return { kind: parts[1]!, id };
  } catch {
    return null;
  }
}

export function iso(ms: number | null): string | null {
  if (ms === null) return null;
  const text = new Date(ms).toISOString();
  return `${text.slice(0, 19)}Z`;
}

/** Line counts for a text blob ("a\nb" and "a\nb\n" are both two lines). */
function splitLines(data: Buffer): string[] | null {
  if (data.includes(0)) return null;
  const text = data.toString('utf8');
  if (text === '') return [];
  const lines = text.split('\n');
  if (lines[lines.length - 1] === '') lines.pop();
  return lines;
}

/** The Myers edit distance between two line arrays (insertions + deletions). */
function editDistance(a: string[], b: string[]): number {
  const n = a.length;
  const m = b.length;
  const max = n + m;
  if (max === 0) return 0;
  const offset = max + 1;
  const v = new Int32Array(2 * max + 3);
  for (let d = 0; d <= max; d += 1) {
    for (let k = -d; k <= d; k += 2) {
      let x: number;
      if (k === -d || (k !== d && v[offset + k - 1]! < v[offset + k + 1]!)) x = v[offset + k + 1]!;
      else x = v[offset + k - 1]! + 1;
      let y = x - k;
      while (x < n && y < m && a[x] === b[y]) {
        x += 1;
        y += 1;
      }
      v[offset + k] = x;
      if (x >= n && y >= m) return d;
    }
    if (d > 20000) break;
  }
  return n + m;
}

function lineChanges(store: ObjectStore, before: string | null, after: string | null): { additions: number; deletions: number } {
  const a = before === null ? [] : splitLines(store.get(before)?.data ?? Buffer.alloc(0));
  const b = after === null ? [] : splitLines(store.get(after)?.data ?? Buffer.alloc(0));
  if (a === null || b === null) return { additions: 0, deletions: 0 };
  if (before === null) return { additions: b.length, deletions: 0 };
  if (after === null) return { additions: 0, deletions: a.length };
  const d = editDistance(a, b);
  const common = (a.length + b.length - d) / 2;
  return { additions: b.length - common, deletions: a.length - common };
}

export class Forge {
  readonly store = new ObjectStore();
  readonly viewer: User;
  readonly users = new Map<string, User>();
  readonly repos = new Map<string, Repo>();
  readonly reposById = new Map<number, Repo>();
  readonly issuesById = new Map<number, Issue>();
  readonly pullsById = new Map<number, Issue>();
  readonly commentsById = new Map<number, Comment>();
  readonly labelsById = new Map<number, Label>();
  private nextRepoId = 700001;
  private nextIssueId = 2100001;
  private nextPullId = 3100001;
  private nextCommentId = 4100001;
  private nextLabelId = 5100001;
  /** Monotonic clock: two objects created in the same millisecond still order by creation. */
  private lastNow = 0;
  /** Seconds for server-made commits, strictly increasing so history orders by time. */
  private lastCommitTime = 0;

  constructor(viewerLogin: string, viewerName: string, viewerEmail: string, others: Array<{ login: string; type: 'User' | 'Organization' }>) {
    let nextUserId = 1000;
    const make = (login: string, type: 'User' | 'Organization', name: string, email: string): User => {
      const id = nextUserId;
      nextUserId += 1;
      const user: User = { id, nodeId: encodeNodeId(type === 'User' ? 'U' : 'O', type, id), login, name, email, type };
      this.users.set(login.toLowerCase(), user);
      return user;
    };
    this.viewer = make(viewerLogin, 'User', viewerName, viewerEmail);
    for (const other of others) make(other.login, other.type, other.login, `${other.login}@example.invalid`);
  }

  now(): number {
    let t = Date.now();
    if (t <= this.lastNow) t = this.lastNow + 1;
    this.lastNow = t;
    return t;
  }

  user(login: string): User | undefined {
    return this.users.get(login.toLowerCase());
  }

  // ---------------------------------------------------------------- repositories

  repo(owner: string, name: string): Repo | undefined {
    const repo = this.repos.get(`${owner.toLowerCase()}/${name.toLowerCase()}`);
    return repo === undefined || repo.deleted ? undefined : repo;
  }

  /** A repository the viewer may see (every repository here is the viewer's or its orgs'). */
  visibleRepo(owner: string, name: string): Repo | undefined {
    const repo = this.repo(owner, name);
    if (repo === undefined) return undefined;
    if (repo.owner !== this.viewer && repo.owner.type !== 'Organization') return undefined;
    return repo;
  }

  createRepo(owner: User, options: { name: string; description?: string | null; private?: boolean; autoInit?: boolean; defaultBranch?: string; homepage?: string | null; hasIssues?: boolean }): Repo {
    const name = options.name;
    if (typeof name !== 'string' || name.trim() === '' || !/^[A-Za-z0-9._-]+$/.test(name) || name === '.' || name === '..' || name.length > 100) {
      throw new ForgeError(422, 'Repository creation failed.', [{ resource: 'Repository', code: 'custom', field: 'name', message: 'name is invalid' }]);
    }
    const key = `${owner.login.toLowerCase()}/${name.toLowerCase()}`;
    const existing = this.repos.get(key);
    if (existing !== undefined && !existing.deleted) {
      throw new ForgeError(422, 'Repository creation failed.', [{ resource: 'Repository', code: 'custom', field: 'name', message: 'name already exists on this account' }]);
    }
    const id = this.nextRepoId;
    this.nextRepoId += 1;
    const now = this.now();
    const repo: Repo = {
      id,
      nodeId: encodeNodeId('R', 'Repository', id),
      owner,
      name,
      description: options.description ?? null,
      homepage: options.homepage ?? null,
      private: options.private === true,
      defaultBranch: options.defaultBranch !== undefined && options.defaultBranch !== '' ? options.defaultBranch : 'main',
      hasIssues: options.hasIssues !== false,
      hasWiki: true,
      hasProjects: true,
      archived: false,
      refs: new Map(),
      version: 0,
      createdAt: now,
      updatedAt: now,
      pushedAt: now,
      issues: new Map(),
      issueList: [],
      nextNumber: 1,
      labels: new Map(),
      deleted: false,
    };
    this.repos.set(key, repo);
    this.reposById.set(id, repo);
    if (options.autoInit === true) {
      const readme = this.store.put(OBJ_BLOB, Buffer.from(`# ${name}\n${repo.description !== null && repo.description !== '' ? `\n${repo.description}\n` : ''}`));
      this.commitTree(repo, repo.defaultBranch, new Map([['README.md', { mode: '100644', oid: readme }]]), 'Initial commit', null);
    }
    return repo;
  }

  deleteRepo(repo: Repo): void {
    repo.deleted = true;
    this.repos.delete(`${repo.owner.login.toLowerCase()}/${repo.name.toLowerCase()}`);
    this.reposById.delete(repo.id);
    for (const issue of repo.issueList) {
      this.issuesById.delete(issue.id);
      if (issue.pull !== null) this.pullsById.delete(issue.pull.id);
      for (const comment of issue.comments) this.commentsById.delete(comment.id);
    }
    for (const label of repo.labels.values()) this.labelsById.delete(label.id);
  }

  renameRepo(repo: Repo, name: string): void {
    if (name === repo.name) return;
    if (!/^[A-Za-z0-9._-]+$/.test(name)) throw new ForgeError(422, 'Validation Failed', [{ resource: 'Repository', code: 'invalid', field: 'name' }]);
    const key = `${repo.owner.login.toLowerCase()}/${name.toLowerCase()}`;
    const clash = this.repos.get(key);
    if (clash !== undefined && clash !== repo) throw new ForgeError(422, 'Repository creation failed.', [{ resource: 'Repository', code: 'custom', field: 'name', message: 'name already exists on this account' }]);
    this.repos.delete(`${repo.owner.login.toLowerCase()}/${repo.name.toLowerCase()}`);
    repo.name = name;
    this.repos.set(key, repo);
    this.touchRepo(repo);
  }

  touchRepo(repo: Repo): void {
    repo.updatedAt = this.now();
    repo.json = undefined;
    for (const issue of repo.issueList) {
      issue.json = undefined;
      if (issue.pull !== null) issue.pull.json = undefined;
    }
  }

  /** Every repository the viewer owns, in creation order. */
  viewerRepos(): Repo[] {
    const out: Repo[] = [];
    for (const repo of this.reposById.values()) if (repo.owner === this.viewer) out.push(repo);
    return out;
  }

  // ---------------------------------------------------------------- refs and commits

  setRef(repo: Repo, ref: string, oid: string | null): void {
    if (oid === null || oid === ZERO_OID) repo.refs.delete(ref);
    else repo.refs.set(ref, oid);
    repo.version += 1;
    repo.pushedAt = this.now();
    repo.updatedAt = repo.pushedAt;
    repo.json = undefined;
    if (oid !== null && ref.startsWith('refs/heads/') && !repo.refs.has(`refs/heads/${repo.defaultBranch}`)) {
      // The first branch of an empty repository becomes its default, as GitHub does.
      let branches = 0;
      for (const name of repo.refs.keys()) if (name.startsWith('refs/heads/')) branches += 1;
      if (branches === 1) repo.defaultBranch = ref.slice('refs/heads/'.length);
    }
    for (const issue of repo.issueList) if (issue.pull !== null && issue.pull.json !== undefined && issue.state === 'open') issue.pull.json = undefined;
  }

  branchTip(repo: Repo, branch: string): string | undefined {
    return repo.refs.get(`refs/heads/${branch}`);
  }

  /** Resolve a branch name, tag name, full ref or commit sha to a commit id. */
  resolveCommitish(repo: Repo, ref: string): string | undefined {
    if (ref === '' || ref === 'HEAD') return this.branchTip(repo, repo.defaultBranch);
    const direct = repo.refs.get(ref) ?? repo.refs.get(`refs/heads/${ref}`) ?? repo.refs.get(`refs/tags/${ref}`);
    if (direct !== undefined) return this.store.peel(direct);
    if (/^[0-9a-f]{40}$/.test(ref) && this.store.commit(ref) !== null) return ref;
    if (/^[0-9a-f]{4,39}$/.test(ref)) {
      for (const oid of repo.refs.values()) if (oid.startsWith(ref)) return this.store.peel(oid);
    }
    return undefined;
  }

  signature(): Signature {
    let seconds = Math.floor(Date.now() / 1000);
    if (seconds <= this.lastCommitTime) seconds = this.lastCommitTime + 1;
    this.lastCommitTime = seconds;
    return { name: this.viewer.name, email: this.viewer.email, time: seconds, tz: '+0000' };
  }

  /** Write a commit with `files` as its whole tree on `branch` (parent: the branch tip unless given). */
  commitTree(repo: Repo, branch: string, files: Map<string, { mode: string; oid: string }>, message: string, parents: string[] | null, author?: Signature): string {
    const tree = buildTree(this.store, files);
    const tip = this.branchTip(repo, branch);
    const sig = this.signature();
    const commit: Commit = {
      tree,
      parents: parents ?? (tip === undefined ? [] : [tip]),
      author: author ?? sig,
      committer: sig,
      message: message.endsWith('\n') ? message : `${message}\n`,
    };
    const oid = this.store.put(OBJ_COMMIT, serializeCommit(commit));
    this.setRef(repo, `refs/heads/${branch}`, oid);
    return oid;
  }

  /** The flattened file map of a commit (empty for no commit). */
  filesAt(commitOid: string | undefined): Map<string, { mode: string; oid: string }> {
    if (commitOid === undefined) return new Map();
    const commit = this.store.commit(commitOid);
    if (commit === null) return new Map();
    return flattenTree(this.store, commit.tree);
  }

  // ---------------------------------------------------------------- labels

  createLabel(repo: Repo, name: string, color: string | undefined, description: string | null | undefined): Label {
    if (typeof name !== 'string' || name.trim() === '') throw new ForgeError(422, 'Validation Failed', [{ resource: 'Label', code: 'missing_field', field: 'name' }]);
    if (repo.labels.has(name.toLowerCase())) throw new ForgeError(422, 'Validation Failed', [{ resource: 'Label', code: 'already_exists', field: 'name' }]);
    const id = this.nextLabelId;
    this.nextLabelId += 1;
    const label: Label = { id, nodeId: encodeNodeId('LA', 'Label', id), name, color: (color ?? 'ededed').replace(/^#/, ''), description: description ?? null };
    repo.labels.set(name.toLowerCase(), label);
    this.labelsById.set(id, label);
    return label;
  }

  resolveLabels(repo: Repo, names: unknown): Label[] {
    if (names === undefined || names === null) return [];
    if (!Array.isArray(names)) throw new ForgeError(422, 'Invalid request.\n\nFor \'properties/labels\', it is not an array.');
    const out: Label[] = [];
    for (const raw of names) {
      const name = typeof raw === 'string' ? raw : raw !== null && typeof raw === 'object' && typeof (raw as { name?: unknown }).name === 'string' ? (raw as { name: string }).name : null;
      if (name === null) throw new ForgeError(422, 'Validation Failed', [{ resource: 'Label', code: 'invalid', field: 'labels' }]);
      const label = repo.labels.get(name.toLowerCase());
      if (label === undefined) {
        throw new ForgeError(422, 'Validation Failed', [{ value: name, resource: 'Label', field: 'labels', code: 'invalid', message: `Label '${name}' does not exist` }]);
      }
      if (!out.includes(label)) out.push(label);
    }
    return out;
  }

  // ---------------------------------------------------------------- issues

  createIssue(repo: Repo, input: { title: unknown; body?: unknown; labels?: unknown }): Issue {
    if (typeof input.title !== 'string' && typeof input.title !== 'number') {
      throw new ForgeError(422, 'Validation Failed', [{ resource: 'Issue', code: 'missing_field', field: 'title' }]);
    }
    const title = String(input.title);
    if (title.trim() === '') throw new ForgeError(422, 'Validation Failed', [{ resource: 'Issue', code: 'missing_field', field: 'title' }]);
    if (input.body !== undefined && input.body !== null && typeof input.body !== 'string') {
      throw new ForgeError(422, 'Invalid request.\n\nFor \'properties/body\', it is not a string.');
    }
    const labels = this.resolveLabels(repo, input.labels);
    const now = this.now();
    const id = this.nextIssueId;
    this.nextIssueId += 1;
    const issue: Issue = {
      id,
      nodeId: encodeNodeId('I', 'Issue', id),
      repo,
      number: repo.nextNumber,
      title,
      body: typeof input.body === 'string' ? input.body : null,
      state: 'open',
      stateReason: null,
      labels,
      comments: [],
      user: this.viewer,
      createdAt: now,
      updatedAt: now,
      closedAt: null,
      pull: null,
    };
    repo.nextNumber += 1;
    repo.issues.set(issue.number, issue);
    repo.issueList.push(issue);
    this.issuesById.set(id, issue);
    repo.json = undefined;
    return issue;
  }

  updateIssue(issue: Issue, patch: { title?: unknown; body?: unknown; state?: unknown; stateReason?: unknown; labels?: unknown }): void {
    if (patch.title !== undefined) {
      if (typeof patch.title !== 'string' || patch.title.trim() === '') throw new ForgeError(422, 'Validation Failed', [{ resource: 'Issue', code: 'missing_field', field: 'title' }]);
      issue.title = patch.title;
    }
    if (patch.body !== undefined) {
      if (patch.body !== null && typeof patch.body !== 'string') throw new ForgeError(422, 'Invalid request.\n\nFor \'properties/body\', it is not a string.');
      issue.body = patch.body as string | null;
    }
    if (patch.labels !== undefined) issue.labels = this.resolveLabels(issue.repo, patch.labels);
    if (patch.state !== undefined) {
      const state = typeof patch.state === 'string' ? patch.state.toLowerCase() : '';
      if (state !== 'open' && state !== 'closed') throw new ForgeError(422, 'Validation Failed', [{ resource: 'Issue', code: 'invalid', field: 'state' }]);
      if (state === 'closed' && issue.state !== 'closed') {
        issue.state = 'closed';
        issue.closedAt = this.now();
        issue.stateReason = typeof patch.stateReason === 'string' ? patch.stateReason : 'completed';
        if (issue.pull !== null) this.freezePull(issue);
      } else if (state === 'open' && issue.state !== 'open') {
        if (issue.pull !== null && issue.pull.merged) throw new ForgeError(422, 'Validation Failed', [{ resource: 'PullRequest', code: 'custom', field: 'state', message: 'state cannot be changed. The pull request has been merged.' }]);
        issue.state = 'open';
        issue.closedAt = null;
        issue.stateReason = 'reopened';
      }
    } else if (patch.stateReason !== undefined && issue.state === 'closed' && typeof patch.stateReason === 'string') {
      issue.stateReason = patch.stateReason;
    }
    this.touchIssue(issue);
  }

  touchIssue(issue: Issue): void {
    issue.updatedAt = this.now();
    issue.json = undefined;
    if (issue.pull !== null) issue.pull.json = undefined;
    issue.repo.json = undefined;
  }

  addComment(issue: Issue, body: unknown): Comment {
    if (typeof body !== 'string' || body === '') throw new ForgeError(422, 'Validation Failed', [{ resource: 'IssueComment', code: 'missing_field', field: 'body' }]);
    const now = this.now();
    const id = this.nextCommentId;
    this.nextCommentId += 1;
    const comment: Comment = { id, nodeId: encodeNodeId('IC', 'IssueComment', id), body, user: this.viewer, createdAt: now, updatedAt: now, issue };
    issue.comments.push(comment);
    this.commentsById.set(id, comment);
    this.touchIssue(issue);
    return comment;
  }

  // ---------------------------------------------------------------- pull requests

  createPull(repo: Repo, input: { title?: unknown; body?: unknown; head?: unknown; base?: unknown; draft?: unknown }): Issue {
    const headSpec = typeof input.head === 'string' ? input.head : '';
    const baseSpec = typeof input.base === 'string' ? input.base : '';
    const head = headSpec.includes(':') ? headSpec.slice(headSpec.indexOf(':') + 1) : headSpec;
    const base = baseSpec.includes(':') ? baseSpec.slice(baseSpec.indexOf(':') + 1) : baseSpec;
    if (typeof input.title !== 'string' || input.title.trim() === '') throw new ForgeError(422, 'Validation Failed', [{ resource: 'PullRequest', code: 'missing_field', field: 'title' }]);
    if (head === '') throw new ForgeError(422, 'Validation Failed', [{ resource: 'PullRequest', code: 'missing_field', field: 'head' }]);
    if (base === '') throw new ForgeError(422, 'Validation Failed', [{ resource: 'PullRequest', code: 'missing_field', field: 'base' }]);
    const headSha = this.branchTip(repo, head);
    const baseSha = this.branchTip(repo, base);
    if (headSha === undefined) throw new ForgeError(422, 'Validation Failed', [{ resource: 'PullRequest', code: 'invalid', field: 'head' }]);
    if (baseSha === undefined) throw new ForgeError(422, 'Validation Failed', [{ resource: 'PullRequest', code: 'invalid', field: 'base' }]);
    if (head === base) throw new ForgeError(422, 'Validation Failed', [{ resource: 'PullRequest', code: 'custom', message: `No commits between ${base} and ${head}` }]);
    const issue = this.createIssue(repo, { title: input.title, body: input.body });
    const id = this.nextPullId;
    this.nextPullId += 1;
    issue.pull = {
      id,
      nodeId: encodeNodeId('PR', 'PullRequest', id),
      head,
      base,
      headSha,
      baseSha,
      merged: false,
      mergedAt: null,
      mergeCommitSha: null,
      mergedBy: null,
      draft: input.draft === true,
    };
    this.pullsById.set(id, issue);
    return issue;
  }

  pullHeadSha(issue: Issue): string {
    const pull = issue.pull!;
    if (issue.state === 'open') return this.branchTip(issue.repo, pull.head) ?? pull.headSha;
    return pull.headSha;
  }

  pullBaseSha(issue: Issue): string {
    const pull = issue.pull!;
    if (issue.state === 'open') return this.branchTip(issue.repo, pull.base) ?? pull.baseSha;
    return pull.baseSha;
  }

  /** Record the branch tips when a pull closes, so later branch moves do not rewrite it. */
  private freezePull(issue: Issue): void {
    const pull = issue.pull!;
    pull.headSha = this.branchTip(issue.repo, pull.head) ?? pull.headSha;
    pull.baseSha = this.branchTip(issue.repo, pull.base) ?? pull.baseSha;
  }

  /** The files a pull request changes: head against its merge base with base. */
  pullFiles(issue: Issue): FileChange[] {
    const pull = issue.pull!;
    const headSha = this.pullHeadSha(issue);
    const baseSha = this.pullBaseSha(issue);
    const key = `${headSha}:${baseSha}`;
    if (pull.files !== undefined && pull.filesKey === key) return pull.files;
    const baseCommit = pull.merged ? (this.store.commit(pull.mergeCommitSha ?? '')?.parents[0] ?? baseSha) : baseSha;
    const base = mergeBase(this.store, baseCommit, headSha) ?? baseCommit;
    const before = this.filesAt(base);
    const after = this.filesAt(headSha);
    const changes: FileChange[] = [];
    const paths = new Set<string>([...before.keys(), ...after.keys()]);
    for (const path of [...paths].sort()) {
      const a = before.get(path);
      const b = after.get(path);
      if (a !== undefined && b !== undefined && a.oid === b.oid && a.mode === b.mode) continue;
      const counts = lineChanges(this.store, a?.oid ?? null, b?.oid ?? null);
      changes.push({
        filename: path,
        status: a === undefined ? 'added' : b === undefined ? 'removed' : 'modified',
        additions: counts.additions,
        deletions: counts.deletions,
        changes: counts.additions + counts.deletions,
        sha: b?.oid ?? a!.oid,
        previousSha: a?.oid ?? null,
      });
    }
    pull.files = changes;
    pull.filesKey = key;
    return changes;
  }

  /** The commits a pull carries (head's history not reachable from base), oldest first. */
  pullCommits(issue: Issue): string[] {
    const headSha = this.pullHeadSha(issue);
    const baseSha = this.pullBaseSha(issue);
    const pull = issue.pull!;
    const baseCommit = pull.merged ? (this.store.commit(pull.mergeCommitSha ?? '')?.parents[0] ?? baseSha) : baseSha;
    const exclude = new Set<string>();
    const stack = [baseCommit];
    while (stack.length > 0) {
      const oid = stack.pop()!;
      if (exclude.has(oid)) continue;
      exclude.add(oid);
      const commit = this.store.commit(oid);
      if (commit !== null) stack.push(...commit.parents);
    }
    const out: string[] = [];
    const seen = new Set<string>();
    const walk = [headSha];
    while (walk.length > 0) {
      const oid = walk.pop()!;
      if (seen.has(oid) || exclude.has(oid)) continue;
      seen.add(oid);
      const commit = this.store.commit(oid);
      if (commit === null) continue;
      out.push(oid);
      walk.push(...commit.parents);
    }
    out.sort((a, b) => (this.store.commit(a)!.committer.time - this.store.commit(b)!.committer.time) || 0);
    return out;
  }

  /**
   * The three-way tree merge of a pull: null when a path changed differently on both sides. A path is
   * merged cleanly when only one side changed it (or both changed it identically).
   */
  mergeTree(issue: Issue): Map<string, { mode: string; oid: string }> | null {
    const headSha = this.pullHeadSha(issue);
    const baseSha = this.pullBaseSha(issue);
    if (isAncestor(this.store, baseSha, headSha)) return this.filesAt(headSha);
    const ancestor = mergeBase(this.store, baseSha, headSha);
    const origin = this.filesAt(ancestor ?? undefined);
    const ours = this.filesAt(baseSha);
    const theirs = this.filesAt(headSha);
    const result = new Map(ours);
    const paths = new Set<string>([...origin.keys(), ...ours.keys(), ...theirs.keys()]);
    for (const path of paths) {
      const o = origin.get(path);
      const a = ours.get(path);
      const b = theirs.get(path);
      const same = (x: { mode: string; oid: string } | undefined, y: { mode: string; oid: string } | undefined): boolean =>
        (x === undefined && y === undefined) || (x !== undefined && y !== undefined && x.oid === y.oid && x.mode === y.mode);
      if (same(a, b)) continue;
      if (same(o, b)) continue;
      if (same(o, a)) {
        if (b === undefined) result.delete(path);
        else result.set(path, b);
        continue;
      }
      return null;
    }
    return result;
  }

  mergeable(issue: Issue): boolean {
    if (issue.pull === null || issue.pull.merged || issue.state !== 'open') return false;
    return this.mergeTree(issue) !== null;
  }

  mergePull(issue: Issue, options: { method?: unknown; title?: unknown; message?: unknown; sha?: unknown }): { sha: string } {
    const pull = issue.pull!;
    if (pull.merged) throw new ForgeError(405, 'Pull Request is not mergeable');
    if (issue.state !== 'open') throw new ForgeError(405, 'Pull Request is not mergeable');
    const method = typeof options.method === 'string' && options.method !== '' ? options.method.toLowerCase() : 'merge';
    if (method !== 'merge' && method !== 'squash' && method !== 'rebase') {
      throw new ForgeError(422, 'Validation Failed', [{ resource: 'PullRequest', code: 'invalid', field: 'merge_method' }]);
    }
    const headSha = this.pullHeadSha(issue);
    const baseSha = this.pullBaseSha(issue);
    if (typeof options.sha === 'string' && options.sha !== '' && options.sha !== headSha) {
      throw new ForgeError(409, 'Head branch was modified. Review and try the merge again.');
    }
    const files = this.mergeTree(issue);
    if (files === null) throw new ForgeError(405, 'Pull Request is not mergeable');
    const title = typeof options.title === 'string' && options.title !== '' ? options.title : method === 'squash' ? `${issue.title} (#${issue.number})` : `Merge pull request #${issue.number} from ${issue.repo.owner.login}/${pull.head}`;
    const extra = typeof options.message === 'string' && options.message !== '' ? options.message : method === 'squash' ? '' : issue.title;
    const message = extra === '' ? `${title}\n` : `${title}\n\n${extra}\n`;
    let sha: string;
    if (method === 'merge') {
      sha = this.commitTree(issue.repo, pull.base, files, message, [baseSha, headSha]);
    } else if (method === 'squash') {
      sha = this.commitTree(issue.repo, pull.base, files, message, [baseSha]);
    } else {
      // Rebase: replay the pull's commits on top of base, each with its own message and tree delta.
      let parent = baseSha;
      let current = this.filesAt(baseSha);
      sha = baseSha;
      for (const oid of this.pullCommits(issue)) {
        const commit = this.store.commit(oid)!;
        const before = this.filesAt(commit.parents[0]);
        const after = this.filesAt(oid);
        const next = new Map(current);
        for (const [path, entry] of after) if (before.get(path)?.oid !== entry.oid) next.set(path, entry);
        for (const path of before.keys()) if (!after.has(path)) next.delete(path);
        sha = this.commitTree(issue.repo, pull.base, next, commit.message, [parent], commit.author);
        parent = sha;
        current = next;
      }
    }
    pull.headSha = headSha;
    pull.baseSha = baseSha;
    pull.merged = true;
    pull.mergedAt = this.now();
    pull.mergeCommitSha = sha;
    pull.mergedBy = this.viewer;
    issue.state = 'closed';
    issue.closedAt = pull.mergedAt;
    issue.stateReason = null;
    this.touchIssue(issue);
    return { sha };
  }
}
