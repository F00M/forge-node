// GraphQL v4 at /api/graphql: a schema-driven executor over the forge (queries, mutations, Relay node
// ids, connections with cursors, introspection through __type/__schema, batching). Unknown fields fail
// validation with GitHub's "Field 'x' doesn't exist on type 'Y'" error before anything executes.

import { ForgeError, decodeNodeId, iso, type Comment, type Forge, type Issue, type Label, type Repo, type User } from './forge.ts';
import { parseDocument, GraphQLSyntaxError, type Directive, type Document, type Field, type Operation, type Selection, type Value } from './gql-parse.ts';

// ---------------------------------------------------------------- schema model

interface Ctx {
  forge: Forge;
  origin: string;
}

type Resolver = (parent: any, args: Record<string, any>, ctx: Ctx) => unknown;

interface FieldDef {
  type: string;
  args?: Record<string, string>;
  resolve?: Resolver;
  description?: string;
}

interface TypeDef {
  kind: 'OBJECT' | 'INTERFACE' | 'UNION' | 'ENUM' | 'INPUT_OBJECT' | 'SCALAR';
  name: string;
  description?: string;
  fields?: Record<string, FieldDef>;
  interfaces?: string[];
  possibleTypes?: string[];
  values?: string[];
  inputFields?: Record<string, string>;
}

const TYPED = Symbol('typed');

interface Typed {
  [TYPED]: string;
  value: unknown;
}

function typed(type: string, value: unknown): Typed {
  return { [TYPED]: type, value };
}

class ResolveError extends Error {
  readonly type: string;
  constructor(type: string, message: string) {
    super(message);
    this.type = type;
  }
}

function b64(text: string): string {
  return Buffer.from(text, 'utf8').toString('base64');
}

function cursorFor(offset: number): string {
  return b64(`cursor:v2:${offset}`);
}

function offsetOf(cursor: unknown): number | null {
  if (typeof cursor !== 'string' || cursor === '') return null;
  const text = Buffer.from(cursor, 'base64').toString('utf8');
  const match = /^cursor:v2:(\d+)$/.exec(text);
  return match === null ? null : Number.parseInt(match[1]!, 10);
}

/** A Relay connection over an ordered list, honouring first/last/after/before. */
function connection<T>(items: T[], args: Record<string, any>): Record<string, unknown> {
  let start = 0;
  let end = items.length;
  const after = offsetOf(args.after);
  if (after !== null) start = Math.min(items.length, Math.max(0, after));
  const before = offsetOf(args.before);
  if (before !== null) end = Math.max(start, Math.min(end, before - 1));
  if (typeof args.first === 'number') {
    if (args.first < 0) throw new ResolveError('INVALID_ARGUMENT', '`first` must be a non-negative integer');
    end = Math.min(end, start + Math.min(args.first, 100));
  } else if (typeof args.last === 'number') {
    if (args.last < 0) throw new ResolveError('INVALID_ARGUMENT', '`last` must be a non-negative integer');
    start = Math.max(start, end - Math.min(args.last, 100));
  } else {
    end = Math.min(end, start + 100);
  }
  const slice = items.slice(start, end);
  const edges = slice.map((node, index) => ({ cursor: cursorFor(start + index + 1), node }));
  return {
    nodes: slice,
    edges,
    totalCount: items.length,
    pageInfo: {
      hasNextPage: end < items.length,
      hasPreviousPage: start > 0,
      startCursor: edges.length > 0 ? edges[0]!.cursor : null,
      endCursor: edges.length > 0 ? edges[edges.length - 1]!.cursor : null,
    },
  };
}

function enumValue(value: unknown): string | null {
  return typeof value === 'string' ? value : null;
}

// ---------------------------------------------------------------- domain helpers

function repoUrl(ctx: Ctx, repo: Repo): string {
  return `${ctx.origin}/${repo.owner.login}/${repo.name}`;
}

function ownerTyped(user: User): Typed {
  return typed(user.type === 'Organization' ? 'Organization' : 'User', user);
}

function issueState(issue: Issue): string {
  return issue.state === 'open' ? 'OPEN' : 'CLOSED';
}

function pullState(issue: Issue): string {
  if (issue.pull!.merged) return 'MERGED';
  return issue.state === 'open' ? 'OPEN' : 'CLOSED';
}

function sortedIssues(list: Issue[], orderBy: any): Issue[] {
  const field = orderBy?.field ?? 'CREATED_AT';
  const direction = orderBy?.direction ?? 'ASC';
  const key = (issue: Issue): number => (field === 'UPDATED_AT' ? issue.updatedAt : field === 'COMMENTS' ? issue.comments.length : issue.createdAt);
  const sorted = [...list].sort((a, b) => key(a) - key(b) || a.number - b.number);
  if (direction === 'DESC') sorted.reverse();
  return sorted;
}

function lookupNode(ctx: Ctx, id: unknown): Typed | null {
  if (typeof id !== 'string') return null;
  const decoded = decodeNodeId(id);
  if (decoded === null) return null;
  const forge = ctx.forge;
  switch (decoded.kind) {
    case 'Repository': {
      const repo = forge.reposById.get(decoded.id);
      return repo === undefined ? null : typed('Repository', repo);
    }
    case 'Issue': {
      const issue = forge.issuesById.get(decoded.id);
      if (issue === undefined || issue.repo.deleted) return null;
      return typed(issue.pull === null ? 'Issue' : 'Issue', issue);
    }
    case 'PullRequest': {
      const issue = forge.pullsById.get(decoded.id);
      return issue === undefined || issue.repo.deleted ? null : typed('PullRequest', issue);
    }
    case 'IssueComment': {
      const comment = forge.commentsById.get(decoded.id);
      return comment === undefined ? null : typed('IssueComment', comment);
    }
    case 'User':
    case 'Organization': {
      for (const user of forge.users.values()) if (user.id === decoded.id && user.type === decoded.kind) return typed(decoded.kind, user);
      return null;
    }
    case 'Label': {
      const label = forge.labelsById.get(decoded.id);
      return label === undefined ? null : typed('Label', label);
    }
    default:
      return null;
  }
}

function requireNode(ctx: Ctx, id: unknown, kinds: string[]): Typed {
  const node = lookupNode(ctx, id);
  if (node === null || !kinds.includes(node[TYPED])) throw new ResolveError('NOT_FOUND', `Could not resolve to a node with the global id of '${String(id)}'.`);
  return node;
}

function wrapForge<T>(fn: () => T): T {
  try {
    return fn();
  } catch (err) {
    if (err instanceof ForgeError) {
      const detail = Array.isArray(err.errors) && err.errors.length > 0 ? ` ${JSON.stringify(err.errors)}` : '';
      throw new ResolveError(err.status === 404 ? 'NOT_FOUND' : 'UNPROCESSABLE', `${err.message}${detail}`);
    }
    throw err;
  }
}

// ---------------------------------------------------------------- the schema

const CONNECTION_ARGS = { first: 'Int', last: 'Int', after: 'String', before: 'String' };

function connectionTypes(name: string, node: string): TypeDef[] {
  return [
    {
      kind: 'OBJECT',
      name: `${name}Connection`,
      fields: {
        nodes: { type: `[${node}]` },
        edges: { type: `[${name}Edge]` },
        pageInfo: { type: 'PageInfo!' },
        totalCount: { type: 'Int!' },
      },
    },
    { kind: 'OBJECT', name: `${name}Edge`, fields: { cursor: { type: 'String!' }, node: { type: node } } },
  ];
}

