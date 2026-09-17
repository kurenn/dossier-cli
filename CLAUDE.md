# dossier-cli

A Go command-line client for the [Dossier](https://github.com/kurenn/dossier) vault's
API v1. Single static binary, no runtime.

## Where this sits

This repository holds only the client. The API, its reference and its product rules live
in `kurenn/dossier` (private), and the design this build follows is `docs/cli-plan.md`
**there**, not here — it documents the private API's internals, including a candid list
of its residual gaps, so it is deliberately not published alongside a public binary.

That split has one real consequence, and it is worth stating rather than discovering: a
schema change and this client's reaction to it are now two pull requests in two
repositories. The plan originally put the CLI inside the Rails repo precisely so they
would be one diff. The compensating control is the contract job, which is now built and
runs on every push to the Rails repo — see "The contract suite".

## Non-negotiables

These are inherited from the product and are cheap to break by accident.

1. **`/api/v1` and nothing else.** Never a web route, never a cookie, never scraped
   HTML. Enforced in `internal/api.Client.Do` before a socket opens, so it holds by
   construction rather than by care. Everything the API reference proves about the
   API — no field value reaching a recipient who was not given it, no un-burned burn
   share, no credential management — is true of this client only because of that line.
2. **The server decides state.** Share state, expiry, lockout and token validity are
   rendered, never computed. A client that decides whether a link has expired is a
   suggestion, not a product.
3. **The schema is the source for every number and vocabulary.** Scopes, limits, filter
   values, expiry presets: read from `GET /api/v1/schema`, cached 24h. A literal list of
   scopes or statuses in this codebase is a defect, not a shortcut.
4. **A token never touches argv.** Hidden prompt or `--token-stdin`. `--token` exists
   only to be refused with the reason. Shell history and `ps` are why.
5. **`0600` is a refusal, not a warning.** A credentials file readable by anyone else
   stops the command with exit 2 and the `chmod` to run. There is no degraded mode: a
   warning in a cron job is a line nobody reads, and the credential is exposed either way.
6. **Expiry is required, never defaulted.** When `shares mint` lands, exactly one of
   `--expires` or `--no-expiry` is mandatory, and "no expiry" is rendered as the labelled
   exception it is — not as a peer option.
7. **The exit codes are a contract.** `internal/exitcode` may gain codes; it may never
   renumber one. Scripts and agents branch on these.
8. **API copy is verbatim.** A `message` or `hint` from an envelope is printed unchanged.
   The one deliberate exception is the 401, where §5.5's copy explains an ambiguity the
   API's own hint does not — see `internal/cli/unauthenticated.go`, which says why.
9. **Colour is state, and state is complete without it.** Only the state word and the
   countdown, only on a TTY, and `EXPIRED` takes no colour in either mode. Every golden
   test asserts the stripped output still reads correctly.
10. **Prose and data never share a line.** Data goes in labelled blocks and aligned
    tables; prose goes in sentences above or below them. This is how rule 7's
    mono-versus-sans split is re-earned in a terminal that has one typeface.

## Layout

| Path | What it holds |
|---|---|
| `cmd/dossier/` | The entry point, deliberately almost empty — everything testable lives in `internal/cli` |
| `internal/cli/` | Command tree, the `App` context, error-to-exit classification |
| `internal/api/` | HTTP transport, the error envelope, the schema document |
| `internal/store/` | XDG paths, `config.toml`, `credentials.toml`, the schema cache |
| `internal/render/` | Label-column blocks, colour detection, state words |
| `internal/exitcode/` | The exit-code contract |
| `testdata/golden/` | Byte-exact expected output |

## Commands

```bash
go build -o dossier ./cmd/dossier   # build
go test ./...                       # suite
go test ./internal/cli -update      # rewrite golden files
gofmt -l .                          # format check (must print nothing)
go vet ./...                        # vet
staticcheck ./...                   # lint
govulncheck ./...                   # dependency vulnerabilities
```

## Dev-loop config

- **Install / prepare:** `go mod download`
- **Test:** `go test ./... -race -covermode=atomic -coverpkg=./...` (floor: 85% total)
- **Lint:** `gofmt -l .` and `go vet ./...` and `staticcheck ./...`
- **Typecheck:** the compiler — `go build ./...`
- **Security:** `govulncheck ./...`
- **Checkpoints:** none — autonomous by the owner's standing instruction
- **Critical paths:**
  - `internal/api/client.go` — the `/api/v1` restriction, the retry policy, the redirect
    refusal, the absence of a cookie jar
  - `internal/store/credentials.go` and `paths.go` — the `0600` refusal and the atomic
    write
  - `internal/exitcode/` — the contract
  - `internal/cli/login.go` — the one command that handles a raw credential
  - anything that will send an `Idempotency-Key`, once M3 lands
- **Fix-round cap:** 2
- **Extra rating axes:** fidelity to `docs/cli-plan.md` in the Rails repo; compliance
  with the ten non-negotiables above
- **PR conventions:** conventional commit subjects.

## Coverage is measured with `-coverpkg=./...`

Most of the transport and store code is exercised through the command tests, which is
the right place to test it — the exit code a command produces is what callers depend on.
Per-package coverage reports those packages as thinly tested and measures the wrong
thing. The CI gate uses `-coverpkg=./...` for that reason.

## Testing notes

- `internal/cli/helpers_test.go` has the harness: injected streams, a frozen clock, XDG
  roots in a temp dir, and a fake API that **fails the test if anything outside
  `/api/v1` is requested**.
- `App.IsInteractive` is injected so the prompting paths are testable without a pty.
  Production leaves it nil and gets the real terminal check.
- Golden files are regenerated with `-update`. Read the diff rather than accepting it:
  a golden file will happily record a misalignment bug as correct, and did once already.

## Not yet built

Milestone 0 is done: `version`, `schema`, `login`, `logout`, `whoami`, `profiles`, the
transport, the stores and CI. Still to come, in the plan's order — `fields list/create`,
`documents attach`, `shares list/show`, `shares mint` with the idempotency ledger,
`open`, and shell completion.

## The contract suite

`internal/cli/contract_test.go`, behind `//go:build contract`. It is the only test here
that needs a real Dossier on a socket, and its job is not to test the CLI again — it is to
**arbitrate the fakes**. `schemaFixture` and the `httptest` handlers were written from
`docs/api.md`, so they agree with the client by construction, including where both are
wrong. The contract suite is what makes them falsifiable.

```bash
go test -tags contract -run TestContract ./internal/cli
```

It needs `DOSSIER_CONTRACT_HOST` and a fixture at `testdata/contract.json`, generated by
`RAILS_ENV=test bin/rails cli:seed_contract` in the Rails repo against the same database
the server is using.

**It runs in `kurenn/dossier`'s CI, not here** — see the `contract` job in that repo's
`.github/workflows/ci.yml`. That direction is the one that needs no new credential: the
job lives in the private repo and checks out this public one, so the default
`GITHUB_TOKEN` is enough. The reverse would need a PAT for a private repo *and* would put
the API's behaviour in a public repo's logs. It also fires on the change that actually
causes drift, which is a change to the API.

Two consequences worth knowing:

- A red contract job in that repo usually means a fake **here** is now wrong. Fix the
  fake, then regenerate the goldens with `-update` and read the diff.
- The suite's test order is load-bearing and nothing in it runs in parallel. The last
  test deliberately exhausts the per-IP failed-authentication limiter, so a re-run within
  a minute fails on purpose, with a message saying so.

It found one real bug on its first run: `whoami --probe` printed "unknown — rate limited"
for every scope and exited 0, reporting a sweep that determined nothing as a pass. That is
the same defect that had already been fixed for dead tokens, in the one case no fake could
reach.

## Not yet built

Still to come, in the plan's order — `fields list/create`, `documents attach`,
`shares list/show`, `shares mint` with the idempotency ledger, `open`, and shell
completion. Each milestone extends the contract suite: §9.1 also asks for a real mint,
list, show and open round-trip, and for a burn share to refuse a second `open --document`,
none of which can be written before the commands exist.
