// The GitHub REST v3 surface under /api/v3: repositories, git refs, branches, commits, contents, labels,
// issues, comments and pull requests. Errors are GitHub's {message, documentation_url[, errors]} bodies.

import { ForgeError, type Forge, type Issue, type Repo } from './forge.ts';
import { OBJ_BLOB, OBJ_TAG, ZERO_OID, isAncestor, logFrom, lookupPath } from './git.ts';
import type { Request, Response } from './http.ts';
import type { Views } from './views.ts';

export const JSON_TYPE = 'application/json; charset=utf-8';
const DOCS = 'https://docs.github.com/rest';

export function json(status: number, body: unknown, extra?: string): Response {
  return { status, type: JSON_TYPE, body: typeof body === 'string' ? body : JSON.stringify(body), extra };
}

export function errorJson(status: number, message: string, errors?: unknown[]): Response {
  const body: Record<string, unknown> = { message, documentation_url: DOCS, status: String(status) };
  if (errors !== undefined) body.errors = errors;
  return json(status, body);
}

export function notFound(): Response {
  return errorJson(404, 'Not Found');
}

class BadJson extends Error {}

/** The JSON object of a request body ({} when empty); throws BadJson for unparsable text. */
export function parseBody(request: Request): Record<string, unknown> {
  if (request.body.length === 0) return {};
  const text = request.body.toString('utf8').trim();
  if (text === '') return {};
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch {
    throw new BadJson('Problems parsing JSON');
  }
  if (value === null || typeof value !== 'object' || Array.isArray(value)) throw new ForgeError(422, 'Invalid request.\n\nThe request body must be a JSON object.');
  return value as Record<string, unknown>;
}

function intParam(params: URLSearchParams, name: string, fallback: number, min: number, max: number): number {
  const raw = params.get(name);
  if (raw === null || raw === '') return fallback;
  const value = Number.parseInt(raw, 10);
  if (!Number.isFinite(value)) return fallback;
  return Math.min(max, Math.max(min, value));
}

function joinJson(items: string[]): string {
  return `[${items.join(',')}]`;
}

export class Rest {
  private readonly forge: Forge;
  private readonly views: Views;

  constructor(forge: Forge, views: Views) {
    this.forge = forge;
    this.views = views;
  }

  /** Slice a list by page/per_page and build GitHub's Link header for it. */
  private paginate<T>(items: T[], request: Request, params: URLSearchParams): { page: T[]; extra?: string } {
    const perPage = intParam(params, 'per_page', 30, 1, 100);
    const page = intParam(params, 'page', 1, 1, 1_000_000);
    const start = (page - 1) * perPage;
    const slice = items.slice(start, start + perPage);
    const last = Math.max(1, Math.ceil(items.length / perPage));
    const host = request.headers.host ?? 'localhost';
    const link = (n: number): string => {
      const copy = new URLSearchParams(params);
      copy.set('per_page', String(perPage));
      copy.set('page', String(n));
      return `http://${host}${request.path}?${copy.toString()}`;
    };
    const rels: string[] = [];
    if (page < last) {
      rels.push(`<${link(page + 1)}>; rel="next"`);
      rels.push(`<${link(last)}>; rel="last"`);
    }
    if (page > 1) {
      rels.push(`<${link(1)}>; rel="first"`);
      rels.push(`<${link(Math.min(page - 1, last))}>; rel="prev"`);
    }
    return { page: slice, extra: rels.length > 0 ? `Link: ${rels.join(', ')}\r\n` : undefined };
  }

  handle(request: Request, segments: string[]): Response {
    try {
      return this.route(request, segments);
    } catch (err) {
      if (err instanceof BadJson) return errorJson(400, 'Problems parsing JSON');
      if (err instanceof ForgeError) return errorJson(err.status, err.message, err.errors);
      throw err;
    }
  }

