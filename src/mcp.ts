// MCP over streamable HTTP at /mcp (JSON answers, stateless). Speaks the 2026-07-28 revision through
// server/discover and the earlier revisions through the initialize handshake. The tools are thin wrappers
// over the same forge operations the REST surface uses, and answer with the REST JSON shapes.

import { ForgeError, type Forge, type Repo } from './forge.ts';
import type { Views } from './views.ts';

const SERVER_INFO = { name: 'forge-node-mcp', title: 'Forge MCP', version: '1.0.0' };
const MODERN = '2026-07-28';
const SUPPORTED = [MODERN, '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];
const INSTRUCTIONS = 'Tools for this git forge: repositories, issues and pull requests. Owners and repository names match the REST API.';

interface ToolSpec {
  name: string;
  title: string;
  description: string;
  inputSchema: Record<string, unknown>;
  annotations: Record<string, unknown>;
}

const str = (description: string): Record<string, unknown> => ({ type: 'string', description });
const num = (description: string): Record<string, unknown> => ({ type: 'number', description });
const bool = (description: string): Record<string, unknown> => ({ type: 'boolean', description });

function schema(properties: Record<string, unknown>, required: string[]): Record<string, unknown> {
  return { type: 'object', properties, required, additionalProperties: false };
}

const TOOLS: ToolSpec[] = [
  {
    name: 'get_me',
    title: 'Get my user',
    description: 'Get the authenticated user (the token owner): login, id and profile.',
    inputSchema: schema({}, []),
    annotations: { readOnlyHint: true },
  },
  {
    name: 'create_repo',
    title: 'Create repository',
    description: 'Create a new repository owned by the authenticated user (or by an organization when owner names one).',
    inputSchema: schema(
      {
        name: str('Repository name'),
        description: str('Repository description'),
        private: bool('Whether the repository is private'),
        autoInit: bool('Initialize with a README commit'),
        owner: str('Owner login (defaults to the authenticated user)'),
      },
      ['name'],
    ),
    annotations: { readOnlyHint: false },
  },
  {
    name: 'get_repo',
    title: 'Get repository',
    description: 'Get a repository by owner and name.',
    inputSchema: schema({ owner: str('Repository owner'), repo: str('Repository name') }, ['owner', 'repo']),
    annotations: { readOnlyHint: true },
  },
  {
    name: 'list_repos',
    title: 'List repositories',
    description: 'List the repositories of the authenticated user.',
    inputSchema: schema({ page: num('Page number (1-based)'), perPage: num('Results per page (max 100)') }, []),
    annotations: { readOnlyHint: true },
  },
  {
    name: 'create_issue',
    title: 'Create issue',
    description: 'Create a new issue in a repository.',
    inputSchema: schema(
      {
        owner: str('Repository owner'),
        repo: str('Repository name'),
        title: str('Issue title'),
        body: str('Issue body (Markdown)'),
        labels: { type: 'array', items: { type: 'string' }, description: 'Names of existing labels to attach' },
      },
      ['owner', 'repo', 'title'],
    ),
    annotations: { readOnlyHint: false },
  },
  {
    name: 'get_issue',
    title: 'Get issue',
    description: 'Get one issue of a repository by number.',
    inputSchema: schema({ owner: str('Repository owner'), repo: str('Repository name'), issue_number: num('Issue number') }, ['owner', 'repo', 'issue_number']),
    annotations: { readOnlyHint: true },
  },
  {
    name: 'list_issues',
    title: 'List issues',
    description: 'List the issues of a repository, filtered by state (open, closed or all).',
    inputSchema: schema(
      {
        owner: str('Repository owner'),
        repo: str('Repository name'),
        state: { type: 'string', enum: ['open', 'closed', 'all'], description: 'Issue state filter (default open)' },
        page: num('Page number (1-based)'),
        perPage: num('Results per page (max 100)'),
      },
      ['owner', 'repo'],
    ),
    annotations: { readOnlyHint: true },
  },
  {
    name: 'update_issue',
    title: 'Update issue',
    description: 'Update an issue: title, body or state (open/closed).',
    inputSchema: schema(
      {
        owner: str('Repository owner'),
        repo: str('Repository name'),
        issue_number: num('Issue number'),
        title: str('New title'),
        body: str('New body'),
        state: { type: 'string', enum: ['open', 'closed'], description: 'New state' },
      },
      ['owner', 'repo', 'issue_number'],
    ),
    annotations: { readOnlyHint: false },
  },
  {
    name: 'add_issue_comment',
    title: 'Comment on issue',
    description: 'Add a comment to an issue or pull request.',
    inputSchema: schema({ owner: str('Repository owner'), repo: str('Repository name'), issue_number: num('Issue or pull request number'), body: str('Comment text') }, ['owner', 'repo', 'issue_number', 'body']),
    annotations: { readOnlyHint: false },
  },
  {
    name: 'create_pull_request',
    title: 'Create pull request',
    description: 'Open a pull request from a head branch into a base branch.',
    inputSchema: schema(
      {
        owner: str('Repository owner'),
        repo: str('Repository name'),
        title: str('Pull request title'),
        body: str('Pull request description'),
        head: str('Branch with the changes'),
        base: str('Branch to merge into'),
        draft: bool('Open as a draft'),
      },
      ['owner', 'repo', 'title', 'head', 'base'],
    ),
    annotations: { readOnlyHint: false },
  },
  {
    name: 'get_pull_request',
    title: 'Get pull request',
    description: 'Get one pull request by number.',
    inputSchema: schema({ owner: str('Repository owner'), repo: str('Repository name'), pull_number: num('Pull request number') }, ['owner', 'repo', 'pull_number']),
    annotations: { readOnlyHint: true },
  },
  {
    name: 'list_pull_requests',
    title: 'List pull requests',
    description: 'List the pull requests of a repository, filtered by state (open, closed or all).',
    inputSchema: schema(
      { owner: str('Repository owner'), repo: str('Repository name'), state: { type: 'string', enum: ['open', 'closed', 'all'], description: 'State filter (default open)' } },
      ['owner', 'repo'],
    ),
    annotations: { readOnlyHint: true },
  },
  {
    name: 'merge_pull_request',
    title: 'Merge pull request',
    description: 'Merge a pull request (merge, squash or rebase).',
    inputSchema: schema(
      {
        owner: str('Repository owner'),
        repo: str('Repository name'),
        pull_number: num('Pull request number'),
        merge_method: { type: 'string', enum: ['merge', 'squash', 'rebase'], description: 'Merge method (default merge)' },
        commit_title: str('Title of the merge commit'),
        commit_message: str('Extra detail for the merge commit'),
      },
      ['owner', 'repo', 'pull_number'],
    ),
    annotations: { readOnlyHint: false },
  },
  {
    name: 'list_branches',
    title: 'List branches',
    description: 'List the branches of a repository with their tip commits.',
    inputSchema: schema({ owner: str('Repository owner'), repo: str('Repository name') }, ['owner', 'repo']),
    annotations: { readOnlyHint: true },
  },
];

const TOOLS_BY_NAME = new Map(TOOLS.map((tool) => [tool.name, tool]));
/** The tools/list result, serialized once: two calls answer byte-identical arrays. */
const TOOLS_JSON = JSON.stringify(TOOLS);

class ToolInputError extends Error {}

function stringArg(args: Record<string, unknown>, ...names: string[]): string {
  for (const name of names) {
    const value = args[name];
    if (typeof value === 'string' && value !== '') return value;
  }
  throw new ToolInputError(`missing required argument: ${names[0]}`);
}

function optionalString(args: Record<string, unknown>, ...names: string[]): string | undefined {
  for (const name of names) {
    const value = args[name];
    if (typeof value === 'string') return value;
  }
  return undefined;
}

function numberArg(args: Record<string, unknown>, ...names: string[]): number {
  for (const name of names) {
    const value = args[name];
    if (typeof value === 'number' && Number.isInteger(value)) return value;
    if (typeof value === 'string' && /^\d+$/.test(value)) return Number.parseInt(value, 10);
  }
  throw new ToolInputError(`missing required argument: ${names[0]}`);
}

export interface McpAnswer {
  status: number;
  body: string;
}

export class Mcp {
  private readonly forge: Forge;
  private readonly views: Views;

  constructor(forge: Forge, views: Views) {
    this.forge = forge;
    this.views = views;
  }

  /** Answer one POST body (already authenticated). */
  handle(bodyText: string): McpAnswer {
    let message: unknown;
    try {
      message = JSON.parse(bodyText);
    } catch {
      return { status: 400, body: JSON.stringify({ jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } }) };
    }
    if (Array.isArray(message)) {
      if (message.length === 0) return { status: 400, body: JSON.stringify({ jsonrpc: '2.0', id: null, error: { code: -32600, message: 'Invalid Request: empty batch' } }) };
      const answers = message.map((item) => this.one(item)).filter((answer): answer is Record<string, unknown> => answer !== null);
      if (answers.length === 0) return { status: 202, body: '' };
      return { status: 200, body: JSON.stringify(answers) };
    }
    const answer = this.one(message);
    if (answer === null) return { status: 202, body: '' };
    return { status: 200, body: JSON.stringify(answer) };
  }

  private one(message: unknown): Record<string, unknown> | null {
    if (message === null || typeof message !== 'object') return { jsonrpc: '2.0', id: null, error: { code: -32600, message: 'Invalid Request' } };
    const request = message as Record<string, unknown>;
    const id = request.id;
    const hasId = id !== undefined;
    if (typeof request.method !== 'string') {
      if (!hasId) return null;
      if ('result' in request || 'error' in request) return null;
      return { jsonrpc: '2.0', id: id ?? null, error: { code: -32600, message: 'Invalid Request' } };
    }
    if (!hasId) return null;
    const params = request.params !== null && typeof request.params === 'object' && !Array.isArray(request.params) ? (request.params as Record<string, unknown>) : {};
    try {
      const result = this.dispatch(request.method, params);
      if ('__error' in result) return { jsonrpc: '2.0', id, error: result.__error };
      return { jsonrpc: '2.0', id, result };
    } catch (err) {
      return { jsonrpc: '2.0', id, error: { code: -32603, message: err instanceof Error ? err.message : 'Internal error' } };
    }
  }

  private dispatch(method: string, params: Record<string, unknown>): Record<string, any> {
    switch (method) {
      case 'server/discover':
        return {
          supportedVersions: SUPPORTED,
          capabilities: { tools: { listChanged: false } },
          serverInfo: SERVER_INFO,
          instructions: INSTRUCTIONS,
          _meta: { 'io.modelcontextprotocol/serverInfo': SERVER_INFO },
        };
      case 'initialize': {
        const requested = typeof params.protocolVersion === 'string' ? params.protocolVersion : SUPPORTED[1]!;
        const chosen = SUPPORTED.includes(requested) && requested !== MODERN ? requested : SUPPORTED[1]!;
        return { protocolVersion: chosen, capabilities: { tools: { listChanged: false } }, serverInfo: SERVER_INFO, instructions: INSTRUCTIONS };
      }
      case 'ping':
        return {};
      case 'tools/list':
        return { tools: JSON.parse(TOOLS_JSON) as unknown[] };
      case 'tools/call':
        return this.call(params);
      case 'resources/list':
        return { resources: [] };
      case 'resources/templates/list':
        return { resourceTemplates: [] };
      case 'prompts/list':
        return { prompts: [] };
      case 'logging/setLevel':
        return {};
      default:
        return { __error: { code: -32601, message: `Method not found: ${method}` } };
    }
  }

  private call(params: Record<string, unknown>): Record<string, any> {
    const name = params.name;
    if (typeof name !== 'string' || !TOOLS_BY_NAME.has(name)) {
      return { __error: { code: -32602, message: `Unknown tool: ${String(name)}` } };
    }
    const args = params.arguments !== null && typeof params.arguments === 'object' && !Array.isArray(params.arguments) ? (params.arguments as Record<string, unknown>) : {};
    try {
      const value = this.run(name, args);
      const text = JSON.stringify(value);
      const structured = Array.isArray(value) ? { items: value } : (value as Record<string, unknown>);
      return { content: [{ type: 'text', text }], structuredContent: structured, isError: false };
    } catch (err) {
      if (err instanceof ToolInputError) return { content: [{ type: 'text', text: err.message }], isError: true };
      if (err instanceof ForgeError) {
        const detail = Array.isArray(err.errors) && err.errors.length > 0 ? `: ${JSON.stringify(err.errors)}` : '';
        return { content: [{ type: 'text', text: `${err.status} ${err.message}${detail}` }], isError: true };
      }
      throw err;
    }
  }

  private repoArg(args: Record<string, unknown>): Repo {
    const owner = stringArg(args, 'owner');
    const name = stringArg(args, 'repo', 'name');
    const repo = this.forge.visibleRepo(owner, name);
    if (repo === undefined) throw new ForgeError(404, `Not Found: repository ${owner}/${name}`);
    return repo;
  }

  private issueArg(repo: Repo, args: Record<string, unknown>, ...names: string[]) {
    const number = numberArg(args, ...names);
    const issue = repo.issues.get(number);
    if (issue === undefined) throw new ForgeError(404, `Not Found: #${number}`);
    return issue;
  }

  private run(name: string, args: Record<string, unknown>): unknown {
    const forge = this.forge;
    const views = this.views;
    switch (name) {
      case 'get_me':
        return views.userFull(forge.viewer);
      case 'create_repo': {
        const repoName = stringArg(args, 'name');
        const ownerLogin = optionalString(args, 'owner', 'organization');
        let owner = forge.viewer;
        if (ownerLogin !== undefined && ownerLogin !== '' && ownerLogin.toLowerCase() !== forge.viewer.login.toLowerCase()) {
          const other = forge.user(ownerLogin);
          if (other === undefined || other.type !== 'Organization') throw new ForgeError(404, `Not Found: owner ${ownerLogin}`);
          owner = other;
        }
        const repo = forge.createRepo(owner, {
          name: repoName,
          description: optionalString(args, 'description') ?? null,
          private: args.private === true,
          autoInit: args.autoInit === true || args.auto_init === true,
        });
        return JSON.parse(views.repo(repo));
      }
      case 'get_repo':
        return JSON.parse(views.repo(this.repoArg(args)));
      case 'list_repos': {
        const perPage = Math.min(100, Math.max(1, typeof args.perPage === 'number' ? args.perPage : 30));
        const page = Math.max(1, typeof args.page === 'number' ? args.page : 1);
        return forge.viewerRepos().slice((page - 1) * perPage, page * perPage).map((repo) => JSON.parse(views.repo(repo)));
      }
      case 'create_issue': {
        const repo = this.repoArg(args);
        const title = stringArg(args, 'title');
        const issue = forge.createIssue(repo, { title, body: optionalString(args, 'body'), labels: args.labels });
        return JSON.parse(views.issue(issue));
      }
      case 'get_issue': {
        const repo = this.repoArg(args);
        return JSON.parse(views.issue(this.issueArg(repo, args, 'issue_number', 'number')));
      }
      case 'list_issues': {
        const repo = this.repoArg(args);
        const state = (optionalString(args, 'state') ?? 'open').toLowerCase();
        const perPage = Math.min(100, Math.max(1, typeof args.perPage === 'number' ? args.perPage : 30));
        const page = Math.max(1, typeof args.page === 'number' ? args.page : 1);
        const list = repo.issueList.filter((issue) => state === 'all' || issue.state === state).reverse();
        return list.slice((page - 1) * perPage, page * perPage).map((issue) => JSON.parse(views.issue(issue)));
      }
      case 'update_issue': {
        const repo = this.repoArg(args);
        const issue = this.issueArg(repo, args, 'issue_number', 'number');
        forge.updateIssue(issue, { title: args.title, body: args.body, state: args.state });
        return JSON.parse(views.issue(issue));
      }
      case 'add_issue_comment': {
        const repo = this.repoArg(args);
        const issue = this.issueArg(repo, args, 'issue_number', 'number');
        const body = stringArg(args, 'body');
        return views.comment(forge.addComment(issue, body));
      }
      case 'create_pull_request': {
        const repo = this.repoArg(args);
        const title = stringArg(args, 'title');
        const head = stringArg(args, 'head');
        const base = stringArg(args, 'base');
        const issue = forge.createPull(repo, { title, body: optionalString(args, 'body'), head, base, draft: args.draft });
        return JSON.parse(views.pull(issue));
      }
      case 'get_pull_request': {
        const repo = this.repoArg(args);
        const issue = this.issueArg(repo, args, 'pull_number', 'number');
        if (issue.pull === null) throw new ForgeError(404, 'Not Found: not a pull request');
        return JSON.parse(views.pull(issue));
      }
      case 'list_pull_requests': {
        const repo = this.repoArg(args);
        const state = (optionalString(args, 'state') ?? 'open').toLowerCase();
        return repo.issueList
          .filter((issue) => issue.pull !== null && (state === 'all' || issue.state === state))
          .reverse()
          .map((issue) => JSON.parse(views.pull(issue)));
      }
      case 'merge_pull_request': {
        const repo = this.repoArg(args);
        const issue = this.issueArg(repo, args, 'pull_number', 'number');
        if (issue.pull === null) throw new ForgeError(404, 'Not Found: not a pull request');
        const result = forge.mergePull(issue, { method: optionalString(args, 'merge_method', 'mergeMethod'), title: args.commit_title, message: args.commit_message });
        return { sha: result.sha, merged: true, message: 'Pull Request successfully merged', number: issue.number, html_url: `${views.origin}/${repo.owner.login}/${repo.name}/pull/${issue.number}` };
      }
      case 'list_branches': {
        const repo = this.repoArg(args);
        return [...repo.refs.entries()].filter(([ref]) => ref.startsWith('refs/heads/')).map(([ref, oid]) => views.branchShort(repo, ref.slice(11), oid));
      }
      default:
        throw new ToolInputError(`unknown tool ${name}`);
    }
  }
}
