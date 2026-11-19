// The entry's HTTP surface: /healthz, git smart HTTP at /{owner}/{repo}.git, REST v3 at /api/v3,
// GraphQL at /api/graphql and MCP at /mcp, all behind the one bearer token.

import type { Server } from 'node:net';
import { Forge } from './forge.ts';
import { GraphQL } from './graphql.ts';
import { createHttpServer, type Request, type Response } from './http.ts';
import { Mcp } from './mcp.ts';
import { JSON_TYPE, Rest, errorJson, json, notFound } from './rest.ts';
import { SmartHttp } from './smart.ts';
import { Views } from './views.ts';

/** The contest scenario's fixed viewer (wire.ts derives it from the fixed seed 0x6a09e667f3bcc909). */
const VIEWER_LOGIN = 'adaiversen';
const VIEWER_NAME = 'Ada Iversen';
const VIEWER_EMAIL = 'adaiversen@example.invalid';
const OTHER_ACCOUNTS: Array<{ login: string; type: 'User' | 'Organization' }> = [
  { login: 'olgabauer', type: 'User' },
  { login: 'quinnkeller', type: 'User' },
  { login: 'faint-meadow-org', type: 'Organization' },
];

const HEALTH = JSON.stringify({ ok: true });

function bearerToken(header: string | undefined): string | null {
  if (header === undefined) return null;
  const value = header.trim();
  const space = value.indexOf(' ');
  if (space < 0) return null;
  const scheme = value.slice(0, space).toLowerCase();
  const credential = value.slice(space + 1).trim();
  if (credential === '') return null;
  if (scheme === 'bearer' || scheme === 'token') return credential;
  if (scheme === 'basic') {
    const decoded = Buffer.from(credential, 'base64').toString('utf8');
    const colon = decoded.indexOf(':');
    if (colon < 0) return decoded === '' ? null : decoded;
    const password = decoded.slice(colon + 1);
    return password !== '' ? password : decoded.slice(0, colon) || null;
  }
  return null;
}

export interface AppOptions {
  /** The token every request must carry; empty accepts any non-empty token. */
  token: string;
}

