// REST v3 JSON views (GitHub's shapes). Repository, issue and pull views are cached as strings on the
// objects and dropped whenever the object (or, for a pull, its branches) changes.

import { iso, type Comment, type Forge, type Issue, type Label, type Repo, type User } from './forge.ts';
import type { Signature } from './git.ts';

function signatureJson(sig: Signature): { name: string; email: string; date: string } {
  return { name: sig.name, email: sig.email, date: iso(sig.time * 1000)! };
}

export class Views {
  private readonly forge: Forge;
  /** Origin the URLs are built on, e.g. "http://127.0.0.1:41234". */
  origin = 'http://localhost';
  api = 'http://localhost/api/v3';

  constructor(forge: Forge) {
    this.forge = forge;
  }

  /** Adopt the request's Host as the URL origin; a change drops every cached view. */
  setHost(host: string | undefined): void {
    if (host === undefined || host === '') return;
    const origin = `http://${host}`;
    if (origin === this.origin) return;
    this.origin = origin;
    this.api = `${origin}/api/v3`;
    for (const repo of this.forge.reposById.values()) this.forge.touchRepo(repo);
  }

  user(user: User): Record<string, unknown> {
    return {
      login: user.login,
      id: user.id,
      node_id: user.nodeId,
      avatar_url: `${this.origin}/avatars/${user.login}`,
      gravatar_id: '',
      url: `${this.api}/users/${user.login}`,
      html_url: `${this.origin}/${user.login}`,
      repos_url: `${this.api}/users/${user.login}/repos`,
      type: user.type,
      user_view_type: 'public',
      site_admin: false,
    };
  }

  userFull(user: User): Record<string, unknown> {
    let publicRepos = 0;
    let privateRepos = 0;
    for (const repo of this.forge.reposById.values()) {
      if (repo.owner !== user) continue;
      if (repo.private) privateRepos += 1;
      else publicRepos += 1;
    }
    return {
      ...this.user(user),
      name: user.name,
      company: null,
      blog: '',
      location: null,
      email: user.email,
      hireable: null,
      bio: null,
      twitter_username: null,
      public_repos: publicRepos,
      public_gists: 0,
      followers: 0,
      following: 0,
      created_at: '2026-01-01T00:00:00Z',
      updated_at: '2026-01-01T00:00:00Z',
      ...(user === this.forge.viewer ? { total_private_repos: privateRepos, owned_private_repos: privateRepos, two_factor_authentication: false, plan: { name: 'free', space: 976562499, collaborators: 0, private_repos: 10000 } } : {}),
    };
  }

  repoObject(repo: Repo): Record<string, unknown> {
    const full = `${repo.owner.login}/${repo.name}`;
    const url = `${this.api}/repos/${full}`;
    let open = 0;
    for (const issue of repo.issueList) if (issue.state === 'open') open += 1;
    return {
      id: repo.id,
      node_id: repo.nodeId,
      name: repo.name,
      full_name: full,
      private: repo.private,
      owner: this.user(repo.owner),
      html_url: `${this.origin}/${full}`,
      description: repo.description,
      fork: false,
      url,
      issues_url: `${url}/issues{/number}`,
      pulls_url: `${url}/pulls{/number}`,
      contents_url: `${url}/contents/{+path}`,
      branches_url: `${url}/branches{/branch}`,
      commits_url: `${url}/commits{/sha}`,
      labels_url: `${url}/labels{/name}`,
      git_refs_url: `${url}/git/refs{/sha}`,
      created_at: iso(repo.createdAt),
      updated_at: iso(repo.updatedAt),
      pushed_at: iso(repo.pushedAt),
      git_url: `git://${this.origin.replace(/^https?:\/\//, '')}/${full}.git`,
      ssh_url: `git@localhost:${full}.git`,
      clone_url: `${this.origin}/${full}.git`,
      homepage: repo.homepage,
      size: 0,
      stargazers_count: 0,
      watchers_count: 0,
      language: null,
      has_issues: repo.hasIssues,
      has_projects: repo.hasProjects,
      has_downloads: true,
      has_wiki: repo.hasWiki,
      has_pages: false,
      has_discussions: false,
      forks_count: 0,
      mirror_url: null,
      archived: repo.archived,
      disabled: false,
      open_issues_count: open,
      license: null,
      allow_forking: true,
      is_template: false,
      web_commit_signoff_required: false,
      topics: [],
      visibility: repo.private ? 'private' : 'public',
      forks: 0,
      open_issues: open,
      watchers: 0,
      default_branch: repo.defaultBranch,
      permissions: { admin: true, maintain: true, push: true, triage: true, pull: true },
      allow_squash_merge: true,
      allow_merge_commit: true,
      allow_rebase_merge: true,
      delete_branch_on_merge: false,
      network_count: 0,
      subscribers_count: 1,
    };
  }