  private route(request: Request, s: string[]): Response {
    const method = request.method;
    const n = s.length;
    const forge = this.forge;
    const views = this.views;
    const params = new URLSearchParams(request.query);

    if (n === 0 || (n === 1 && s[0] === '')) {
      if (method !== 'GET' && method !== 'HEAD') return notFound();
      const api = views.api;
      return json(
        200,
        {
          current_user_url: `${api}/user`,
          authorizations_url: `${api}/authorizations`,
          emojis_url: `${api}/emojis`,
          events_url: `${api}/events`,
          issues_url: `${api}/issues`,
          rate_limit_url: `${api}/rate_limit`,
          repository_url: `${api}/repos/{owner}/{repo}`,
          user_url: `${api}/users/{user}`,
          user_repositories_url: `${api}/users/{user}/repos{?type,page,per_page,sort}`,
          graphql_url: `${views.origin}/api/graphql`,
        },
        'X-OAuth-Scopes: repo, read:org, workflow, gist\r\nX-Accepted-OAuth-Scopes: \r\nX-GitHub-Media-Type: github.v3; format=json\r\n',
      );
    }
    const head = s[0]!;

    if (head === 'meta' && n === 1 && method === 'GET') {
      return json(200, { verifiable_password_authentication: false, installed_version: '3.16.0', hooks: [], git: [], web: [], api: [], packages: [], pages: [], importer: [] });
    }
    if (head === 'rate_limit' && n === 1 && method === 'GET') {
      const reset = Math.floor(Date.now() / 1000) + 3600;
      const core = { limit: 5000, used: 0, remaining: 5000, reset };
      return json(200, { resources: { core, graphql: core, search: { ...core, limit: 30, remaining: 30 } }, rate: core });
    }
    if (head === 'zen' && n === 1) return json(200, JSON.stringify('Keep it logically awesome.'));

    if (head === 'user') {
      if (n === 1 && method === 'GET') return json(200, views.userFull(forge.viewer), 'X-OAuth-Scopes: repo, read:org, workflow, gist\r\n');
      if (n === 2 && s[1] === 'repos') {
        if (method === 'GET') return this.listRepos(request, params);
        if (method === 'POST') return this.createRepo(request, forge.viewer.login);
      }
      if (n === 2 && s[1] === 'orgs' && method === 'GET') return json(200, [...forge.users.values()].filter((u) => u.type === 'Organization').map((u) => views.user(u)));
      if (n === 2 && s[1] === 'emails' && method === 'GET') return json(200, [{ email: forge.viewer.email, primary: true, verified: true, visibility: 'private' }]);
      return notFound();
    }
    if (head === 'users' && n >= 2) {
      const user = forge.user(s[1]!);
      if (user === undefined) return notFound();
      if (n === 2 && method === 'GET') return json(200, views.userFull(user));
      if (n === 3 && s[2] === 'repos' && method === 'GET') {
        const repos = [...forge.reposById.values()].filter((repo) => repo.owner === user && (!repo.private || user === forge.viewer));
        repos.sort((a, b) => (a.name.toLowerCase() < b.name.toLowerCase() ? -1 : 1));
        const { page, extra } = this.paginate(repos, request, params);
        return json(200, joinJson(page.map((repo) => views.repo(repo))), extra);
      }
      return notFound();
    }
    if (head === 'orgs' && n >= 2) {
      const org = forge.user(s[1]!);
      if (org === undefined || org.type !== 'Organization') return notFound();
      if (n === 2 && method === 'GET') return json(200, { ...views.user(org), name: org.name, description: null });
      if (n === 3 && s[2] === 'repos') {
        if (method === 'POST') return this.createRepo(request, org.login);
        if (method === 'GET') {
          const repos = [...forge.reposById.values()].filter((repo) => repo.owner === org);
          const { page, extra } = this.paginate(repos, request, params);
          return json(200, joinJson(page.map((repo) => views.repo(repo))), extra);
        }
      }
      return notFound();
    }
    if (head === 'repos' && n >= 3) {
      const repo = forge.visibleRepo(s[1]!, s[2]!);
      if (repo === undefined) return notFound();
      return this.repoRoute(request, repo, s.slice(3), params);
    }
    return notFound();
  }

  // ---------------------------------------------------------------- repositories

  private listRepos(request: Request, params: URLSearchParams): Response {
    const forge = this.forge;
    let repos = [...forge.reposById.values()].filter((repo) => repo.owner === forge.viewer || repo.owner.type === 'Organization');
    const visibility = params.get('visibility');
    if (visibility === 'public') repos = repos.filter((repo) => !repo.private);
    else if (visibility === 'private') repos = repos.filter((repo) => repo.private);
    const sort = params.get('sort') ?? 'full_name';
    const direction = params.get('direction') ?? (sort === 'full_name' ? 'asc' : 'desc');
    const key = (repo: Repo): string | number =>
      sort === 'created' ? repo.createdAt : sort === 'updated' ? repo.updatedAt : sort === 'pushed' ? repo.pushedAt : `${repo.owner.login}/${repo.name}`.toLowerCase();
    repos.sort((a, b) => {
      const ka = key(a);
      const kb = key(b);
      const order = ka < kb ? -1 : ka > kb ? 1 : a.id - b.id;
      return direction === 'asc' ? order : -order;
    });
    const { page, extra } = this.paginate(repos, request, params);
    return json(200, joinJson(page.map((repo) => this.views.repo(repo))), extra);
  }

  private createRepo(request: Request, ownerLogin: string): Response {
    const body = parseBody(request);
    const owner = this.forge.user(ownerLogin)!;
    if (typeof body.name !== 'string') throw new ForgeError(422, 'Repository creation failed.', [{ resource: 'Repository', code: 'missing_field', field: 'name' }]);
    const repo = this.forge.createRepo(owner, {
      name: body.name,
      description: typeof body.description === 'string' ? body.description : null,
      private: body.private === true || body.visibility === 'private',
      autoInit: body.auto_init === true,
      homepage: typeof body.homepage === 'string' ? body.homepage : null,
      hasIssues: body.has_issues !== false,
      defaultBranch: typeof body.default_branch === 'string' ? body.default_branch : undefined,
    });
    return json(201, this.views.repo(repo), `Location: ${this.views.api}/repos/${repo.owner.login}/${repo.name}\r\n`);
  }

