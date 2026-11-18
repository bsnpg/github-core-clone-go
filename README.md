# github-core-clone-go

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/bsnpg/github-core-clone/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/bsnpg/github-core-clone/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

Referral code: `26zqsqqz`

A standalone GitHub-compatible git forge in one process, built for [make.fun's github-core-clone-01](https://make.fun/bounty/github-core-clone-01): a git smart-HTTP server, the REST v3 API, GraphQL v4, and an MCP endpoint, all over one in-memory store. No dependencies, no git binary, no outside service, no proxy — a statically linked Go binary only. This repository is a Go port of the Node entry `github-core-clone`, written to beat its performance while keeping the same wire behaviour.

## Run it

```sh
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o forge .
node main.ts
```

The platform runs `node /bot/main.ts` on Node 22. `main.ts` is only a thin
launcher: it finds the vendored `./forge` binary next to it, spawns it with
inherited stdio, forwards `SIGINT`/`SIGTERM`, and exits with the binary's
exit code. All of the forge's behaviour lives in Go; the launcher carries
no environment glue and no server logic.

## The lifecycle

The env is a server the harness starts beside yours. Its origin is in
`MAKEFUN_ENV_URL` (`http://store:8080` in a trial).

1. **Bind an ephemeral port.** The Go server listens on port 0 and lets the
   kernel pick one. Never bind 8080: that is the env's port.
2. **Register.** `POST $MAKEFUN_ENV_URL/register` with
   `{"port": <the port you bound>}`, as JSON. The first register wins.
3. **Wait for done.** `GET $MAKEFUN_ENV_URL/done` answers `{"done":false}`
   while the suite runs and `{"done":true}` once its export is written.
   Poll it with a short backoff; never exit before it says `true`.
4. **Exit 0** when it does.

## The surface

- **Git smart HTTP** at `/{owner}/{repo}.git`: upload-pack protocol v2
  (`ls-refs`, `fetch`, including shallow/deepen and thin packs) and
  receive-pack v0 (`report-status`, `side-band-64k`, `delete-refs`), with
  fast-forward enforcement — a non-fast-forward push is refused per ref
  with `ng <ref> non-fast-forward` and the ref does not move. Packs are
  written and read by hand: SHA-1 object ids, zlib objects, plain pack v2,
  no deltas.
- **REST v3** under `/api/v3`: repositories, branches, commits, git refs,
  contents (create/update/delete with stale-sha `409`), labels, issues,
  comments, pull requests (including squash merge with a real merge commit
  on the default branch), `Link` pagination, `X-GitHub-Request-Id` on every
  reply, JSON errors with a string `message`, `404` JSON for unknown
  routes, `400` for malformed JSON, `401` for a missing bearer token, and
  `404` (never `403`) for a repository under another owner.
- **GraphQL v4** at `/api/graphql`: `viewer`, `repository`, `node(id:)`
  with inline fragments, `search`, `__type`/`__schema` introspection,
  cursor pagination on `issues`, and the `createIssue` / `closeIssue` /
  `createPullRequest` / `mergePullRequest` / `addComment` mutations.
  Global ids are opaque (`R<id>`, `I<id>-<n>`, `P<id>-<n>`) and stable
  across reads. A GraphQL-level failure is an HTTP 200 carrying `errors`;
  two operation documents in one body answer two bodies in order.
- **MCP** at `/mcp`: JSON-RPC 2.0 Streamable HTTP, modern revision
  `2026-07-28` (`server/discover` answers identity, capabilities and
  `supportedVersions`) with the legacy `initialize` handshake also
  answered. Six tools — `create_repo`, `get_repo`, `create_issue`,
  `list_issues`, `create_pull_request`, `merge_pull_request` — acting on
  the same store as REST. An unknown tool is JSON-RPC `-32602`, a call
  without its required arguments answers `isError: true`, and a body that
  is not JSON is HTTP 400 with JSON-RPC `-32700`.

## Determinism

A trial is replayed and its state export must come out byte for byte. So
the forge never reads a wall clock and never draws unseeded randomness:
every timestamp comes from `MAKEFUN_TRIAL_NOW`, every id is a counter,
every request id is a counter, and the one request at a time is answered
in the order it arrived. The store is in-memory and fresh per trial.

## Layout

| File | What it holds |
| --- | --- |
| `main.ts` | Thin launcher: spawns the vendored `./forge` binary. |
| `internal/util/util.go` | Trial time, SHA-1, base64, and the deterministic id helpers. |
| `internal/gitobj/gitobj.go` | Git object model: hashing, commit/tree build and parse, reachability walks. |
| `internal/store/store.go` | The in-memory store: repos, objects, refs, issues, pulls, merges. |
| `internal/server/` | (to follow) HTTP transport, git smart-HTTP, REST, GraphQL and MCP handlers. |

## Performance

The score is conformance (600) + agent-native (200) + speed (200). The
goal is the same conformance and agent-native maxima as the Node entry,
with a faster median: a single static Go binary, no interpreter startup,
cheap per-request allocation, and the same cached single-issue view the
Node entry uses for the read workload.