  repo(repo: Repo): string {
    if (repo.json === undefined) repo.json = JSON.stringify(this.repoObject(repo));
    return repo.json;
  }

  label(repo: Repo, label: Label): Record<string, unknown> {
    return {
      id: label.id,
      node_id: label.nodeId,
      url: `${this.api}/repos/${repo.owner.login}/${repo.name}/labels/${encodeURIComponent(label.name)}`,
      name: label.name,
      color: label.color,
      default: false,
      description: label.description,
    };
  }

  issueObject(issue: Issue): Record<string, unknown> {
    const repo = issue.repo;
    const repoUrl = `${this.api}/repos/${repo.owner.login}/${repo.name}`;
    const url = `${repoUrl}/issues/${issue.number}`;
    const html = `${this.origin}/${repo.owner.login}/${repo.name}/${issue.pull === null ? 'issues' : 'pull'}/${issue.number}`;
    const out: Record<string, unknown> = {
      url,
      repository_url: repoUrl,
      labels_url: `${url}/labels{/name}`,
      comments_url: `${url}/comments`,
      events_url: `${url}/events`,
      html_url: html,
      id: issue.id,
      node_id: issue.nodeId,
      number: issue.number,
      title: issue.title,
      user: this.user(issue.user),
      labels: issue.labels.map((label) => this.label(repo, label)),
      state: issue.state,
      locked: false,
      assignee: null,
      assignees: [],
      milestone: null,
      comments: issue.comments.length,
      created_at: iso(issue.createdAt),
      updated_at: iso(issue.updatedAt),
      closed_at: iso(issue.closedAt),
      author_association: 'OWNER',
      type: null,
      active_lock_reason: null,
      sub_issues_summary: { total: 0, completed: 0, percent_completed: 0 },
      body: issue.body,
      closed_by: issue.state === 'closed' ? this.user(this.forge.viewer) : null,
      reactions: { url: `${url}/reactions`, total_count: 0, '+1': 0, '-1': 0, laugh: 0, hooray: 0, confused: 0, heart: 0, rocket: 0, eyes: 0 },
      timeline_url: `${url}/timeline`,
      performed_via_github_app: null,
      state_reason: issue.stateReason,
    };
    if (issue.pull !== null) {
      out.draft = issue.pull.draft;
      out.pull_request = {
        url: `${repoUrl}/pulls/${issue.number}`,
        html_url: html,
        diff_url: `${html}.diff`,
        patch_url: `${html}.patch`,
        merged_at: iso(issue.pull.mergedAt),
      };
    }
    return out;
  }

  issue(issue: Issue): string {
    if (issue.json === undefined) issue.json = JSON.stringify(this.issueObject(issue));
    return issue.json;
  }

  comment(comment: Comment): Record<string, unknown> {
    const repo = comment.issue.repo;
    const repoUrl = `${this.api}/repos/${repo.owner.login}/${repo.name}`;
    return {
      url: `${repoUrl}/issues/comments/${comment.id}`,
      html_url: `${this.origin}/${repo.owner.login}/${repo.name}/issues/${comment.issue.number}#issuecomment-${comment.id}`,
      issue_url: `${repoUrl}/issues/${comment.issue.number}`,
      id: comment.id,
      node_id: comment.nodeId,
      user: this.user(comment.user),
      created_at: iso(comment.createdAt),
      updated_at: iso(comment.updatedAt),
      author_association: 'OWNER',
      body: comment.body,
      reactions: { url: `${repoUrl}/issues/comments/${comment.id}/reactions`, total_count: 0, '+1': 0, '-1': 0, laugh: 0, hooray: 0, confused: 0, heart: 0, rocket: 0, eyes: 0 },
      performed_via_github_app: null,
    };
  }