  private repoRoute(request: Request, repo: Repo, s: string[], params: URLSearchParams): Response {
    const method = request.method;
    const forge = this.forge;
    const views = this.views;
    const n = s.length;
    if (n === 0 || (n === 1 && s[0] === '')) {
      if (method === 'GET' || method === 'HEAD') return json(200, views.repo(repo));
      if (method === 'PATCH') return this.patchRepo(request, repo);
      if (method === 'DELETE') {
        forge.deleteRepo(repo);
        return { status: 204, type: JSON_TYPE, body: '' };
      }
      return notFound();
    }
    const resource = s[0]!;
    switch (resource) {
      case 'issues':
        return this.issuesRoute(request, repo, s.slice(1), params);
      case 'pulls':
        return this.pullsRoute(request, repo, s.slice(1), params);
      case 'labels':
        return this.labelsRoute(request, repo, s.slice(1), params);
      case 'contents':
        return this.contentsRoute(request, repo, s.slice(1).join('/'), params);
      case 'readme': {
        if (method !== 'GET') return notFound();
        const tip = forge.resolveCommitish(repo, params.get('ref') ?? '');
        if (tip === undefined) return errorJson(404, 'Not Found');
        const tree = forge.store.commit(tip)!.tree;
        const entries = forge.store.tree(tree) ?? [];
        const readme = entries.find((entry) => /^readme(\.|$)/i.test(entry.name) && entry.mode !== '40000');
        if (readme === undefined) return errorJson(404, 'Not Found');
        return this.contentsRoute(request, repo, readme.name, params);
      }
      case 'branches':
        return this.branchesRoute(request, repo, s.slice(1), params);
      case 'commits':
        return this.commitsRoute(request, repo, s.slice(1), params);
      case 'git':
        return this.gitRoute(request, repo, s.slice(1), params);
      case 'collaborators':
        if (method === 'GET' && n === 1) return json(200, [{ ...views.user(repo.owner), permissions: { admin: true, maintain: true, push: true, triage: true, pull: true }, role_name: 'admin' }]);
        if (method === 'GET' && n === 3 && s[2] === 'permission') return json(200, { permission: 'admin', role_name: 'admin', user: views.user(forge.viewer) });
        return notFound();
      case 'languages':
        return method === 'GET' ? json(200, {}) : notFound();
      case 'topics':
        return method === 'GET' ? json(200, { names: [] }) : notFound();
      case 'tags': {
        if (method !== 'GET') return notFound();
        const tags = [...repo.refs.entries()].filter(([name]) => name.startsWith('refs/tags/')).sort((a, b) => (a[0] < b[0] ? 1 : -1));
        const { page, extra } = this.paginate(tags, request, params);
        return json(200, page.map(([name, oid]) => ({ name: name.slice(10), commit: { sha: forge.store.peel(oid), url: `${views.api}/repos/${repo.owner.login}/${repo.name}/commits/${forge.store.peel(oid)}` }, node_id: `REF_${name}` })), extra);
      }
      default:
        return notFound();
    }
  }

  private patchRepo(request: Request, repo: Repo): Response {
    const body = parseBody(request);
    const forge = this.forge;
    if (body.name !== undefined) {
      if (typeof body.name !== 'string') throw new ForgeError(422, 'Validation Failed', [{ resource: 'Repository', code: 'invalid', field: 'name' }]);
      forge.renameRepo(repo, body.name);
    }
    if (body.description !== undefined) repo.description = typeof body.description === 'string' ? body.description : null;
    if (body.homepage !== undefined) repo.homepage = typeof body.homepage === 'string' ? body.homepage : null;
    if (body.private !== undefined) repo.private = body.private === true;
    if (body.visibility === 'private' || body.visibility === 'public') repo.private = body.visibility === 'private';
    if (body.has_issues !== undefined) repo.hasIssues = body.has_issues !== false;
    if (body.has_wiki !== undefined) repo.hasWiki = body.has_wiki !== false;
    if (body.has_projects !== undefined) repo.hasProjects = body.has_projects !== false;
    if (body.archived !== undefined) repo.archived = body.archived === true;
    if (body.default_branch !== undefined) {
      if (typeof body.default_branch !== 'string' || (repo.refs.size > 0 && forge.branchTip(repo, body.default_branch) === undefined)) {
        throw new ForgeError(422, 'Validation Failed', [{ resource: 'Repository', code: 'invalid', field: 'default_branch' }]);
      }
      repo.defaultBranch = body.default_branch;
      repo.version += 1;
    }
    forge.touchRepo(repo);
    return json(200, this.views.repo(repo));
  }

  // ---------------------------------------------------------------- git data

