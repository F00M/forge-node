# forge-node

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/F00M/forge-node/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/F00M/forge-node/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A single-process, GitHub-compatible git forge. One in-memory store serves four surfaces behind one
bearer token, with no dependencies, no build step at run time and no `git` binary:

- **Git smart HTTP** at `/{owner}/{repo}.git`: protocol v2 `upload-pack` (`ls-refs`, `fetch` with
  shallow/deepen, `want-ref`, `include-tag`), the v0 advertisement, and `receive-pack` with
  `report-status`, `side-band-64k`, `delete-refs` and `ofs-delta`. Packfiles are read (OFS/REF deltas,
  thin packs) and written by hand.
- **REST v3** at `/api/v3`: user, repositories, contents, branches, commits, git refs/trees/blobs/tags,
  issues, comments, labels, pull requests (files, commits, merge/squash/rebase), with `Link` pagination
  and GitHub-shaped errors.
- **GraphQL** at `/api/graphql`: its own parser and a schema-driven executor with variables, fragments,
  aliases, directives, connections with cursors, global node ids, mutations and introspection.
- **MCP** at `/mcp`: streamable HTTP JSON-RPC with `server/discover` (protocol `2026-07-28`) and the
  legacy `initialize` handshake, and 14 tools for repositories, issues, pull requests and branches.

The forge exists twice, with the same behaviour and byte-identical exported state:

- **The native core** (`core/`, Go standard library only), committed as a statically linked
  linux/amd64 binary in `bin/forge-linux-amd64`. Its HTTP/1.1 server is written over raw sockets
  (keep-alive, pipelined answers batched into one write, chunked bodies, `Expect: 100-continue`); on
  Linux the first connections are served with plain blocking `read`/`write` calls on their own socket,
  one of each per request, instead of a poller round trip.
- **The Node 22 forge** (`src/`, TypeScript through Node's type stripping, `node:` modules only), which
  `main.ts` falls back to when the core cannot start on the platform.

`main.ts` (run as `node main.ts`) starts the core on an ephemeral port, waits for its `LISTEN` line and
a healthy `/healthz`, registers the port with the env, polls until the env says done, then stops it.

| Path | What it holds |
| --- | --- |
| `main.ts` | the env lifecycle: start the core (or the Node forge), register, wait for done |
| `bin/forge-linux-amd64` | the core, built from `core/` (see below) |
| `core/http.go`, `core/iomode_*.go` | the core's HTTP/1.1 server and its per-platform socket I/O |
| `core/git.go`, `core/smart.go` | objects, packfiles, deltas, pkt-lines; upload-pack v0/v2, receive-pack |
| `core/forge.go`, `core/views.go`, `core/rest.go`, `core/params.go` | domain model, REST JSON shapes, REST v3 routes |
| `core/gqlparse.go`, `core/graphql.go` | GraphQL parser, schema and executor |
| `core/mcp.go`, `core/app.go`, `core/jsonx.go` | MCP server, top-level routing and auth, ordered JSON |
| `src/` | the Node forge: `http.ts`, `git.ts`, `smart.ts`, `forge.ts`, `views.ts`, `rest.ts`, `gql-parse.ts`, `graphql.ts`, `mcp.ts`, `app.ts` |
| `tools/serve.ts` | the Node forge on a fixed port for practice |

## Run it

```sh
# Under the bounty's env (MAKEFUN_ENV_URL and MAKEFUN_SHOP_TOKEN are set by the grader):
node main.ts

# The core alone, on a fixed port with your own token:
MAKEFUN_SHOP_TOKEN=my-token bin/forge-linux-amd64 --port 8099
# or the Node forge:
node tools/serve.ts 8099 my-token

git clone http://x:my-token@127.0.0.1:8099/adaiversen/demo.git   # after creating "demo" over REST
curl -H "Authorization: Bearer my-token" http://127.0.0.1:8099/api/v3/user
```

## Rebuild the core

```sh
cd core
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o ../bin/forge-linux-amd64 .
```

---

<sub>Entered on [make.fun](https://make.fun/?ref=yd04c6gr&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