export function createApp(options: AppOptions = { token: process.env.MAKEFUN_SHOP_TOKEN ?? '' }): Server {
  const forge = new Forge(VIEWER_LOGIN, VIEWER_NAME, VIEWER_EMAIL, OTHER_ACCOUNTS);
  const views = new Views(forge);
  const rest = new Rest(forge, views);
  const smart = new SmartHttp(forge);
  const graphql = new GraphQL(forge);
  const mcp = new Mcp(forge, views);
  const expected = options.token;

  const authorized = (request: Request): boolean => {
    const token = bearerToken(request.headers.authorization);
    if (token === null) return false;
    return expected === '' || token === expected;
  };

  const handle = (request: Request): Response => {
    const path = request.path;

    if (path === '/healthz') return json(200, HEALTH);

    if (path.startsWith('/api/v3')) {
      if (path.length > 7 && path[7] !== '/') return notFound();
      views.setHost(request.headers.host);
      if (!authorized(request)) return errorJson(401, 'Requires authentication');
      const rest_ = path.length <= 8 ? '' : path.slice(8);
      const segments = rest_ === '' ? [] : rest_.split('/');
      if (segments.length > 0 && segments[segments.length - 1] === '') segments.pop();
      return rest.handle(request, segments);
    }

    if (path === '/api/graphql' || path === '/graphql' || path === '/api/graphql/') {
      views.setHost(request.headers.host);
      if (!authorized(request)) return json(401, { message: 'This endpoint requires you to be authenticated.', documentation_url: 'https://docs.github.com/graphql/guides/forming-calls-with-graphql#authenticating-with-graphql' });
      if (request.method !== 'POST' && request.method !== 'GET') return errorJson(405, 'Method Not Allowed');
      let payload: unknown;
      if (request.method === 'GET') {
        const params = new URLSearchParams(request.query);
        let variables: unknown = {};
        try {
          variables = JSON.parse(params.get('variables') ?? '{}');
        } catch {
          variables = {};
        }
        payload = { query: params.get('query') ?? '', variables, operationName: params.get('operationName') };
      } else {
        try {
          payload = JSON.parse(request.body.toString('utf8'));
        } catch {
          return errorJson(400, 'Problems parsing JSON');
        }
      }
      const origin = views.origin;
      if (Array.isArray(payload)) {
        return json(200, payload.map((item) => graphql.execute(item !== null && typeof item === 'object' ? item : {}, origin)));
      }
      if (payload === null || typeof payload !== 'object') return errorJson(400, 'Problems parsing JSON');
      return json(200, graphql.execute(payload, origin));
    }

    if (path === '/mcp' || path === '/mcp/') {
      views.setHost(request.headers.host);
      if (!authorized(request)) {
        return { status: 401, type: JSON_TYPE, body: JSON.stringify({ jsonrpc: '2.0', id: null, error: { code: -32001, message: 'Unauthorized: a bearer token is required' } }), extra: 'WWW-Authenticate: Bearer realm="forge"\r\n' };
      }
      if (request.method === 'GET') return { status: 405, type: JSON_TYPE, body: JSON.stringify({ jsonrpc: '2.0', id: null, error: { code: -32000, message: 'Method Not Allowed: this server does not offer an SSE stream' } }), extra: 'Allow: POST, DELETE\r\n' };
      if (request.method === 'DELETE') return { status: 204, type: JSON_TYPE, body: '' };
      if (request.method !== 'POST') return { status: 405, type: JSON_TYPE, body: JSON.stringify({ jsonrpc: '2.0', id: null, error: { code: -32000, message: 'Method Not Allowed' } }), extra: 'Allow: POST, DELETE\r\n' };
      const answer = mcp.handle(request.body.toString('utf8'));
      return { status: answer.status, type: JSON_TYPE, body: answer.body };
    }

    // Git smart HTTP: /{owner}/{repo}(.git)/info/refs, /git-upload-pack, /git-receive-pack.
    const git = /^\/([^/]+)\/([^/]+?)(?:\.git)?\/(info\/refs|git-upload-pack|git-receive-pack)$/.exec(path);
    if (git !== null) {
      if (!authorized(request)) {
        return { status: 401, type: 'text/plain; charset=utf-8', body: 'Authentication required\n', extra: 'WWW-Authenticate: Basic realm="forge"\r\n' };
      }
      const repo = forge.visibleRepo(git[1]!, git[2]!);
      if (repo === undefined) return { status: 404, type: 'text/plain; charset=utf-8', body: 'Repository not found\n' };
      const action = git[3]!;
      const protocol = request.headers['git-protocol'] ?? '';
      if (action === 'info/refs') {
        if (request.method !== 'GET' && request.method !== 'HEAD') return { status: 405, type: 'text/plain; charset=utf-8', body: 'Method Not Allowed\n' };
        const service = new URLSearchParams(request.query).get('service');
        if (service !== 'git-upload-pack' && service !== 'git-receive-pack') return { status: 403, type: 'text/plain; charset=utf-8', body: 'dumb http is not supported\n' };
        return smart.infoRefs(repo, service, protocol);
      }
      if (request.method !== 'POST') return { status: 405, type: 'text/plain; charset=utf-8', body: 'Method Not Allowed\n' };
      try {
        if (action === 'git-upload-pack') return smart.uploadPack(repo, request.body, protocol);
        return smart.receivePack(repo, request.body);
      } catch (err) {
        return { status: 400, type: 'text/plain; charset=utf-8', body: `${err instanceof Error ? err.message : 'bad request'}\n` };
      }
    }

    if (path === '/' || path === '') return json(200, { name: 'forge', api: '/api/v3', graphql: '/api/graphql', mcp: '/mcp' });
    return notFound();
  };

  return createHttpServer(handle);
}