  pullObject(issue: Issue): Record<string, unknown> {
    const forge = this.forge;
    const pull = issue.pull!;
    const repo = issue.repo;
    const repoUrl = `${this.api}/repos/${repo.owner.login}/${repo.name}`;
    const url = `${repoUrl}/pulls/${issue.number}`;
    const html = `${this.origin}/${repo.owner.login}/${repo.name}/pull/${issue.number}`;
    const headSha = forge.pullHeadSha(issue);
    const baseSha = forge.pullBaseSha(issue);
    const files = forge.pullFiles(issue);
    let additions = 0;
    let deletions = 0;
    for (const file of files) {
      additions += file.additions;
      deletions += file.deletions;
    }
    const repoObject = this.repoObject(repo);
    const mergeable = issue.state === 'open' ? forge.mergeable(issue) : pull.merged ? null : forge.mergeTree(issue) !== null;
    return {
      url,
      id: pull.id,
      node_id: pull.nodeId,
      html_url: html,
      diff_url: `${html}.diff`,
      patch_url: `${html}.patch`,
      issue_url: `${repoUrl}/issues/${issue.number}`,
      number: issue.number,
      state: issue.state,
      locked: false,
      title: issue.title,
      user: this.user(issue.user),
      body: issue.body,
      created_at: iso(issue.createdAt),
      updated_at: iso(issue.updatedAt),
      closed_at: iso(issue.closedAt),
      merged_at: iso(pull.mergedAt),
      merge_commit_sha: pull.mergeCommitSha,
      assignee: null,
      assignees: [],
      requested_reviewers: [],
      requested_teams: [],
      labels: issue.labels.map((label) => this.label(repo, label)),
      milestone: null,
      draft: pull.draft,
      commits_url: `${url}/commits`,
      review_comments_url: `${url}/comments`,
      review_comment_url: `${repoUrl}/pulls/comments{/number}`,
      comments_url: `${repoUrl}/issues/${issue.number}/comments`,
      statuses_url: `${repoUrl}/statuses/${headSha}`,
      head: { label: `${repo.owner.login}:${pull.head}`, ref: pull.head, sha: headSha, user: this.user(repo.owner), repo: repoObject },
      base: { label: `${repo.owner.login}:${pull.base}`, ref: pull.base, sha: baseSha, user: this.user(repo.owner), repo: repoObject },
      _links: {
        self: { href: url },
        html: { href: html },
        issue: { href: `${repoUrl}/issues/${issue.number}` },
        comments: { href: `${repoUrl}/issues/${issue.number}/comments` },
        commits: { href: `${url}/commits` },
        statuses: { href: `${repoUrl}/statuses/${headSha}` },
      },
      author_association: 'OWNER',
      auto_merge: null,
      active_lock_reason: null,
      merged: pull.merged,
      mergeable,
      rebaseable: mergeable,
      mergeable_state: pull.merged ? 'unknown' : mergeable === true ? 'clean' : mergeable === false ? 'dirty' : 'unknown',
      merged_by: pull.mergedBy === null ? null : this.user(pull.mergedBy),
      comments: issue.comments.length,
      review_comments: 0,
      maintainer_can_modify: false,
      commits: forge.pullCommits(issue).length,
      additions,
      deletions,
      changed_files: files.length,
    };
  }

  pull(issue: Issue): string {
    const pull = issue.pull!;
    if (pull.json === undefined || pull.jsonVersion !== issue.repo.version) {
      pull.json = JSON.stringify(this.pullObject(issue));
      pull.jsonVersion = issue.repo.version;
    }
    return pull.json;
  }

  commit(repo: Repo, oid: string): Record<string, unknown> | null {
    const commit = this.forge.store.commit(oid);
    if (commit === null) return null;
    const repoUrl = `${this.api}/repos/${repo.owner.login}/${repo.name}`;
    const authorUser = commit.author.email === this.forge.viewer.email ? this.user(this.forge.viewer) : null;
    const committerUser = commit.committer.email === this.forge.viewer.email ? this.user(this.forge.viewer) : null;
    const message = commit.message.replace(/\n+$/, '');
    return {
      sha: oid,
      node_id: `C_${Buffer.from(`0:Commit:${oid}`).toString('base64url')}`,
      commit: {
        author: signatureJson(commit.author),
        committer: signatureJson(commit.committer),
        message,
        tree: { sha: commit.tree, url: `${repoUrl}/git/trees/${commit.tree}` },
        url: `${repoUrl}/git/commits/${oid}`,
        comment_count: 0,
        verification: { verified: false, reason: 'unsigned', signature: null, payload: null, verified_at: null },
      },
      url: `${repoUrl}/commits/${oid}`,
      html_url: `${this.origin}/${repo.owner.login}/${repo.name}/commit/${oid}`,
      comments_url: `${repoUrl}/commits/${oid}/comments`,
      author: authorUser,
      committer: committerUser,
      parents: commit.parents.map((parent) => ({ sha: parent, url: `${repoUrl}/commits/${parent}`, html_url: `${this.origin}/${repo.owner.login}/${repo.name}/commit/${parent}` })),
    };
  }