function buildSchema(): Map<string, TypeDef> {
  const types: TypeDef[] = [];
  const add = (...defs: TypeDef[]): void => {
    types.push(...defs);
  };

  for (const scalar of ['String', 'Int', 'Float', 'Boolean', 'ID', 'DateTime', 'URI', 'GitObjectID', 'HTML', 'GitTimestamp']) add({ kind: 'SCALAR', name: scalar });

  add(
    { kind: 'ENUM', name: 'IssueState', values: ['OPEN', 'CLOSED'] },
    { kind: 'ENUM', name: 'IssueStateReason', values: ['COMPLETED', 'NOT_PLANNED', 'REOPENED', 'DUPLICATE'] },
    { kind: 'ENUM', name: 'IssueClosedStateReason', values: ['COMPLETED', 'NOT_PLANNED', 'DUPLICATE'] },
    { kind: 'ENUM', name: 'PullRequestState', values: ['OPEN', 'CLOSED', 'MERGED'] },
    { kind: 'ENUM', name: 'MergeableState', values: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'] },
    { kind: 'ENUM', name: 'MergeStateStatus', values: ['BEHIND', 'BLOCKED', 'CLEAN', 'DIRTY', 'DRAFT', 'HAS_HOOKS', 'UNKNOWN', 'UNSTABLE'] },
    { kind: 'ENUM', name: 'PullRequestMergeMethod', values: ['MERGE', 'SQUASH', 'REBASE'] },
    { kind: 'ENUM', name: 'RepositoryPermission', values: ['ADMIN', 'MAINTAIN', 'WRITE', 'TRIAGE', 'READ'] },
    { kind: 'ENUM', name: 'RepositoryVisibility', values: ['PRIVATE', 'PUBLIC', 'INTERNAL'] },
    { kind: 'ENUM', name: 'SearchType', values: ['ISSUE', 'ISSUE_ADVANCED', 'REPOSITORY', 'USER', 'DISCUSSION'] },
    { kind: 'ENUM', name: 'IssueOrderField', values: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'] },
    { kind: 'ENUM', name: 'IssueCommentOrderField', values: ['UPDATED_AT'] },
    { kind: 'ENUM', name: 'OrderDirection', values: ['ASC', 'DESC'] },
    { kind: 'ENUM', name: 'PatchStatus', values: ['ADDED', 'DELETED', 'RENAMED', 'COPIED', 'MODIFIED', 'CHANGED'] },
    { kind: 'ENUM', name: 'CommentAuthorAssociation', values: ['MEMBER', 'OWNER', 'MANNEQUIN', 'COLLABORATOR', 'CONTRIBUTOR', 'FIRST_TIME_CONTRIBUTOR', 'FIRST_TIMER', 'NONE'] },
    { kind: 'ENUM', name: 'PullRequestReviewDecision', values: ['CHANGES_REQUESTED', 'APPROVED', 'REVIEW_REQUIRED'] },
    { kind: 'ENUM', name: 'RepositoryAffiliation', values: ['OWNER', 'COLLABORATOR', 'ORGANIZATION_MEMBER'] },
    { kind: 'ENUM', name: '__TypeKind', values: ['SCALAR', 'OBJECT', 'INTERFACE', 'UNION', 'ENUM', 'INPUT_OBJECT', 'LIST', 'NON_NULL'] },
  );

  add(
    { kind: 'INPUT_OBJECT', name: 'IssueOrder', inputFields: { field: 'IssueOrderField!', direction: 'OrderDirection!' } },
    { kind: 'INPUT_OBJECT', name: 'IssueCommentOrder', inputFields: { field: 'IssueCommentOrderField!', direction: 'OrderDirection!' } },
    { kind: 'INPUT_OBJECT', name: 'IssueFilters', inputFields: { states: '[IssueState!]', labels: '[String!]', createdBy: 'String', since: 'DateTime' } },
    { kind: 'INPUT_OBJECT', name: 'CreateIssueInput', inputFields: { repositoryId: 'ID!', title: 'String!', body: 'String', labelIds: '[ID!]', assigneeIds: '[ID!]', milestoneId: 'ID', clientMutationId: 'String' } },
    { kind: 'INPUT_OBJECT', name: 'CloseIssueInput', inputFields: { issueId: 'ID!', stateReason: 'IssueClosedStateReason', clientMutationId: 'String' } },
    { kind: 'INPUT_OBJECT', name: 'ReopenIssueInput', inputFields: { issueId: 'ID!', clientMutationId: 'String' } },
    { kind: 'INPUT_OBJECT', name: 'UpdateIssueInput', inputFields: { id: 'ID!', title: 'String', body: 'String', state: 'IssueState', labelIds: '[ID!]', clientMutationId: 'String' } },
    { kind: 'INPUT_OBJECT', name: 'CreatePullRequestInput', inputFields: { repositoryId: 'ID!', baseRefName: 'String!', headRefName: 'String!', title: 'String!', body: 'String', draft: 'Boolean', maintainerCanModify: 'Boolean', clientMutationId: 'String' } },
    { kind: 'INPUT_OBJECT', name: 'MergePullRequestInput', inputFields: { pullRequestId: 'ID!', mergeMethod: 'PullRequestMergeMethod', commitHeadline: 'String', commitBody: 'String', expectedHeadOid: 'GitObjectID', authorEmail: 'String', clientMutationId: 'String' } },
    { kind: 'INPUT_OBJECT', name: 'ClosePullRequestInput', inputFields: { pullRequestId: 'ID!', clientMutationId: 'String' } },
    { kind: 'INPUT_OBJECT', name: 'UpdatePullRequestInput', inputFields: { pullRequestId: 'ID!', title: 'String', body: 'String', baseRefName: 'String', clientMutationId: 'String' } },
    { kind: 'INPUT_OBJECT', name: 'AddCommentInput', inputFields: { subjectId: 'ID!', body: 'String!', clientMutationId: 'String' } },
  );

  add(
    { kind: 'INTERFACE', name: 'Node', fields: { id: { type: 'ID!' } }, possibleTypes: ['Repository', 'Issue', 'PullRequest', 'IssueComment', 'User', 'Organization', 'Label'] },
    {
      kind: 'INTERFACE',
      name: 'Actor',
      fields: { login: { type: 'String!' }, avatarUrl: { type: 'URI!', args: { size: 'Int' } }, url: { type: 'URI!' }, resourcePath: { type: 'URI!' } },
      possibleTypes: ['User', 'Organization'],
    },
    {
      kind: 'INTERFACE',
      name: 'RepositoryOwner',
      fields: { id: { type: 'ID!' }, login: { type: 'String!' }, avatarUrl: { type: 'URI!', args: { size: 'Int' } }, url: { type: 'URI!' }, resourcePath: { type: 'URI!' } },
      possibleTypes: ['User', 'Organization'],
    },
    { kind: 'INTERFACE', name: 'GitObject', fields: { id: { type: 'ID!' }, oid: { type: 'GitObjectID!' }, abbreviatedOid: { type: 'String!' }, commitUrl: { type: 'URI!' } }, possibleTypes: ['Commit'] },
    { kind: 'UNION', name: 'IssueOrPullRequest', possibleTypes: ['Issue', 'PullRequest'] },
  );

  const actorFields = (kind: 'User' | 'Organization'): Record<string, FieldDef> => ({
    id: { type: 'ID!', resolve: (u: User) => u.nodeId },
    databaseId: { type: 'Int', resolve: (u: User) => u.id },
    login: { type: 'String!', resolve: (u: User) => u.login },
    name: { type: 'String', resolve: (u: User) => u.name },
    email: { type: kind === 'User' ? 'String!' : 'String', resolve: (u: User) => u.email },
    avatarUrl: { type: 'URI!', args: { size: 'Int' }, resolve: (u: User, _a, ctx) => `${ctx.origin}/avatars/${u.login}` },
    url: { type: 'URI!', resolve: (u: User, _a, ctx) => `${ctx.origin}/${u.login}` },
    resourcePath: { type: 'URI!', resolve: (u: User) => `/${u.login}` },
    createdAt: { type: 'DateTime!', resolve: () => '2026-01-01T00:00:00Z' },
    bio: { type: 'String', resolve: () => null },
    company: { type: 'String', resolve: () => null },
    location: { type: 'String', resolve: () => null },
    isViewer: { type: 'Boolean!', resolve: (u: User, _a, ctx) => u === ctx.forge.viewer },
    repository: { type: 'Repository', args: { name: 'String!' }, resolve: (u: User, a, ctx) => ctx.forge.repo(u.login, String(a.name)) ?? null },
    repositories: {
      type: 'RepositoryConnection!',
      args: { ...CONNECTION_ARGS, privacy: 'RepositoryPrivacy', ownerAffiliations: '[RepositoryAffiliation]', orderBy: 'RepositoryOrder' },
      resolve: (u: User, a, ctx) => connection([...ctx.forge.reposById.values()].filter((repo) => repo.owner === u), a),
    },
  });

  add(
    { kind: 'OBJECT', name: 'User', interfaces: ['Node', 'Actor', 'RepositoryOwner'], fields: actorFields('User') },
    { kind: 'OBJECT', name: 'Organization', interfaces: ['Node', 'Actor', 'RepositoryOwner'], fields: actorFields('Organization') },
    { kind: 'ENUM', name: 'RepositoryPrivacy', values: ['PUBLIC', 'PRIVATE'] },
    { kind: 'INPUT_OBJECT', name: 'RepositoryOrder', inputFields: { field: 'String!', direction: 'OrderDirection!' } },
    {
      kind: 'OBJECT',
      name: 'PageInfo',
      fields: { hasNextPage: { type: 'Boolean!' }, hasPreviousPage: { type: 'Boolean!' }, startCursor: { type: 'String' }, endCursor: { type: 'String' } },
    },
    {
      kind: 'OBJECT',
      name: 'RateLimit',
      fields: { limit: { type: 'Int!' }, cost: { type: 'Int!' }, remaining: { type: 'Int!' }, resetAt: { type: 'DateTime!' }, used: { type: 'Int!' }, nodeCount: { type: 'Int!' } },
    },
    { kind: 'OBJECT', name: 'Language', fields: { name: { type: 'String!' }, color: { type: 'String' } } },
    { kind: 'OBJECT', name: 'License', fields: { name: { type: 'String!' }, key: { type: 'String!' }, spdxId: { type: 'String' } } },
    {
      kind: 'OBJECT',
      name: 'Label',
      interfaces: ['Node'],
      fields: {
        id: { type: 'ID!', resolve: (l: Label) => l.nodeId },
        name: { type: 'String!', resolve: (l: Label) => l.name },
        color: { type: 'String!', resolve: (l: Label) => l.color },
        description: { type: 'String', resolve: (l: Label) => l.description },
        isDefault: { type: 'Boolean!', resolve: () => false },
      },
    },
    {
      kind: 'OBJECT',
      name: 'Commit',
      interfaces: ['Node', 'GitObject'],
      fields: {
        id: { type: 'ID!', resolve: (c: { oid: string }) => `C_${Buffer.from(`0:Commit:${c.oid}`).toString('base64url')}` },
        oid: { type: 'GitObjectID!', resolve: (c: { oid: string }) => c.oid },
        abbreviatedOid: { type: 'String!', resolve: (c: { oid: string }) => c.oid.slice(0, 7) },
        commitUrl: { type: 'URI!', resolve: (c: { oid: string; repo: Repo }, _a, ctx) => `${repoUrl(ctx, c.repo)}/commit/${c.oid}` },
        url: { type: 'URI!', resolve: (c: { oid: string; repo: Repo }, _a, ctx) => `${repoUrl(ctx, c.repo)}/commit/${c.oid}` },
        message: { type: 'String!', resolve: (c: { oid: string }, _a, ctx) => ctx.forge.store.commit(c.oid)?.message.replace(/\n+$/, '') ?? '' },
        messageHeadline: { type: 'String!', resolve: (c: { oid: string }, _a, ctx) => (ctx.forge.store.commit(c.oid)?.message ?? '').split('\n')[0] },
        messageBody: { type: 'String!', resolve: (c: { oid: string }, _a, ctx) => (ctx.forge.store.commit(c.oid)?.message ?? '').split('\n').slice(2).join('\n').replace(/\n+$/, '') },
        committedDate: { type: 'DateTime!', resolve: (c: { oid: string }, _a, ctx) => iso((ctx.forge.store.commit(c.oid)?.committer.time ?? 0) * 1000) },
        authoredDate: { type: 'DateTime!', resolve: (c: { oid: string }, _a, ctx) => iso((ctx.forge.store.commit(c.oid)?.author.time ?? 0) * 1000) },
        parents: {
          type: 'CommitConnection!',
          args: CONNECTION_ARGS,
          resolve: (c: { oid: string; repo: Repo }, a, ctx) => connection((ctx.forge.store.commit(c.oid)?.parents ?? []).map((oid) => ({ oid, repo: c.repo })), a),
        },
      },
    },
    ...connectionTypes('Commit', 'Commit'),
    {
      kind: 'OBJECT',
      name: 'Ref',
      interfaces: ['Node'],
      fields: {
        id: { type: 'ID!', resolve: (r: { repo: Repo; name: string }) => `REF_${Buffer.from(`0:Ref:${r.repo.id}:${r.name}`).toString('base64url')}` },
        name: { type: 'String!', resolve: (r: { name: string }) => r.name.replace(/^refs\/(heads|tags)\//, '') },
        prefix: { type: 'String!', resolve: (r: { name: string }) => (r.name.startsWith('refs/tags/') ? 'refs/tags/' : 'refs/heads/') },
        target: { type: 'GitObject', resolve: (r: { repo: Repo; oid: string }, _a, ctx) => typed('Commit', { oid: ctx.forge.store.peel(r.oid), repo: r.repo }) },
        repository: { type: 'Repository!', resolve: (r: { repo: Repo }) => r.repo },
      },
    },
    ...connectionTypes('Ref', 'Ref'),
  );

  const issueLike: Record<string, FieldDef> = {
    databaseId: { type: 'Int', resolve: (i: Issue) => i.id },
    number: { type: 'Int!', resolve: (i: Issue) => i.number },
    title: { type: 'String!', resolve: (i: Issue) => i.title },
    body: { type: 'String!', resolve: (i: Issue) => i.body ?? '' },
    bodyText: { type: 'String!', resolve: (i: Issue) => i.body ?? '' },
    closed: { type: 'Boolean!', resolve: (i: Issue) => i.state === 'closed' },
    closedAt: { type: 'DateTime', resolve: (i: Issue) => iso(i.closedAt) },
    createdAt: { type: 'DateTime!', resolve: (i: Issue) => iso(i.createdAt) },
    updatedAt: { type: 'DateTime!', resolve: (i: Issue) => iso(i.updatedAt) },
    author: { type: 'Actor', resolve: (i: Issue) => ownerTyped(i.user) },
    authorAssociation: { type: 'CommentAuthorAssociation!', resolve: () => 'OWNER' },
    locked: { type: 'Boolean!', resolve: () => false },
    repository: { type: 'Repository!', resolve: (i: Issue) => i.repo },
    labels: { type: 'LabelConnection', args: CONNECTION_ARGS, resolve: (i: Issue, a) => connection(i.labels, a) },
    comments: { type: 'IssueCommentConnection!', args: { ...CONNECTION_ARGS, orderBy: 'IssueCommentOrder' }, resolve: (i: Issue, a) => connection(i.comments, a) },
    assignees: { type: 'UserConnection!', args: CONNECTION_ARGS, resolve: (_i: Issue, a) => connection([], a) },
    viewerCanUpdate: { type: 'Boolean!', resolve: () => true },
    viewerDidAuthor: { type: 'Boolean!', resolve: (i: Issue, _a, ctx) => i.user === ctx.forge.viewer },
  };

  add(
    {
      kind: 'OBJECT',
      name: 'Issue',
      interfaces: ['Node'],
      fields: {
        id: { type: 'ID!', resolve: (i: Issue) => i.nodeId },
        ...issueLike,
        state: { type: 'IssueState!', resolve: (i: Issue) => issueState(i) },
        stateReason: { type: 'IssueStateReason', resolve: (i: Issue) => (i.stateReason === null ? null : i.stateReason.toUpperCase()) },
        url: { type: 'URI!', resolve: (i: Issue, _a, ctx) => `${repoUrl(ctx, i.repo)}/issues/${i.number}` },
        isPinned: { type: 'Boolean', resolve: () => false },
      },
    },
    {
      kind: 'OBJECT',
      name: 'PullRequest',
      interfaces: ['Node'],
      fields: {
        id: { type: 'ID!', resolve: (i: Issue) => i.pull!.nodeId },
        ...issueLike,
        databaseId: { type: 'Int', resolve: (i: Issue) => i.pull!.id },
        state: { type: 'PullRequestState!', resolve: (i: Issue) => pullState(i) },
        url: { type: 'URI!', resolve: (i: Issue, _a, ctx) => `${repoUrl(ctx, i.repo)}/pull/${i.number}` },
        permalink: { type: 'URI!', resolve: (i: Issue, _a, ctx) => `${repoUrl(ctx, i.repo)}/pull/${i.number}` },
        merged: { type: 'Boolean!', resolve: (i: Issue) => i.pull!.merged },
        mergedAt: { type: 'DateTime', resolve: (i: Issue) => iso(i.pull!.mergedAt) },
        mergedBy: { type: 'Actor', resolve: (i: Issue) => (i.pull!.mergedBy === null ? null : ownerTyped(i.pull!.mergedBy)) },
        mergeCommit: { type: 'Commit', resolve: (i: Issue) => (i.pull!.mergeCommitSha === null ? null : { oid: i.pull!.mergeCommitSha, repo: i.repo }) },
        isDraft: { type: 'Boolean!', resolve: (i: Issue) => i.pull!.draft },
        isCrossRepository: { type: 'Boolean!', resolve: () => false },
        maintainerCanModify: { type: 'Boolean!', resolve: () => false },
        headRefName: { type: 'String!', resolve: (i: Issue) => i.pull!.head },
        baseRefName: { type: 'String!', resolve: (i: Issue) => i.pull!.base },
        headRefOid: { type: 'GitObjectID!', resolve: (i: Issue, _a, ctx) => ctx.forge.pullHeadSha(i) },
        baseRefOid: { type: 'GitObjectID!', resolve: (i: Issue, _a, ctx) => ctx.forge.pullBaseSha(i) },
        headRef: { type: 'Ref', resolve: (i: Issue, _a, ctx) => refValue(ctx, i.repo, `refs/heads/${i.pull!.head}`) },
        baseRef: { type: 'Ref', resolve: (i: Issue, _a, ctx) => refValue(ctx, i.repo, `refs/heads/${i.pull!.base}`) },
        headRepository: { type: 'Repository', resolve: (i: Issue) => i.repo },
        baseRepository: { type: 'Repository', resolve: (i: Issue) => i.repo },
        mergeable: {
          type: 'MergeableState!',
          resolve: (i: Issue, _a, ctx) => {
            if (i.pull!.merged || i.state !== 'open') return 'UNKNOWN';
            return ctx.forge.mergeable(i) ? 'MERGEABLE' : 'CONFLICTING';
          },
        },
        mergeStateStatus: {
          type: 'MergeStateStatus!',
          resolve: (i: Issue, _a, ctx) => (i.pull!.merged || i.state !== 'open' ? 'UNKNOWN' : ctx.forge.mergeable(i) ? 'CLEAN' : 'DIRTY'),
        },
        reviewDecision: { type: 'PullRequestReviewDecision', resolve: () => null },
        additions: { type: 'Int!', resolve: (i: Issue, _a, ctx) => ctx.forge.pullFiles(i).reduce((sum, f) => sum + f.additions, 0) },
        deletions: { type: 'Int!', resolve: (i: Issue, _a, ctx) => ctx.forge.pullFiles(i).reduce((sum, f) => sum + f.deletions, 0) },
        changedFiles: { type: 'Int!', resolve: (i: Issue, _a, ctx) => ctx.forge.pullFiles(i).length },
        files: {
          type: 'PullRequestChangedFileConnection',
          args: CONNECTION_ARGS,
          resolve: (i: Issue, a, ctx) =>
            connection(
              ctx.forge.pullFiles(i).map((f) => ({ path: f.filename, additions: f.additions, deletions: f.deletions, changeType: f.status === 'added' ? 'ADDED' : f.status === 'removed' ? 'DELETED' : 'MODIFIED' })),
              a,
            ),
        },
        commits: {
          type: 'PullRequestCommitConnection!',
          args: CONNECTION_ARGS,
          resolve: (i: Issue, a, ctx) => connection(ctx.forge.pullCommits(i).map((oid) => ({ commit: { oid, repo: i.repo }, id: `PRC_${oid}` })), a),
        },
      },
    },
    { kind: 'OBJECT', name: 'PullRequestChangedFile', fields: { path: { type: 'String!' }, additions: { type: 'Int!' }, deletions: { type: 'Int!' }, changeType: { type: 'PatchStatus!' } } },
    { kind: 'OBJECT', name: 'PullRequestCommit', fields: { id: { type: 'ID!' }, commit: { type: 'Commit!' } } },
    {
      kind: 'OBJECT',
      name: 'IssueComment',
      interfaces: ['Node'],
      fields: {
        id: { type: 'ID!', resolve: (c: Comment) => c.nodeId },
        databaseId: { type: 'Int', resolve: (c: Comment) => c.id },
        body: { type: 'String!', resolve: (c: Comment) => c.body },
        bodyText: { type: 'String!', resolve: (c: Comment) => c.body },
        createdAt: { type: 'DateTime!', resolve: (c: Comment) => iso(c.createdAt) },
        updatedAt: { type: 'DateTime!', resolve: (c: Comment) => iso(c.updatedAt) },
        author: { type: 'Actor', resolve: (c: Comment) => ownerTyped(c.user) },
        authorAssociation: { type: 'CommentAuthorAssociation!', resolve: () => 'OWNER' },
        url: { type: 'URI!', resolve: (c: Comment, _a, ctx) => `${repoUrl(ctx, c.issue.repo)}/issues/${c.issue.number}#issuecomment-${c.id}` },
        issue: { type: 'Issue!', resolve: (c: Comment) => c.issue },
        viewerDidAuthor: { type: 'Boolean!', resolve: (c: Comment, _a, ctx) => c.user === ctx.forge.viewer },
      },
    },
    ...connectionTypes('Issue', 'Issue'),
    ...connectionTypes('PullRequest', 'PullRequest'),
    ...connectionTypes('IssueComment', 'IssueComment'),
    ...connectionTypes('Label', 'Label'),
    ...connectionTypes('Repository', 'Repository'),
    ...connectionTypes('User', 'User'),
    ...connectionTypes('PullRequestChangedFile', 'PullRequestChangedFile'),
    ...connectionTypes('PullRequestCommit', 'PullRequestCommit'),
  );

  add({
    kind: 'OBJECT',
    name: 'Repository',
    interfaces: ['Node'],
    fields: {
      id: { type: 'ID!', resolve: (r: Repo) => r.nodeId },
      databaseId: { type: 'Int', resolve: (r: Repo) => r.id },
      name: { type: 'String!', resolve: (r: Repo) => r.name },
      nameWithOwner: { type: 'String!', resolve: (r: Repo) => `${r.owner.login}/${r.name}` },
      owner: { type: 'RepositoryOwner!', resolve: (r: Repo) => ownerTyped(r.owner) },
      description: { type: 'String', resolve: (r: Repo) => r.description },
      homepageUrl: { type: 'URI', resolve: (r: Repo) => r.homepage },
      url: { type: 'URI!', resolve: (r: Repo, _a, ctx) => repoUrl(ctx, r) },
      sshUrl: { type: 'String!', resolve: (r: Repo) => `git@localhost:${r.owner.login}/${r.name}.git` },
      isPrivate: { type: 'Boolean!', resolve: (r: Repo) => r.private },
      visibility: { type: 'RepositoryVisibility!', resolve: (r: Repo) => (r.private ? 'PRIVATE' : 'PUBLIC') },
      isFork: { type: 'Boolean!', resolve: () => false },
      isArchived: { type: 'Boolean!', resolve: (r: Repo) => r.archived },
      isDisabled: { type: 'Boolean!', resolve: () => false },
      isTemplate: { type: 'Boolean!', resolve: () => false },
      isMirror: { type: 'Boolean!', resolve: () => false },
      isLocked: { type: 'Boolean!', resolve: () => false },
      isEmpty: { type: 'Boolean!', resolve: (r: Repo) => r.refs.size === 0 },
      createdAt: { type: 'DateTime!', resolve: (r: Repo) => iso(r.createdAt) },
      updatedAt: { type: 'DateTime!', resolve: (r: Repo) => iso(r.updatedAt) },
      pushedAt: { type: 'DateTime', resolve: (r: Repo) => iso(r.pushedAt) },
      defaultBranchRef: { type: 'Ref', resolve: (r: Repo, _a, ctx) => refValue(ctx, r, `refs/heads/${r.defaultBranch}`) },
      viewerPermission: { type: 'RepositoryPermission', resolve: () => 'ADMIN' },
      viewerCanAdminister: { type: 'Boolean!', resolve: () => true },
      viewerHasStarred: { type: 'Boolean!', resolve: () => false },
      hasIssuesEnabled: { type: 'Boolean!', resolve: (r: Repo) => r.hasIssues },
      hasWikiEnabled: { type: 'Boolean!', resolve: (r: Repo) => r.hasWiki },
      hasProjectsEnabled: { type: 'Boolean!', resolve: (r: Repo) => r.hasProjects },
      hasDiscussionsEnabled: { type: 'Boolean!', resolve: () => false },
      mergeCommitAllowed: { type: 'Boolean!', resolve: () => true },
      squashMergeAllowed: { type: 'Boolean!', resolve: () => true },
      rebaseMergeAllowed: { type: 'Boolean!', resolve: () => true },
      deleteBranchOnMerge: { type: 'Boolean!', resolve: () => false },
      stargazerCount: { type: 'Int!', resolve: () => 0 },
      forkCount: { type: 'Int!', resolve: () => 0 },
      diskUsage: { type: 'Int', resolve: () => 0 },
      primaryLanguage: { type: 'Language', resolve: () => null },
      licenseInfo: { type: 'License', resolve: () => null },
      parent: { type: 'Repository', resolve: () => null },
      issue: { type: 'Issue', args: { number: 'Int!' }, resolve: (r: Repo, a) => {
        const issue = r.issues.get(Number(a.number));
        return issue === undefined || issue.pull !== null ? null : issue;
      } },
      issues: {
        type: 'IssueConnection!',
        args: { ...CONNECTION_ARGS, states: '[IssueState!]', orderBy: 'IssueOrder', labels: '[String!]', filterBy: 'IssueFilters' },
        resolve: (r: Repo, a) => {
          let list = r.issueList.filter((issue) => issue.pull === null);
          const states: string[] | null = Array.isArray(a.states) ? a.states : Array.isArray(a.filterBy?.states) ? a.filterBy.states : null;
          if (states !== null && states.length > 0) list = list.filter((issue) => states.includes(issueState(issue)));
          const labels: string[] | null = Array.isArray(a.labels) ? a.labels : Array.isArray(a.filterBy?.labels) ? a.filterBy.labels : null;
          if (labels !== null && labels.length > 0) list = list.filter((issue) => issue.labels.some((label) => labels.includes(label.name)));
          return connection(sortedIssues(list, a.orderBy), a);
        },
      },
      pullRequest: { type: 'PullRequest', args: { number: 'Int!' }, resolve: (r: Repo, a) => {
        const issue = r.issues.get(Number(a.number));
        return issue === undefined || issue.pull === null ? null : issue;
      } },
      pullRequests: {
        type: 'PullRequestConnection!',
        args: { ...CONNECTION_ARGS, states: '[PullRequestState!]', orderBy: 'IssueOrder', headRefName: 'String', baseRefName: 'String', labels: '[String!]' },
        resolve: (r: Repo, a) => {
          let list = r.issueList.filter((issue) => issue.pull !== null);
          if (Array.isArray(a.states) && a.states.length > 0) list = list.filter((issue) => a.states.includes(pullState(issue)));
          if (typeof a.headRefName === 'string') list = list.filter((issue) => issue.pull!.head === a.headRefName);
          if (typeof a.baseRefName === 'string') list = list.filter((issue) => issue.pull!.base === a.baseRefName);
          return connection(sortedIssues(list, a.orderBy), a);
        },
      },
      issueOrPullRequest: { type: 'IssueOrPullRequest', args: { number: 'Int!' }, resolve: (r: Repo, a) => {
        const issue = r.issues.get(Number(a.number));
        if (issue === undefined) return null;
        return typed(issue.pull === null ? 'Issue' : 'PullRequest', issue);
      } },
      ref: { type: 'Ref', args: { qualifiedName: 'String!' }, resolve: (r: Repo, a, ctx) => {
        const name = String(a.qualifiedName);
        const full = name.startsWith('refs/') ? name : r.refs.has(`refs/heads/${name}`) ? `refs/heads/${name}` : `refs/tags/${name}`;
        return refValue(ctx, r, full);
      } },
      refs: {
        type: 'RefConnection',
        args: { ...CONNECTION_ARGS, refPrefix: 'String!', query: 'String' },
        resolve: (r: Repo, a) => {
          const prefix = String(a.refPrefix ?? 'refs/');
          const refs = [...r.refs.entries()].filter(([name]) => name.startsWith(prefix)).sort((x, y) => (x[0] < y[0] ? -1 : 1)).map(([name, oid]) => ({ repo: r, name, oid }));
          return connection(refs, a);
        },
      },
      labels: { type: 'LabelConnection', args: { ...CONNECTION_ARGS, query: 'String' }, resolve: (r: Repo, a) => connection([...r.labels.values()], a) },
      label: { type: 'Label', args: { name: 'String!' }, resolve: (r: Repo, a) => r.labels.get(String(a.name).toLowerCase()) ?? null },
      assignableUsers: { type: 'UserConnection!', args: CONNECTION_ARGS, resolve: (r: Repo, a) => connection([r.owner], a) },
    },
  });

  add(
    { kind: 'OBJECT', name: 'CreateIssuePayload', fields: { issue: { type: 'Issue' }, clientMutationId: { type: 'String' } } },
    { kind: 'OBJECT', name: 'CloseIssuePayload', fields: { issue: { type: 'Issue' }, clientMutationId: { type: 'String' } } },
    { kind: 'OBJECT', name: 'ReopenIssuePayload', fields: { issue: { type: 'Issue' }, clientMutationId: { type: 'String' } } },
    { kind: 'OBJECT', name: 'UpdateIssuePayload', fields: { issue: { type: 'Issue' }, clientMutationId: { type: 'String' } } },
    { kind: 'OBJECT', name: 'CreatePullRequestPayload', fields: { pullRequest: { type: 'PullRequest' }, clientMutationId: { type: 'String' } } },
    { kind: 'OBJECT', name: 'MergePullRequestPayload', fields: { pullRequest: { type: 'PullRequest' }, actor: { type: 'Actor' }, clientMutationId: { type: 'String' } } },
    { kind: 'OBJECT', name: 'ClosePullRequestPayload', fields: { pullRequest: { type: 'PullRequest' }, clientMutationId: { type: 'String' } } },
    { kind: 'OBJECT', name: 'UpdatePullRequestPayload', fields: { pullRequest: { type: 'PullRequest' }, clientMutationId: { type: 'String' } } },
    { kind: 'OBJECT', name: 'AddCommentPayload', fields: { commentEdge: { type: 'IssueCommentEdge' }, subject: { type: 'Node' }, timelineEdge: { type: 'IssueCommentEdge' }, clientMutationId: { type: 'String' } } },
  );

  add({
    kind: 'OBJECT',
    name: 'Query',
    fields: {
      viewer: { type: 'User!', resolve: (_p, _a, ctx) => ctx.forge.viewer },
      repository: {
        type: 'Repository',
        args: { owner: 'String!', name: 'String!', followRenames: 'Boolean' },
        resolve: (_p, a, ctx) => ctx.forge.visibleRepo(String(a.owner ?? ''), String(a.name ?? '')) ?? null,
      },
      user: { type: 'User', args: { login: 'String!' }, resolve: (_p, a, ctx) => {
        const user = ctx.forge.user(String(a.login));
        return user !== undefined && user.type === 'User' ? user : null;
      } },
      organization: { type: 'Organization', args: { login: 'String!' }, resolve: (_p, a, ctx) => {
        const user = ctx.forge.user(String(a.login));
        return user !== undefined && user.type === 'Organization' ? user : null;
      } },
      repositoryOwner: { type: 'RepositoryOwner', args: { login: 'String!' }, resolve: (_p, a, ctx) => {
        const user = ctx.forge.user(String(a.login));
        return user === undefined ? null : ownerTyped(user);
      } },
      node: { type: 'Node', args: { id: 'ID!' }, resolve: (_p, a, ctx) => lookupNode(ctx, a.id) },
      nodes: { type: '[Node]!', args: { ids: '[ID!]!' }, resolve: (_p, a, ctx) => (Array.isArray(a.ids) ? a.ids.map((id: unknown) => lookupNode(ctx, id)) : []) },
      rateLimit: {
        type: 'RateLimit',
        args: { dryRun: 'Boolean' },
        resolve: () => ({ limit: 5000, cost: 1, remaining: 4999, used: 1, nodeCount: 1, resetAt: iso(Date.now() + 3600_000) }),
      },
      __type: { type: '__Type', args: { name: 'String!' }, resolve: (_p, a) => SCHEMA.get(String(a.name)) ?? null },
      __schema: { type: '__Schema!', resolve: () => ({}) },
    },
  });

  add({
    kind: 'OBJECT',
    name: 'Mutation',
    fields: {
      createIssue: {
        type: 'CreateIssuePayload',
        args: { input: 'CreateIssueInput!' },
        resolve: (_p, a, ctx) => {
          const input = a.input ?? {};
          const repo = requireNode(ctx, input.repositoryId, ['Repository']).value as Repo;
          const labels = Array.isArray(input.labelIds)
            ? input.labelIds.map((id: unknown) => (requireNode(ctx, id, ['Label']).value as Label).name)
            : undefined;
          const issue = wrapForge(() => ctx.forge.createIssue(repo, { title: input.title, body: input.body, labels }));
          return { issue, clientMutationId: input.clientMutationId ?? null };
        },
      },
      closeIssue: {
        type: 'CloseIssuePayload',
        args: { input: 'CloseIssueInput!' },
        resolve: (_p, a, ctx) => {
          const input = a.input ?? {};
          const issue = requireNode(ctx, input.issueId, ['Issue']).value as Issue;
          const reason = typeof input.stateReason === 'string' ? input.stateReason.toLowerCase() : 'completed';
          wrapForge(() => ctx.forge.updateIssue(issue, { state: 'closed', stateReason: reason }));
          return { issue, clientMutationId: input.clientMutationId ?? null };
        },
      },
      reopenIssue: {
        type: 'ReopenIssuePayload',
        args: { input: 'ReopenIssueInput!' },
        resolve: (_p, a, ctx) => {
          const input = a.input ?? {};
          const issue = requireNode(ctx, input.issueId, ['Issue']).value as Issue;
          wrapForge(() => ctx.forge.updateIssue(issue, { state: 'open' }));
          return { issue, clientMutationId: input.clientMutationId ?? null };
        },
      },
      updateIssue: {
        type: 'UpdateIssuePayload',
        args: { input: 'UpdateIssueInput!' },
        resolve: (_p, a, ctx) => {
          const input = a.input ?? {};
          const issue = requireNode(ctx, input.id, ['Issue']).value as Issue;
          const labels = Array.isArray(input.labelIds) ? input.labelIds.map((id: unknown) => (requireNode(ctx, id, ['Label']).value as Label).name) : undefined;
          wrapForge(() => ctx.forge.updateIssue(issue, { title: input.title ?? undefined, body: input.body ?? undefined, state: typeof input.state === 'string' ? input.state.toLowerCase() : undefined, labels }));
          return { issue, clientMutationId: input.clientMutationId ?? null };
        },
      },
      createPullRequest: {
        type: 'CreatePullRequestPayload',
        args: { input: 'CreatePullRequestInput!' },
        resolve: (_p, a, ctx) => {
          const input = a.input ?? {};
          const repo = requireNode(ctx, input.repositoryId, ['Repository']).value as Repo;
          const pull = wrapForge(() => ctx.forge.createPull(repo, { title: input.title, body: input.body, head: input.headRefName, base: input.baseRefName, draft: input.draft }));
          return { pullRequest: pull, clientMutationId: input.clientMutationId ?? null };
        },
      },
      mergePullRequest: {
        type: 'MergePullRequestPayload',
        args: { input: 'MergePullRequestInput!' },
        resolve: (_p, a, ctx) => {
          const input = a.input ?? {};
          const pull = requireNode(ctx, input.pullRequestId, ['PullRequest']).value as Issue;
          wrapForge(() =>
            ctx.forge.mergePull(pull, {
              method: typeof input.mergeMethod === 'string' ? input.mergeMethod.toLowerCase() : 'merge',
              title: input.commitHeadline,
              message: input.commitBody,
              sha: input.expectedHeadOid,
            }),
          );
          return { pullRequest: pull, actor: ownerTyped(ctx.forge.viewer), clientMutationId: input.clientMutationId ?? null };
        },
      },
      closePullRequest: {
        type: 'ClosePullRequestPayload',
        args: { input: 'ClosePullRequestInput!' },
        resolve: (_p, a, ctx) => {
          const input = a.input ?? {};
          const pull = requireNode(ctx, input.pullRequestId, ['PullRequest']).value as Issue;
          wrapForge(() => ctx.forge.updateIssue(pull, { state: 'closed' }));
          return { pullRequest: pull, clientMutationId: input.clientMutationId ?? null };
        },
      },
      updatePullRequest: {
        type: 'UpdatePullRequestPayload',
        args: { input: 'UpdatePullRequestInput!' },
        resolve: (_p, a, ctx) => {
          const input = a.input ?? {};
          const pull = requireNode(ctx, input.pullRequestId, ['PullRequest']).value as Issue;
          wrapForge(() => ctx.forge.updateIssue(pull, { title: input.title ?? undefined, body: input.body ?? undefined }));
          if (typeof input.baseRefName === 'string' && ctx.forge.branchTip(pull.repo, input.baseRefName) !== undefined) {
            pull.pull!.base = input.baseRefName;
            ctx.forge.touchIssue(pull);
          }
          return { pullRequest: pull, clientMutationId: input.clientMutationId ?? null };
        },
      },
      addComment: {
        type: 'AddCommentPayload',
        args: { input: 'AddCommentInput!' },
        resolve: (_p, a, ctx) => {
          const input = a.input ?? {};
          const subject = requireNode(ctx, input.subjectId, ['Issue', 'PullRequest']);
          const issue = subject.value as Issue;
          const comment = wrapForge(() => ctx.forge.addComment(issue, input.body));
          const edge = { cursor: cursorFor(issue.comments.length), node: comment };
          return { commentEdge: edge, subject, timelineEdge: edge, clientMutationId: input.clientMutationId ?? null };
        },
      },
    },
  });

  // Introspection types.
  add(
    {
      kind: 'OBJECT',
      name: '__Schema',
      fields: {
        description: { type: 'String', resolve: () => null },
        queryType: { type: '__Type!', resolve: () => SCHEMA.get('Query') },
        mutationType: { type: '__Type', resolve: () => SCHEMA.get('Mutation') },
        subscriptionType: { type: '__Type', resolve: () => null },
        types: { type: '[__Type!]!', resolve: () => [...SCHEMA.values()] },
        directives: { type: '[__Directive!]!', resolve: () => DIRECTIVES },
      },
    },
    {
      kind: 'OBJECT',
      name: '__Type',
      fields: {
        kind: { type: '__TypeKind!', resolve: (t: IntrospectionType) => t.kind },
        name: { type: 'String', resolve: (t: IntrospectionType) => t.name ?? null },
        description: { type: 'String', resolve: (t: IntrospectionType) => t.description ?? null },
        specifiedByURL: { type: 'String', resolve: () => null },
        fields: {
          type: '[__Field!]',
          args: { includeDeprecated: 'Boolean' },
          resolve: (t: IntrospectionType) => {
            if (t.kind !== 'OBJECT' && t.kind !== 'INTERFACE') return null;
            return Object.entries((t as TypeDef).fields ?? {}).filter(([name]) => !name.startsWith('__')).map(([name, def]) => ({ name, def }));
          },
        },
        interfaces: { type: '[__Type!]', resolve: (t: IntrospectionType) => (t.kind === 'OBJECT' ? ((t as TypeDef).interfaces ?? []).map((name) => SCHEMA.get(name)) : t.kind === 'INTERFACE' ? [] : null) },
        possibleTypes: { type: '[__Type!]', resolve: (t: IntrospectionType) => (t.kind === 'INTERFACE' || t.kind === 'UNION' ? ((t as TypeDef).possibleTypes ?? []).map((name) => SCHEMA.get(name)) : null) },
        enumValues: {
          type: '[__EnumValue!]',
          args: { includeDeprecated: 'Boolean' },
          resolve: (t: IntrospectionType) => (t.kind === 'ENUM' ? ((t as TypeDef).values ?? []).map((name) => ({ name })) : null),
        },
        inputFields: {
          type: '[__InputValue!]',
          args: { includeDeprecated: 'Boolean' },
          resolve: (t: IntrospectionType) => (t.kind === 'INPUT_OBJECT' ? Object.entries((t as TypeDef).inputFields ?? {}).map(([name, type]) => ({ name, type })) : null),
        },
        ofType: { type: '__Type', resolve: (t: IntrospectionType) => ('ofType' in t ? t.ofType : null) },
        isOneOf: { type: 'Boolean', resolve: () => false },
      },
    },
    {
      kind: 'OBJECT',
      name: '__Field',
      fields: {
        name: { type: 'String!', resolve: (f: { name: string }) => f.name },
        description: { type: 'String', resolve: (f: { def: FieldDef }) => f.def.description ?? null },
        args: { type: '[__InputValue!]!', args: { includeDeprecated: 'Boolean' }, resolve: (f: { def: FieldDef }) => Object.entries(f.def.args ?? {}).map(([name, type]) => ({ name, type })) },
        type: { type: '__Type!', resolve: (f: { def: FieldDef }) => typeRef(f.def.type) },
        isDeprecated: { type: 'Boolean!', resolve: () => false },
        deprecationReason: { type: 'String', resolve: () => null },
      },
    },
    {
      kind: 'OBJECT',
      name: '__InputValue',
      fields: {
        name: { type: 'String!', resolve: (v: { name: string }) => v.name },
        description: { type: 'String', resolve: () => null },
        type: { type: '__Type!', resolve: (v: { type: string }) => typeRef(v.type) },
        defaultValue: { type: 'String', resolve: () => null },
        isDeprecated: { type: 'Boolean!', resolve: () => false },
        deprecationReason: { type: 'String', resolve: () => null },
      },
    },
    {
      kind: 'OBJECT',
      name: '__EnumValue',
      fields: {
        name: { type: 'String!', resolve: (v: { name: string }) => v.name },
        description: { type: 'String', resolve: () => null },
        isDeprecated: { type: 'Boolean!', resolve: () => false },
        deprecationReason: { type: 'String', resolve: () => null },
      },
    },
    {
      kind: 'OBJECT',
      name: '__Directive',
      fields: {
        name: { type: 'String!', resolve: (d: { name: string }) => d.name },
        description: { type: 'String', resolve: () => null },
        locations: { type: '[String!]!', resolve: (d: { locations: string[] }) => d.locations },
        args: { type: '[__InputValue!]!', args: { includeDeprecated: 'Boolean' }, resolve: (d: { args: Record<string, string> }) => Object.entries(d.args).map(([name, type]) => ({ name, type })) },
        isRepeatable: { type: 'Boolean!', resolve: () => false },
      },
    },
  );

  const map = new Map<string, TypeDef>();
  for (const type of types) map.set(type.name, type);
  return map;
}

type IntrospectionType = TypeDef | { kind: 'LIST' | 'NON_NULL'; name?: undefined; description?: undefined; ofType: IntrospectionType | null };

const DIRECTIVES = [
  { name: 'include', locations: ['FIELD', 'FRAGMENT_SPREAD', 'INLINE_FRAGMENT'], args: { if: 'Boolean!' } },
  { name: 'skip', locations: ['FIELD', 'FRAGMENT_SPREAD', 'INLINE_FRAGMENT'], args: { if: 'Boolean!' } },
  { name: 'deprecated', locations: ['FIELD_DEFINITION', 'ENUM_VALUE'], args: { reason: 'String' } },
];

function typeRef(type: string): IntrospectionType | null {
  if (type.endsWith('!')) return { kind: 'NON_NULL', ofType: typeRef(type.slice(0, -1)) };
  if (type.startsWith('[')) return { kind: 'LIST', ofType: typeRef(type.slice(1, -1)) };
  return SCHEMA.get(type) ?? null;
}

function namedType(type: string): string {
  return type.replace(/[[\]!]/g, '');
}

const SCHEMA: Map<string, TypeDef> = buildSchema();

function refValue(ctx: Ctx, repo: Repo, name: string): { repo: Repo; name: string; oid: string } | null {
  void ctx;
  const oid = repo.refs.get(name);
  return oid === undefined ? null : { repo, name, oid };
}

// ---------------------------------------------------------------- validation

interface GqlError {
  message: string;
  locations?: Array<{ line: number; column: number }>;
  path?: Array<string | number>;
  type?: string;
  extensions?: Record<string, unknown>;
}

function fieldsOf(typeName: string): Record<string, FieldDef> | null {
  const type = SCHEMA.get(typeName);
  if (type === undefined) return null;
  if (type.kind === 'OBJECT' || type.kind === 'INTERFACE') return type.fields ?? {};
  if (type.kind === 'UNION') return {};
  return null;
}

function validate(doc: Document, operation: Operation): GqlError[] {
  const errors: GqlError[] = [];
  const opName = `${operation.kind} ${operation.name ?? 'anonymous'}`;
  const visitedFragments = new Set<string>();
  const walk = (typeName: string, selections: Selection[], path: string[]): void => {
    const fields = fieldsOf(typeName);
    for (const selection of selections) {
      if (selection.kind === 'field') {
        if (selection.name === '__typename') continue;
        const def = fields?.[selection.name];
        if (def === undefined) {
          errors.push({
            path: [...path, selection.name],
            extensions: { code: 'undefinedField', typeName, fieldName: selection.name },
            locations: [{ line: selection.line, column: selection.column }],
            message: `Field '${selection.name}' doesn't exist on type '${typeName}'`,
          });
          continue;
        }
        const target = namedType(def.type);
        const targetType = SCHEMA.get(target);
        const composite = targetType !== undefined && (targetType.kind === 'OBJECT' || targetType.kind === 'INTERFACE' || targetType.kind === 'UNION');
        if (composite && selection.selections === null) {
          errors.push({
            path: [...path, selection.name],
            extensions: { code: 'selectionMismatch', nodeName: `field '${selection.name}'`, typeName: target },
            locations: [{ line: selection.line, column: selection.column }],
            message: `Field must have selections (field '${selection.name}' returns ${target} but has no selections. Did you mean '${selection.name} { ... }'?)`,
          });
          continue;
        }
        if (!composite && selection.selections !== null) {
          errors.push({
            path: [...path, selection.name],
            extensions: { code: 'selectionMismatch', nodeName: `field '${selection.name}'`, typeName: target },
            locations: [{ line: selection.line, column: selection.column }],
            message: `Selections can't be made on scalars (field '${selection.name}' returns ${target} but has selections [${selection.selections.map((s) => (s.kind === 'field' ? s.name : '...')).join(', ')}])`,
          });
          continue;
        }
        if (selection.selections !== null) walk(target, selection.selections, [...path, selection.alias ?? selection.name]);
      } else if (selection.kind === 'inline') {
        const condition = selection.typeCondition ?? typeName;
        if (!SCHEMA.has(condition)) {
          errors.push({ path, extensions: { code: 'undefinedType', typeName: condition }, message: `No such type ${condition}, so it can't be a fragment condition` });
          continue;
        }
        walk(condition, selection.selections, [...path, `... on ${condition}`]);
      } else {
        const fragment = doc.fragments.get(selection.name);
        if (fragment === undefined) {
          errors.push({ path, extensions: { code: 'useAndDefineFragment', fragmentName: selection.name }, locations: [{ line: selection.line, column: selection.column }], message: `Fragment ${selection.name} was used, but not defined` });
          continue;
        }
        if (visitedFragments.has(selection.name)) continue;
        visitedFragments.add(selection.name);
        if (!SCHEMA.has(fragment.typeCondition)) {
          errors.push({ path, extensions: { code: 'undefinedType', typeName: fragment.typeCondition }, message: `No such type ${fragment.typeCondition}, so it can't be a fragment condition` });
          continue;
        }
        walk(fragment.typeCondition, fragment.selections, [...path, `...${selection.name}`]);
      }
    }
  };
  const root = operation.kind === 'mutation' ? 'Mutation' : 'Query';
  if (operation.kind === 'subscription') {
    errors.push({ message: 'Schema is not configured for subscriptions' });
    return errors;
  }
  walk(root, operation.selections, [opName]);
  return errors;
}

// ---------------------------------------------------------------- execution

function valueOf(value: Value, variables: Record<string, unknown>): unknown {
  switch (value.kind) {
    case 'var':
      return variables[value.name] ?? null;
    case 'int':
    case 'float':
      return value.value;
    case 'string':
      return value.value;
    case 'bool':
      return value.value;
    case 'null':
      return null;
    case 'enum':
      return value.value;
    case 'list':
      return value.values.map((item) => valueOf(item, variables));
    case 'object': {
      const out: Record<string, unknown> = {};
      for (const [name, item] of value.fields) out[name] = valueOf(item, variables);
      return out;
    }
  }
}

function skipped(directives: Directive[], variables: Record<string, unknown>): boolean {
  for (const directive of directives) {
    const arg = directive.args.find(([name]) => name === 'if');
    const condition = arg === undefined ? true : valueOf(arg[1], variables) === true;
    if (directive.name === 'skip' && condition) return true;
    if (directive.name === 'include' && !condition) return true;
  }
  return false;
}

function implementsType(runtime: string, condition: string): boolean {
  if (runtime === condition) return true;
  const type = SCHEMA.get(runtime);
  if (type?.interfaces?.includes(condition)) return true;
  const cond = SCHEMA.get(condition);
  return cond?.kind === 'UNION' && (cond.possibleTypes ?? []).includes(runtime);
}

class Executor {
  readonly errors: GqlError[] = [];
  private readonly doc: Document;
  private readonly variables: Record<string, unknown>;
  private readonly ctx: Ctx;

  constructor(doc: Document, variables: Record<string, unknown>, ctx: Ctx) {
    this.doc = doc;
    this.variables = variables;
    this.ctx = ctx;
  }

  /** Group selections by response key for a runtime type (fragments flattened, directives applied). */
  private collect(runtime: string, selections: Selection[], out: Map<string, Field[]>, visited: Set<string>): void {
    for (const selection of selections) {
      if (skipped(selection.directives, this.variables)) continue;
      if (selection.kind === 'field') {
        const key = selection.alias ?? selection.name;
        const list = out.get(key);
        if (list === undefined) out.set(key, [selection]);
        else list.push(selection);
      } else if (selection.kind === 'inline') {
        if (selection.typeCondition === null || implementsType(runtime, selection.typeCondition)) this.collect(runtime, selection.selections, out, visited);
      } else {
        if (visited.has(selection.name)) continue;
        const fragment = this.doc.fragments.get(selection.name);
        if (fragment === undefined) continue;
        if (!implementsType(runtime, fragment.typeCondition)) continue;
        visited.add(selection.name);
        this.collect(runtime, fragment.selections, out, visited);
      }
    }
  }

  executeSelections(typeName: string, value: unknown, selections: Selection[], path: Array<string | number>): Record<string, unknown> {
    const grouped = new Map<string, Field[]>();
    this.collect(typeName, selections, grouped, new Set());
    const fields = fieldsOf(typeName) ?? {};
    const out: Record<string, unknown> = {};
    for (const [key, group] of grouped) {
      const field = group[0]!;
      if (field.name === '__typename') {
        out[key] = typeName;
        continue;
      }
      const def = fields[field.name];
      if (def === undefined) {
        out[key] = null;
        continue;
      }
      const args: Record<string, unknown> = {};
      for (const [name, arg] of field.args) args[name] = valueOf(arg, this.variables);
      let raw: unknown;
      try {
        raw = def.resolve !== undefined ? def.resolve(value, args, this.ctx) : (value as Record<string, unknown> | null)?.[field.name];
      } catch (err) {
        const message = err instanceof Error ? err.message : String(err);
        const error: GqlError = { type: err instanceof ResolveError ? err.type : 'INTERNAL', path: [...path, key], locations: [{ line: field.line, column: field.column }], message };
        this.errors.push(error);
        out[key] = null;
        continue;
      }
      const sub: Selection[] = [];
      for (const item of group) if (item.selections !== null) sub.push(...item.selections);
      out[key] = this.complete(def.type, raw, sub, [...path, key]);
    }
    return out;
  }

  private complete(type: string, raw: unknown, selections: Selection[], path: Array<string | number>): unknown {
    if (type.endsWith('!')) return this.complete(type.slice(0, -1), raw, selections, path);
    if (raw === null || raw === undefined) return null;
    if (type.startsWith('[')) {
      if (!Array.isArray(raw)) return null;
      const inner = type.slice(1, -1);
      return raw.map((item, index) => this.complete(inner, item, selections, [...path, index]));
    }
    const def = SCHEMA.get(type);
    if (def === undefined || def.kind === 'SCALAR' || def.kind === 'ENUM') return raw;
    if (def.kind === 'OBJECT') {
      const value = typeof raw === 'object' && raw !== null && TYPED in raw ? (raw as Typed).value : raw;
      return this.executeSelections(type, value, selections, path);
    }
    // Interface or union: the resolver returns a typed envelope naming the runtime type.
    if (typeof raw === 'object' && raw !== null && TYPED in raw) {
      const envelope = raw as Typed;
      return this.executeSelections(envelope[TYPED], envelope.value, selections, path);
    }
    return null;
  }
}

function coerceVariables(operation: Operation, given: unknown): Record<string, unknown> {
  const provided = given !== null && typeof given === 'object' && !Array.isArray(given) ? (given as Record<string, unknown>) : {};
  const out: Record<string, unknown> = { ...provided };
  for (const definition of operation.variables) {
    if (out[definition.name] === undefined && definition.defaultValue !== null) out[definition.name] = valueOf(definition.defaultValue, {});
  }
  return out;
}

export interface GraphQLRequest {
  query?: unknown;
  variables?: unknown;
  operationName?: unknown;
}

export class GraphQL {
  private readonly forge: Forge;
  private readonly documentCache = new Map<string, Document | GraphQLSyntaxError>();

  constructor(forge: Forge) {
    this.forge = forge;
  }

  private parse(source: string): Document | GraphQLSyntaxError {
    const cached = this.documentCache.get(source);
    if (cached !== undefined) return cached;
    let parsed: Document | GraphQLSyntaxError;
    try {
      parsed = parseDocument(source);
    } catch (err) {
      if (!(err instanceof GraphQLSyntaxError)) throw err;
      parsed = err;
    }
    if (this.documentCache.size > 500) this.documentCache.clear();
    this.documentCache.set(source, parsed);
    return parsed;
  }

  execute(request: GraphQLRequest, origin: string): Record<string, unknown> {
    if (typeof request.query !== 'string' || request.query.trim() === '') {
      return { errors: [{ message: 'A query attribute must be specified and must be a string.' }] };
    }
    const doc = this.parse(request.query);
    if (doc instanceof GraphQLSyntaxError) {
      return { errors: [{ message: doc.message, locations: [{ line: doc.line, column: doc.column }] }] };
    }
    let operation: Operation | undefined;
    const name = typeof request.operationName === 'string' && request.operationName !== '' ? request.operationName : null;
    if (name !== null) {
      operation = doc.operations.find((op) => op.name === name);
      if (operation === undefined) return { errors: [{ message: `No operation named "${name}"` }] };
    } else if (doc.operations.length === 1) {
      operation = doc.operations[0];
    } else if (doc.operations.length === 0) {
      return { errors: [{ message: 'No operations in query document' }] };
    } else {
      return { errors: [{ message: 'An operation name is required' }] };
    }
    const op = operation!;
    const errors = validate(doc, op);
    if (errors.length > 0) return { errors };
    const variables = coerceVariables(op, request.variables);
    for (const definition of op.variables) {
      if (definition.type.endsWith('!') && (variables[definition.name] === undefined || variables[definition.name] === null)) {
        return { errors: [{ extensions: { value: null, problems: [{ path: [], explanation: 'Expected value to not be null' }] }, locations: [], message: `Variable $${definition.name} of type ${definition.type} was provided invalid value` }] };
      }
    }
    const executor = new Executor(doc, variables, { forge: this.forge, origin });
    const data = executor.executeSelections(op.kind === 'mutation' ? 'Mutation' : 'Query', null, op.selections, []);
    return executor.errors.length > 0 ? { data, errors: executor.errors } : { data };
  }
}