  private gitRoute(request: Request, repo: Repo, s: string[], params: URLSearchParams): Response {
    const method = request.method;
    const forge = this.forge;
    const views = this.views;
    const kind = s[0];
    const rest = s.slice(1).join('/');
    if (kind === 'refs' || kind === 'ref' || kind === 'matching-refs') {
      if (kind === 'refs' && rest === '' && method === 'POST') {
        const body = parseBody(request);
        const ref = typeof body.ref === 'string' ? body.ref : '';
        const sha = typeof body.sha === 'string' ? body.sha : '';
        if (!ref.startsWith('refs/') || ref.split('/').length < 3) throw new ForgeError(422, 'Reference name must start with \'refs/\' and have at least two slashes.');
        if (!/^[0-9a-f]{40}$/.test(sha) || !forge.store.has(sha)) throw new ForgeError(422, 'Object does not exist');
        if (repo.refs.has(ref)) throw new ForgeError(422, 'Reference already exists');
        forge.setRef(repo, ref, sha);
        return json(201, views.ref(repo, ref, sha), `Location: ${views.api}/repos/${repo.owner.login}/${repo.name}/git/${ref}\r\n`);
      }
      const full = rest === '' ? 'refs' : `refs/${rest}`;
      if (method === 'GET' || method === 'HEAD') {
        if (kind === 'refs' || kind === 'ref') {
          const exact = repo.refs.get(full);
          if (exact !== undefined) return json(200, views.ref(repo, full, exact));
          if (kind === 'ref') return notFound();
        }
        const prefix = rest === '' ? 'refs/' : full;
        const matches = [...repo.refs.entries()].filter(([name]) => name === prefix || name.startsWith(prefix.endsWith('/') ? prefix : `${prefix}/`) || (kind === 'matching-refs' && name.startsWith(prefix))).sort((a, b) => (a[0] < b[0] ? -1 : 1));
        if (matches.length === 0) return kind === 'matching-refs' ? json(200, []) : notFound();
        return json(200, matches.map(([name, oid]) => views.ref(repo, name, oid)));
      }
      if (method === 'PATCH' && kind === 'refs') {
        const current = repo.refs.get(full);
        if (current === undefined) throw new ForgeError(422, 'Reference does not exist');
        const body = parseBody(request);
        const sha = typeof body.sha === 'string' ? body.sha : '';
        if (!/^[0-9a-f]{40}$/.test(sha) || !forge.store.has(sha)) throw new ForgeError(422, 'Object does not exist');
        if (body.force !== true && full.startsWith('refs/heads/') && !isAncestor(forge.store, current, sha)) throw new ForgeError(422, 'Update is not a fast forward');
        forge.setRef(repo, full, sha);
        return json(200, views.ref(repo, full, sha));
      }
      if (method === 'DELETE' && kind === 'refs') {
        if (!repo.refs.has(full)) throw new ForgeError(422, 'Reference does not exist');
        forge.setRef(repo, full, null);
        return { status: 204, type: JSON_TYPE, body: '' };
      }
      return notFound();
    }
    if (kind === 'commits' && s.length === 2 && method === 'GET') {
      const commit = views.gitCommit(repo, s[1]!);
      return commit === null ? notFound() : json(200, commit);
    }
    if (kind === 'trees' && s.length >= 2 && method === 'GET') {
      let oid = s.slice(1).join('/');
      const resolved = forge.resolveCommitish(repo, oid);
      if (resolved !== undefined && forge.store.commit(resolved) !== null) oid = forge.store.commit(resolved)!.tree;
      const entries = forge.store.tree(oid);
      if (entries === null) return notFound();
      const repoUrl = `${views.api}/repos/${repo.owner.login}/${repo.name}`;
      const recursive = params.has('recursive') && params.get('recursive') !== '0' && params.get('recursive') !== 'false';
      const out: Array<Record<string, unknown>> = [];
      const walk = (treeOid: string, prefix: string): void => {
        for (const entry of forge.store.tree(treeOid) ?? []) {
          const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
          const type = entry.mode === '40000' ? 'tree' : entry.mode === '160000' ? 'commit' : 'blob';
          const item: Record<string, unknown> = { path, mode: entry.mode === '40000' ? '040000' : entry.mode, type, sha: entry.oid, url: `${repoUrl}/git/${type === 'tree' ? 'trees' : 'blobs'}/${entry.oid}` };
          if (type === 'blob') item.size = forge.store.get(entry.oid)?.data.length ?? 0;
          out.push(item);
          if (recursive && type === 'tree') walk(entry.oid, path);
        }
      };
      walk(oid, '');
      return json(200, { sha: oid, url: `${repoUrl}/git/trees/${oid}`, tree: out, truncated: false });
    }
    if (kind === 'blobs' && s.length === 2 && method === 'GET') {
      const obj = forge.store.get(s[1]!);
      if (obj === undefined || obj.type !== OBJ_BLOB) return notFound();
      return json(200, { sha: s[1], node_id: `B_${s[1]}`, size: obj.data.length, url: `${views.api}/repos/${repo.owner.login}/${repo.name}/git/blobs/${s[1]}`, content: obj.data.toString('base64'), encoding: 'base64' });
    }
    if (kind === 'blobs' && s.length === 1 && method === 'POST') {
      const body = parseBody(request);
      if (typeof body.content !== 'string') throw new ForgeError(422, 'Validation Failed', [{ resource: 'Blob', code: 'missing_field', field: 'content' }]);
      const data = body.encoding === 'base64' ? Buffer.from(body.content, 'base64') : Buffer.from(body.content, 'utf8');
      const oid = forge.store.put(OBJ_BLOB, data);
      return json(201, { sha: oid, url: `${views.api}/repos/${repo.owner.login}/${repo.name}/git/blobs/${oid}` });
    }
    if (kind === 'tags' && s.length === 2 && method === 'GET') {
      const tag = forge.store.tag(s[1]!);
      if (tag === null || forge.store.get(s[1]!)?.type !== OBJ_TAG) return notFound();
      return json(200, { sha: s[1], tag: tag.tag, message: tag.message, object: { sha: tag.object, type: tag.type } });
    }
    return notFound();
  }

  // ---------------------------------------------------------------- branches and commits

