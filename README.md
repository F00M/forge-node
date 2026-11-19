# forge-node

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/F00M/forge-node/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/F00M/forge-node/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A single-process, GitHub-compatible git forge for Node 22, written in TypeScript that runs through Node's
type stripping. It uses the standard library only: no dependencies, no build step and no `git` binary.
One in-memory store serves four surfaces behind one bearer token:

- **Git smart HTTP** at `/{owner}/{repo}.git`: protocol v2 `upload-pack` (`ls-refs`, `fetch` with
  shallow/deepen, `want-ref`, `include-tag`), the v0 advertisement, and `receive-pack` with
  `report-status`, `side-band-64k`, `delete-refs` and `ofs-delta`. Packfiles are read (OFS/REF deltas,
  thin packs) and written by hand with `node:zlib` and `node:crypto`.
- **REST v3** at `/api/v3`: user, repositories, contents, branches, commits, git refs/trees/blobs/tags,
  issues, comments, labels, pull requests (files, commits, merge/squash/rebase), with `Link` pagination
  and GitHub-shaped errors.
- **GraphQL** at `/api/graphql`: its own parser and a schema-driven executor with variables, fragments,
  aliases, directives, connections with cursors, global node ids, mutations and introspection.
- **MCP** at `/mcp`: streamable HTTP JSON-RPC with `server/discover` (protocol `2026-07-28`) and the
  legacy `initialize` handshake, and 14 tools for repositories, issues, pull requests and branches.

The HTTP/1.1 server is a small parser over `node:net` (keep-alive, pipelining, chunked bodies,
`Expect: 100-continue`), and hot responses are cached until the data behind them changes.

| Path | What it holds |
| --- | --- |
| `main.ts` | the env lifecycle: listen on an ephemeral port, register, wait for done |
| `src/http.ts` | the HTTP/1.1 server |
| `src/git.ts` | objects, trees, commits, packfiles, deltas, pkt-lines |
| `src/smart.ts` | git smart HTTP (upload-pack v0/v2, receive-pack) |
| `src/forge.ts` | the domain model: repositories, refs, issues, labels, pull requests, merges |
| `src/views.ts` | the REST JSON shapes |
| `src/rest.ts` | REST v3 routes |
| `src/gql-parse.ts`, `src/graphql.ts` | GraphQL parser, schema and executor |
| `src/mcp.ts` | the MCP server |
| `tools/serve.ts` | a local server on a fixed port for practice |

## Run it

```sh
# Under the bounty's env (MAKEFUN_ENV_URL and MAKEFUN_SHOP_TOKEN are set by the grader):
node main.ts

# Locally, on a fixed port with your own token:
node tools/serve.ts 8099 my-token
git clone http://x:my-token@127.0.0.1:8099/adaiversen/demo.git   # after creating "demo" over REST
curl -H "Authorization: Bearer my-token" http://127.0.0.1:8099/api/v3/user
```

---

<sub>Entered on [make.fun](https://make.fun/?ref=yd04c6gr&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