  gitCommit(repo: Repo, oid: string): Record<string, unknown> | null {
    const commit = this.forge.store.commit(oid);
    if (commit === null) return null;
    const repoUrl = `${this.api}/repos/${repo.owner.login}/${repo.name}`;
    return {
      sha: oid,
      node_id: `C_${Buffer.from(`0:Commit:${oid}`).toString('base64url')}`,
      url: `${repoUrl}/git/commits/${oid}`,
      html_url: `${this.origin}/${repo.owner.login}/${repo.name}/commit/${oid}`,
      author: signatureJson(commit.author),
      committer: signatureJson(commit.committer),
      tree: { sha: commit.tree, url: `${repoUrl}/git/trees/${commit.tree}` },
      message: commit.message.replace(/\n+$/, ''),
      parents: commit.parents.map((parent) => ({ sha: parent, url: `${repoUrl}/git/commits/${parent}`, html_url: `${this.origin}/${repo.owner.login}/${repo.name}/commit/${parent}` })),
      verification: { verified: false, reason: 'unsigned', signature: null, payload: null, verified_at: null },
    };
  }

  ref(repo: Repo, name: string, oid: string): Record<string, unknown> {
    const repoUrl = `${this.api}/repos/${repo.owner.login}/${repo.name}`;
    const type = this.forge.store.get(oid)?.type === 4 ? 'tag' : 'commit';
    return {
      ref: name,
      node_id: `REF_${Buffer.from(`0:Ref:${repo.id}:${name}`).toString('base64url')}`,
      url: `${repoUrl}/git/${name}`,
      object: { sha: oid, type, url: `${repoUrl}/git/${type === 'tag' ? 'tags' : 'commits'}/${oid}` },
    };
  }

  branchShort(repo: Repo, name: string, oid: string): Record<string, unknown> {
    return { name, commit: { sha: oid, url: `${this.api}/repos/${repo.owner.login}/${repo.name}/commits/${oid}` }, protected: false };
  }

  branchFull(repo: Repo, name: string, oid: string): Record<string, unknown> {
    const repoUrl = `${this.api}/repos/${repo.owner.login}/${repo.name}`;
    return {
      name,
      commit: this.commit(repo, oid),
      _links: { self: `${repoUrl}/branches/${name}`, html: `${this.origin}/${repo.owner.login}/${repo.name}/tree/${name}` },
      protected: false,
      protection: { enabled: false, required_status_checks: { enforcement_level: 'off', contexts: [], checks: [] } },
      protection_url: `${repoUrl}/branches/${name}/protection`,
    };
  }

  contentEntry(repo: Repo, path: string, mode: string, oid: string, ref: string): Record<string, unknown> {
    const repoUrl = `${this.api}/repos/${repo.owner.login}/${repo.name}`;
    const type = mode === '40000' ? 'dir' : mode === '120000' ? 'symlink' : mode === '160000' ? 'submodule' : 'file';
    const name = path.slice(path.lastIndexOf('/') + 1);
    const size = type === 'file' || type === 'symlink' ? (this.forge.store.get(oid)?.data.length ?? 0) : 0;
    const self = `${repoUrl}/contents/${path}?ref=${encodeURIComponent(ref)}`;
    const git = `${repoUrl}/git/${type === 'dir' ? 'trees' : 'blobs'}/${oid}`;
    const html = `${this.origin}/${repo.owner.login}/${repo.name}/${type === 'dir' ? 'tree' : 'blob'}/${ref}/${path}`;
    return {
      name,
      path,
      sha: oid,
      size,
      url: self,
      html_url: html,
      git_url: git,
      download_url: type === 'file' ? `${this.origin}/${repo.owner.login}/${repo.name}/raw/${ref}/${path}` : null,
      type,
      _links: { self, git, html },
    };
  }
}