  private branchesRoute(request: Request, repo: Repo, s: string[], params: URLSearchParams): Response {
    if (request.method !== 'GET' && request.method !== 'HEAD') return notFound();
    const views = this.views;
    if (s.length === 0 || (s.length === 1 && s[0] === '')) {
      const branches = [...repo.refs.entries()].filter(([name]) => name.startsWith('refs/heads/')).map(([name, oid]) => [name.slice(11), oid] as [string, string]);
      branches.sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0));
      const { page, extra } = this.paginate(branches, request, params);
      return json(200, page.map(([name, oid]) => views.branchShort(repo, name, oid)), extra);
    }
    const name = s.join('/');
    const oid = this.forge.branchTip(repo, name);
    if (oid === undefined) return errorJson(404, 'Branch not found');
    return json(200, views.branchFull(repo, name, oid));
  }

  private commitsRoute(request: Request, repo: Repo, s: string[], params: URLSearchParams): Response {
    if (request.method !== 'GET' && request.method !== 'HEAD') return notFound();
    const forge = this.forge;
    const views = this.views;
    if (s.length === 0 || (s.length === 1 && s[0] === '')) {
      if (repo.refs.size === 0) return errorJson(409, 'Git Repository is empty.');
      const start = forge.resolveCommitish(repo, params.get('sha') ?? '');
      if (start === undefined) return errorJson(404, 'No commit found for SHA: ' + (params.get('sha') ?? ''));
      const perPage = intParam(params, 'per_page', 30, 1, 100);
      const page = intParam(params, 'page', 1, 1, 100000);
      const path = params.get('path');
      let oids = logFrom(forge.store, start, path === null ? page * perPage + 1 : 100000);
      if (path !== null && path !== '') {
        oids = oids.filter((oid) => {
          const commit = forge.store.commit(oid)!;
          const mine = lookupPath(forge.store, commit.tree, path)?.oid ?? null;
          if (commit.parents.length === 0) return mine !== null;
          return commit.parents.some((parent) => (lookupPath(forge.store, forge.store.commit(parent)?.tree ?? '', path)?.oid ?? null) !== mine);
        });
      }
      const all = oids;
      const { page: slice, extra } = this.paginate(all, request, params);
      return json(200, slice.map((oid) => views.commit(repo, oid)), extra);
    }
    const ref = s.join('/');
    const oid = forge.resolveCommitish(repo, ref);
    if (oid === undefined) return errorJson(422, `No commit found for SHA: ${ref}`);
    const body = views.commit(repo, oid)!;
    const commit = forge.store.commit(oid)!;
    const before = forge.filesAt(commit.parents[0]);
    const after = forge.filesAt(oid);
    const files: Array<Record<string, unknown>> = [];
    for (const path of new Set([...before.keys(), ...after.keys()])) {
      const a = before.get(path);
      const b = after.get(path);
      if (a?.oid === b?.oid) continue;
      files.push({ sha: b?.oid ?? a!.oid, filename: path, status: a === undefined ? 'added' : b === undefined ? 'removed' : 'modified', additions: 0, deletions: 0, changes: 0 });
    }
    body.files = files;
    body.stats = { total: 0, additions: 0, deletions: 0 };
    return json(200, body);
  }

  // ---------------------------------------------------------------- contents

  private contentsRoute(request: Request, repo: Repo, rawPath: string, params: URLSearchParams): Response {
    const forge = this.forge;
    const path = rawPath.split('/').filter((part) => part !== '').join('/');
    const method = request.method;
    if (method === 'GET' || method === 'HEAD') {
      if (repo.refs.size === 0) return errorJson(404, 'This repository is empty.');
      const refName = params.get('ref') ?? repo.defaultBranch;
      const tip = forge.resolveCommitish(repo, refName);
      if (tip === undefined) return errorJson(404, `No commit found for the ref ${refName}`);
      const commit = forge.store.commit(tip)!;
      const entry = lookupPath(forge.store, commit.tree, path);
      if (entry === null) return notFound();
      if (entry.mode === '40000') {
        const entries = forge.store.tree(entry.oid) ?? [];
        return json(200, entries.map((child) => this.views.contentEntry(repo, path === '' ? child.name : `${path}/${child.name}`, child.mode, child.oid, refName)));
      }
      const blob = forge.store.get(entry.oid);
      const base = this.views.contentEntry(repo, path, entry.mode, entry.oid, refName);
      const data = blob?.data ?? Buffer.alloc(0);
      const encoded = data.toString('base64').replace(/.{60}/g, '$&\n');
      return json(200, { ...base, content: encoded.endsWith('\n') || encoded === '' ? encoded : `${encoded}\n`, encoding: 'base64' });
    }
    if (method === 'PUT' || method === 'DELETE') {
      if (path === '') throw new ForgeError(422, 'Invalid request.\n\n"path" wasn\'t supplied.');
      const body = parseBody(request);
      if (typeof body.message !== 'string' || body.message === '') throw new ForgeError(422, 'Invalid request.\n\n"message" wasn\'t supplied.');
      const branch = typeof body.branch === 'string' && body.branch !== '' ? body.branch.replace(/^refs\/heads\//, '') : repo.defaultBranch;
      let tip = forge.branchTip(repo, branch);
      let startFrom = tip;
      if (tip === undefined && repo.refs.size > 0) {
        // A new branch starts from the default branch (lenient: GitHub wants the branch to exist).
        startFrom = forge.branchTip(repo, repo.defaultBranch);
        if (startFrom === undefined) return errorJson(404, `Branch ${branch} not found`);
      }
      const files = forge.filesAt(startFrom);
      const existing = files.get(path);
      const author = this.signatureFrom(body.author) ?? this.signatureFrom(body.committer);
      if (method === 'DELETE') {
        if (existing === undefined) return notFound();
        if (typeof body.sha !== 'string') throw new ForgeError(422, 'Invalid request.\n\n"sha" wasn\'t supplied.');
        if (body.sha !== existing.oid) return errorJson(409, `${path} does not match ${body.sha}`);
        files.delete(path);
        const oid = forge.commitTree(repo, branch, files, body.message, startFrom === undefined ? [] : [startFrom], author);
        tip = oid;
        return json(200, { content: null, commit: this.contentsCommit(repo, oid) });
      }
      if (typeof body.content !== 'string') throw new ForgeError(422, 'Invalid request.\n\n"content" wasn\'t supplied.');
      if (existing !== undefined) {
        if (typeof body.sha !== 'string' || body.sha === '') throw new ForgeError(422, 'Invalid request.\n\n"sha" wasn\'t supplied.');
        if (body.sha !== existing.oid) return errorJson(409, `${path} does not match ${body.sha}`);
      } else if (lookupPath(forge.store, forge.store.commit(startFrom ?? '')?.tree ?? '', path)?.mode === '40000') {
        throw new ForgeError(422, 'Invalid request.\n\nA directory exists at this path.');
      }
      const blob = forge.store.put(OBJ_BLOB, Buffer.from(body.content, 'base64'));
      // A file cannot sit where a directory prefix is a file: drop such clashing entries.
      const parts = path.split('/');
      for (let i = 1; i < parts.length; i += 1) files.delete(parts.slice(0, i).join('/'));
      for (const key of [...files.keys()]) if (key.startsWith(`${path}/`)) files.delete(key);
      files.set(path, { mode: existing?.mode ?? '100644', oid: blob });
      const oid = forge.commitTree(repo, branch, files, body.message, startFrom === undefined ? [] : [startFrom], author);
      const entry = this.views.contentEntry(repo, path, '100644', blob, branch);
      return json(existing === undefined ? 201 : 200, { content: { ...entry, encoding: 'base64' }, commit: this.contentsCommit(repo, oid) });
    }
    return notFound();
  }

  private signatureFrom(value: unknown): { name: string; email: string; time: number; tz: string } | undefined {
    if (value === null || typeof value !== 'object') return undefined;
    const record = value as Record<string, unknown>;
    if (typeof record.name !== 'string' || typeof record.email !== 'string') return undefined;
    const sig = this.forge.signature();
    return { name: record.name, email: record.email, time: sig.time, tz: '+0000' };
  }

  private contentsCommit(repo: Repo, oid: string): Record<string, unknown> {
    const full = this.views.gitCommit(repo, oid)!;
    return full;
  }

  // ---------------------------------------------------------------- labels

  private labelsRoute(request: Request, repo: Repo, s: string[], params: URLSearchParams): Response {
    const method = request.method;
    const views = this.views;
    if (s.length === 0 || (s.length === 1 && s[0] === '')) {
      if (method === 'GET') {
        const labels = [...repo.labels.values()];
        const { page, extra } = this.paginate(labels, request, params);
        return json(200, page.map((label) => views.label(repo, label)), extra);
      }
      if (method === 'POST') {
        const body = parseBody(request);
        const label = this.forge.createLabel(repo, body.name as string, typeof body.color === 'string' ? body.color : undefined, typeof body.description === 'string' ? body.description : null);
        return json(201, views.label(repo, label));
      }
      return notFound();
    }
    const name = s.join('/');
    const label = repo.labels.get(name.toLowerCase());
    if (label === undefined) return notFound();
    if (method === 'GET') return json(200, views.label(repo, label));
    if (method === 'PATCH') {
      const body = parseBody(request);
      if (typeof body.new_name === 'string' && body.new_name !== '') {
        repo.labels.delete(label.name.toLowerCase());
        label.name = body.new_name;
        repo.labels.set(label.name.toLowerCase(), label);
      }
      if (typeof body.color === 'string') label.color = body.color.replace(/^#/, '');
      if (body.description !== undefined) label.description = typeof body.description === 'string' ? body.description : null;
      this.forge.touchRepo(repo);
      return json(200, views.label(repo, label));
    }
    if (method === 'DELETE') {
      repo.labels.delete(label.name.toLowerCase());
      for (const issue of repo.issueList) issue.labels = issue.labels.filter((other) => other !== label);
      this.forge.touchRepo(repo);
      return { status: 204, type: JSON_TYPE, body: '' };
    }
    return notFound();
  }

  // ---------------------------------------------------------------- issues

  private sortIssues(list: Issue[], params: URLSearchParams): Issue[] {
    const sort = params.get('sort') ?? 'created';
    const direction = params.get('direction') ?? 'desc';
    const key = (issue: Issue): number => (sort === 'updated' ? issue.updatedAt : sort === 'comments' ? issue.comments.length : issue.createdAt);
    const sorted = [...list].sort((a, b) => key(a) - key(b) || a.number - b.number);
    if (direction !== 'asc') sorted.reverse();
    return sorted;
  }

  private filterState(list: Issue[], params: URLSearchParams): Issue[] {
    const state = (params.get('state') ?? 'open').toLowerCase();
    if (state === 'all') return list;
    if (state === 'closed') return list.filter((issue) => issue.state === 'closed');
    return list.filter((issue) => issue.state === 'open');
  }

  private issuesRoute(request: Request, repo: Repo, s: string[], params: URLSearchParams): Response {
    const method = request.method;
    const forge = this.forge;
    const views = this.views;
    if (s.length === 0 || (s.length === 1 && s[0] === '')) {
      if (method === 'GET' || method === 'HEAD') {
        let list = this.filterState(repo.issueList, params);
        const labels = params.get('labels');
        if (labels !== null && labels !== '') {
          const wanted = labels.split(',').map((name) => name.trim().toLowerCase());
          list = list.filter((issue) => wanted.every((name) => issue.labels.some((label) => label.name.toLowerCase() === name)));
        }
        const creator = params.get('creator');
        if (creator !== null && creator !== '') list = list.filter((issue) => issue.user.login.toLowerCase() === creator.toLowerCase());
        const since = params.get('since');
        if (since !== null && since !== '') {
          const t = Date.parse(since);
          if (Number.isFinite(t)) list = list.filter((issue) => issue.updatedAt >= t);
        }
        const sorted = this.sortIssues(list, params);
        const { page, extra } = this.paginate(sorted, request, params);
        return json(200, joinJson(page.map((issue) => views.issue(issue))), extra);
      }
      if (method === 'POST') {
        if (!repo.hasIssues) return errorJson(410, 'Issues are disabled for this repo');
        const body = parseBody(request);
        const issue = forge.createIssue(repo, { title: body.title, body: body.body, labels: body.labels });
        return json(201, views.issue(issue), `Location: ${views.api}/repos/${repo.owner.login}/${repo.name}/issues/${issue.number}\r\n`);
      }
      return notFound();
    }
    if (s[0] === 'comments') {
      if (s.length === 1 && method === 'GET') {
        const comments = repo.issueList.flatMap((issue) => issue.comments);
        const { page, extra } = this.paginate(comments, request, params);
        return json(200, page.map((comment) => views.comment(comment)), extra);
      }
      if (s.length === 2) {
        const comment = forge.commentsById.get(Number.parseInt(s[1]!, 10));
        if (comment === undefined || comment.issue.repo !== repo) return notFound();
        if (method === 'GET') return json(200, views.comment(comment));
        if (method === 'PATCH') {
          const body = parseBody(request);
          if (typeof body.body !== 'string' || body.body === '') throw new ForgeError(422, 'Validation Failed', [{ resource: 'IssueComment', code: 'missing_field', field: 'body' }]);
          comment.body = body.body;
          comment.updatedAt = forge.now();
          forge.touchIssue(comment.issue);
          return json(200, views.comment(comment));
        }
        if (method === 'DELETE') {
          comment.issue.comments = comment.issue.comments.filter((other) => other !== comment);
          forge.commentsById.delete(comment.id);
          forge.touchIssue(comment.issue);
          return { status: 204, type: JSON_TYPE, body: '' };
        }
      }
      return notFound();
    }
    const number = Number(s[0]);
    if (!Number.isInteger(number)) return notFound();
    const issue = repo.issues.get(number);
    if (issue === undefined) return notFound();
    if (s.length === 1) {
      if (method === 'GET' || method === 'HEAD') return json(200, views.issue(issue));
      if (method === 'PATCH') {
        const body = parseBody(request);
        forge.updateIssue(issue, { title: body.title, body: body.body, state: body.state, stateReason: body.state_reason, labels: body.labels });
        return json(200, views.issue(issue));
      }
      return notFound();
    }
    const sub = s[1];
    if (sub === 'comments' && s.length === 2) {
      if (method === 'GET') {
        const { page, extra } = this.paginate(issue.comments, request, params);
        return json(200, page.map((comment) => views.comment(comment)), extra);
      }
      if (method === 'POST') {
        const body = parseBody(request);
        const comment = forge.addComment(issue, body.body);
        return json(201, views.comment(comment), `Location: ${views.api}/repos/${repo.owner.login}/${repo.name}/issues/comments/${comment.id}\r\n`);
      }
      return notFound();
    }
    if (sub === 'labels') {
      if (s.length === 2) {
        if (method === 'GET') return json(200, issue.labels.map((label) => views.label(repo, label)));
        if (method === 'POST' || method === 'PUT') {
          const body = parseBody(request);
          const names = Array.isArray(body.labels) ? body.labels : [];
          const labels = forge.resolveLabels(repo, names);
          issue.labels = method === 'PUT' ? labels : [...issue.labels, ...labels.filter((label) => !issue.labels.includes(label))];
          forge.touchIssue(issue);
          return json(200, issue.labels.map((label) => views.label(repo, label)));
        }
        if (method === 'DELETE') {
          issue.labels = [];
          forge.touchIssue(issue);
          return { status: 204, type: JSON_TYPE, body: '' };
        }
      }
      if (s.length === 3 && method === 'DELETE') {
        const name = s[2]!.toLowerCase();
        const before = issue.labels.length;
        issue.labels = issue.labels.filter((label) => label.name.toLowerCase() !== name);
        if (issue.labels.length === before) return errorJson(404, 'Label does not exist');
        forge.touchIssue(issue);
        return json(200, issue.labels.map((label) => views.label(repo, label)));
      }
      return notFound();
    }
    if (sub === 'events' || sub === 'timeline') return method === 'GET' ? json(200, []) : notFound();
    if (sub === 'lock') return { status: 204, type: JSON_TYPE, body: '' };
    return notFound();
  }

  // ---------------------------------------------------------------- pull requests

  private pullsRoute(request: Request, repo: Repo, s: string[], params: URLSearchParams): Response {
    const method = request.method;
    const forge = this.forge;
    const views = this.views;
    if (s.length === 0 || (s.length === 1 && s[0] === '')) {
      if (method === 'GET' || method === 'HEAD') {
        let list = this.filterState(repo.issueList.filter((issue) => issue.pull !== null), params);
        const headFilter = params.get('head');
        if (headFilter !== null && headFilter !== '') {
          const branch = headFilter.includes(':') ? headFilter.slice(headFilter.indexOf(':') + 1) : headFilter;
          list = list.filter((issue) => issue.pull!.head === branch);
        }
        const baseFilter = params.get('base');
        if (baseFilter !== null && baseFilter !== '') list = list.filter((issue) => issue.pull!.base === baseFilter);
        const sort = params.get('sort') ?? 'created';
        const sorted = this.sortIssues(list, new URLSearchParams({ sort: sort === 'popularity' || sort === 'long-running' ? 'created' : sort, direction: params.get('direction') ?? 'desc' }));
        const { page, extra } = this.paginate(sorted, request, params);
        return json(200, joinJson(page.map((issue) => views.pull(issue))), extra);
      }
      if (method === 'POST') {
        const body = parseBody(request);
        let title = body.title;
        let text = body.body;
        if (typeof body.issue === 'number') {
          const existing = repo.issues.get(body.issue);
          if (existing !== undefined) {
            title = existing.title;
            text = existing.body;
          }
        }
        const issue = forge.createPull(repo, { title, body: text, head: body.head, base: body.base, draft: body.draft });
        return json(201, views.pull(issue), `Location: ${views.api}/repos/${repo.owner.login}/${repo.name}/pulls/${issue.number}\r\n`);
      }
      return notFound();
    }
    const number = Number(s[0]);
    if (!Number.isInteger(number)) return notFound();
    const issue = repo.issues.get(number);
    if (issue === undefined || issue.pull === null) return notFound();
    if (s.length === 1) {
      if (method === 'GET' || method === 'HEAD') return json(200, views.pull(issue));
      if (method === 'PATCH') {
        const body = parseBody(request);
        forge.updateIssue(issue, { title: body.title, body: body.body, state: body.state });
        if (typeof body.base === 'string' && body.base !== '' && issue.state === 'open') {
          if (forge.branchTip(repo, body.base) === undefined) throw new ForgeError(422, 'Validation Failed', [{ resource: 'PullRequest', code: 'invalid', field: 'base' }]);
          issue.pull.base = body.base;
          forge.touchIssue(issue);
        }
        return json(200, views.pull(issue));
      }
      return notFound();
    }
    const sub = s[1];
    if (sub === 'files' && method === 'GET') {
      const repoUrl = `${views.api}/repos/${repo.owner.login}/${repo.name}`;
      const headSha = forge.pullHeadSha(issue);
      const files = forge.pullFiles(issue).map((file) => ({
        sha: file.sha,
        filename: file.filename,
        status: file.status,
        additions: file.additions,
        deletions: file.deletions,
        changes: file.changes,
        blob_url: `${views.origin}/${repo.owner.login}/${repo.name}/blob/${headSha}/${file.filename}`,
        raw_url: `${views.origin}/${repo.owner.login}/${repo.name}/raw/${headSha}/${file.filename}`,
        contents_url: `${repoUrl}/contents/${file.filename}?ref=${headSha}`,
      }));
      const { page, extra } = this.paginate(files, request, params);
      return json(200, page, extra);
    }
    if (sub === 'commits' && method === 'GET') {
      const commits = forge.pullCommits(issue).map((oid) => views.commit(repo, oid));
      const { page, extra } = this.paginate(commits, request, params);
      return json(200, page, extra);
    }
    if (sub === 'merge') {
      if (method === 'GET') return issue.pull.merged ? { status: 204, type: JSON_TYPE, body: '' } : notFound();
      if (method === 'PUT' || method === 'POST') {
        const body = parseBody(request);
        const result = forge.mergePull(issue, { method: body.merge_method, title: body.commit_title, message: body.commit_message, sha: body.sha });
        return json(200, { sha: result.sha, merged: true, message: 'Pull Request successfully merged' });
      }
      return notFound();
    }
    if ((sub === 'reviews' || sub === 'comments' || sub === 'requested_reviewers') && method === 'GET') {
      return json(200, sub === 'requested_reviewers' ? { users: [], teams: [] } : []);
    }
    return notFound();
  }
}

export { ZERO_OID };
