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
4. **A secret never touches argv.** A token: hidden prompt or `--token-stdin`. A field's
   value: hidden prompt, `--value-stdin` or `--value-file`. `--token` and `--value` both
   exist only to be refused with the reason, and `fields create` refuses a positional
   argument the same way. Shell history and `ps` are why.
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
| `internal/store/ledger.go` | The mint ledger: one file per idempotency key under `StateDir`, and the 24 h staleness rule that matches the server's claim TTL |
| `internal/render/` | Label-column blocks, aligned tables, colour detection, state words, the deadline/countdown format |
| `internal/api/writes.go` | `POST /fields` and the multipart upload, including why the part declares its own content type |
| `internal/api/mint.go` | `POST /shares`: the body built once, its digest, and the 201 that is the only response carrying a raw PIN |
| `internal/cli/mint.go` | The §6.3 retry table, `--resume`, and the handover block — the only screen that shows a secret once |
| `internal/cli/expiry.go` | ISO 8601 durations, shorthands and timestamps, resolved client-side |
| `internal/api/dossier.go` | The recipient boundary: the one-shot `view`, the document link, and `FetchSigned` — the only request not composed by this CLI |
| `internal/cli/open.go` | `open` and `open --document`: the one-request rule, the PIN's two doors, the withheld bar |
| `internal/exitcode/` | The exit-code contract |
| `internal/cli/completion.go` | Shell completion, and the rule that none of it touches the network |
| `internal/store/permissions_unix.go` | The mode check: what "only the owner can read this" means on Unix |
| `internal/store/permissions_windows.go` | The DACL check and the ACL writer: the same question, where there is no mode to ask it of |
| `.goreleaser.yaml` | Five targets, the checksum, the Sigstore signature over it, the Homebrew cask |
| `.github/workflows/release.yml` | Tag push to published release, and the step that verifies its own signature |
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
  Production leaves it nil and gets the real terminal check. `App.ForceColor` is the same
  trick for stdout: colour detection type-asserts to `*os.File`, so a test handed a buffer
  can only ever get the no-colour answer, and the coloured path — including the column
  padding that has to discount invisible SGR bytes — would be checked by nothing.
  `--no-color` still wins over it.
- Golden files are regenerated with `-update`. Read the diff rather than accepting it:
  a golden file will happily record a misalignment bug as correct, and did once already.

## The contract suite

`internal/cli/contract_test.go` and `internal/cli/contract_reads_test.go`, behind
`//go:build contract`. They are the only tests here
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
- Owner reads are limited to **60/min per token** and the limiter is live here, because the
  test environment's cache store is a memory store. The M1 tests therefore read through the
  single-scope tokens rather than putting everything on `all` — three tokens is three
  budgets — and the cursor walk covers one state, not four, because the walk costs a
  request per share in the vault regardless of the filter. Adding tests that all read
  through `all` is how this suite starts failing on a fast machine.

It found one real bug on its first run: `whoami --probe` printed "unknown — rate limited"
for every scope and exited 0, reporting a sweep that determined nothing as a pass. That is
the same defect that had already been fixed for dead tokens, in the one case no fake could
reach.

M1 added `TestContractResponseFieldsAreAllDecoded`, which is the generalisation of the bug
that started that milestone: the schema publishes `response_fields` per endpoint, and the
test asserts the CLI has somewhere to put every key the server says it returns. It is
one-directional on purpose — modelling a key the schema does not advertise is fine, being
blind to one it does is not, and that is the direction drift travels. Writing it
immediately found that `api.Endpoint` had tagged that very field `response_keys`, so it had
been decoding to an empty slice since M0; nothing read it, so nothing was broken, but the
struct describing the server's self-description was itself written from memory.

## Not yet built

Milestones 0 through 5 are done. The transport, the stores, CI,
`version`, `schema`, `login`, `logout`, `whoami`, `profiles`; the reads (`fields list`,
`shares list`, `shares show`, with cursor pagination, `--all`, the schema-validated
filters, `--json`, and the rendering of §7.1–7.4); the vault writes (`fields create`,
`documents attach`); the mint (`shares mint`, with the expiry picker, the local ledger,
the §6.3 retry table, `--resume` and the handover block); the recipient side
(`open`, `open --document`); and distribution (five platforms, a signed checksum, a
Homebrew cask, shell completion, and the Windows credentials ACL).

Still to come: nothing on the plan's milestone list. The open items are the gaps in
§12 of `docs/cli-plan.md`, which are mostly the API's to close, and the deferred
keychain backend below.

Two things M1 found and left behind, both in the plan's §12:

- **`revoked_at` was not serialized** (gap 14, now closed). The deadline column has to show
  a closed share's closing fact, and revoking does not touch `expires_at` — so a revoked
  share keeps whatever future expiry it was minted with, and a client reading only
  `expires_at` reports weeks left on a dead link. It is now sent, and
  `TestContractEveryRevokedShareCarriesARevokedAt` asserts both halves.
- **`burn_after_read` was write-only** (gap 15, now closed). The mint endpoint accepted it
  and no read reported it, so neither `shares list` nor `shares show` could say that the
  live dossier in front of you dies on first read. Not worked around at the time, because
  there was nothing to work around with: a derived column cannot invent a fact the response
  does not carry. Not CLI-specific either — the web surfaced it on the mint form and
  nowhere else. M3 made it sharper rather than resolving it: `shares mint
  --burn-after-read` could *set* the flag, and the 201 did not echo it, so the CLI wrote a
  property it could never afterwards read back.

  `share_json` now sends `burn_after_read`, `allow_document_download` and
  `revoked_reason`. The third overturns a decision gap 14 made on purpose: it argued the
  reason could stay unserialized because the audit trail carries it. That holds on `show`,
  which returns the trail, and not at all on `list`, which does not — so distinguishing a
  burn from a holder's revoke across a page cost one request per row. It also matters more
  than "why" usually does, because `SharesHelper#share_restorable?` refuses a restore for a
  burn regardless of timing while a holder's revoke inside the grace period can still be
  brought back.

  Rendering: a `BURN` column on the list, carrying `yes` or the em dash rather than
  `yes`/`no`, so a page of ordinary shares stays quiet. It is *not* coloured — §7.2 allows
  colour on the state word and the countdown and nowhere else, and this is neither. The
  block shows all three, with `REASON` verbatim: `burned_after_read` is the server's
  vocabulary and a friendlier gloss here would be the second source of truth rule 9 keeps
  the audit trail's own `label`/`meaning` away from.

  **This does not help M4, contrary to what this file said while M3 was being built.** The
  three keys are on the *holder's* endpoints, under `shares:read`. The recipient surface is
  `POST /dossiers/:token/view` — one call for the whole dossier, with no gate request
  before it to carry a warning. So `open` still cannot warn before spending the one read it
  gets, and `open --document` still learns a share burns only by being refused. The plan's
  **gap 4** is the one that bears on that, it is still open, and it argues an
  un-authenticated metadata endpoint would be information for a token-guesser.

  The ordering this forced is worth remembering. Adding the keys to the schema's
  `response_fields` makes `TestContractResponseFieldsAreAllDecoded` fail until `api.Share`
  models them, and that test runs in *`dossier`'s* CI against `dossier-cli`'s `main`. So
  the CLI half has to merge first. The check is one-directional on purpose — the CLI may
  model keys the schema does not advertise — which is what makes that order work and the
  reverse order break.

### What M2 turned on

The two write endpoints take **no `Idempotency-Key`** — keys in this API are mint-only —
and that single fact shapes both commands more than anything in §7.

`Client.Do` already retried nothing but a 429, which is safe because the limiter refuses
before the handler runs. What M2 added is the part a transport cannot know: after an
ambiguous failure, `classifyWrite` tells the holder the request was *not* retried and
what to check. The asymmetry is worth keeping in mind — a retried `fields create` fails
loudly with the duplicate-label 422, but a retried `documents attach` **succeeds**, and
silently attaches a second copy, because that endpoint is purely additive and nothing
deduplicates it. It is the one place in this API where retrying is worse than giving up.
`TestWritesAreSentExactlyOnceWhenTheServerNeverAnswers` counts requests against a server
that never replies, for both.

Two smaller things the endpoints dictate:

- **The multipart part declares its own content type.** `multipart.CreateFormFile` writes
  `application/octet-stream`, and the API allow-lists on the *declared* type rather than
  sniffing bytes — so using the stdlib helper would make every upload a guaranteed 415.
  `internal/api/writes.go` builds the part by hand for that one reason.
- **The extension map is not the allow-list.** The schema does not publish the accepted
  types (the plan's gap 5), so the CLI cannot pre-check them and does not try; the map in
  `attach.go` only guesses a declaration from an extension, an unknown extension is a
  refusal that points at `--content-type`, and the server decides. The size cap *is*
  published, so that one is checked locally and the refusal quotes both numbers.

M2 also found two things in the repositories rather than the API:

- **`TestContractDefaultFieldListingIsActiveOnly` counted rows against the fixture**, so
  it broke the moment anything in the suite created a field. It passed in CI only because
  Go runs files alphabetically and `contract_writes_test.go` sorts last — an order
  dependency nothing declared. It now asserts its actual claim (no non-active row comes
  back unfiltered, and every seeded field is reachable under its own status), by id, which
  is immune to a vault that has grown.
- **Cobra reads the first backquoted word in a flag's usage string as the argument
  placeholder.** `"(see \`dossier schema\`)"` rendered as `--status dossier schema`
  instead of `--status string`. Shipped in M1 and unnoticed; flag usage strings now carry
  no backticks.

### What M3 turned on

The mint is the only endpoint in this API that takes an `Idempotency-Key`, and it
**requires** one. Everything structural about `shares mint` follows from that, plus one
consequence of it that is easy to miss.

**The body is built once.** `api.BuildMintBody` returns a byte slice, and that slice is
what the ledger stores and what every attempt sends. The API's identity for a request is
the key plus the *raw body*, so a retry that re-marshalled would risk a different byte
sequence and earn `409 idempotency_key_reused` — which is permanent for that key.
`encoding/json` is stable for a fixed struct and that is not the point: building once
makes the client's digest and the server's agree by construction rather than by luck.
`field_ids` are sent in the order given, unsorted and undeduped, for the same reason.

**The ledger is written before the request, not after.** Its entire value is in the window
between "sent" and "answered", and an entry created from a response would be missing for
exactly the failures it exists to recover from. It lives under `StateDir`, never
`CacheDir`, because something will eventually clear a cache and an unresolved mint must
outlive that. It holds the key, the bytes, a SHA-256 of them, the host, and one more
field:

**`server_error_at` is what makes §6.3 decidable.** A `409 request_in_flight` means "wait
and retry" normally, and means "the batch may have been written while its response could
not be stored" after a 5xx. Same status, same body; the only thing that tells them apart
is whether a 5xx was recorded under that key. So it is written to disk *before* the retry
whose answer it interprets. Without it the CLI would back off 1s/2s/4s against a claim
that can never resolve, then eventually mint a second dossier.

**The mint owns its own 429 handling.** `mintClient` forces `WithNoWait(true)` at the
transport level regardless of the `--no-wait` flag, so the 429 surfaces as an error rather
than being slept on inside `Client.Do`. That is not a duplicate of the flag: §6.3 gives
the mint its own bounded patience (three waits, capped at a minute each — sized for this
endpoint's 10/min budget) and its own prose while it waits, and a transport that had
already slept would make both unreachable.

#### net/http replays a keyed POST, and only a keyed one

Found while writing `TestMintResumeCompletesAnUnknownOutcome`, which kept passing with
exit 0 when it should have failed. `Request.isReplayable` treats a POST as safe to resend
on a *reused* idle connection that died before any response **if and only if** it carries
an `Idempotency-Key` or `X-Idempotency-Key` header — the same signal, for the same reason,
this API uses. The consequences are worth knowing rather than working around:

- `shares mint` may be replayed under the hood. That replay carries the same key and the
  same bytes, so it is exactly the retry §6.3 prescribes and cannot mint twice.
- `fields create` and `documents attach` send no such header, so they are **never**
  replayed. M2's exactly-once claim for two endpoints with no idempotency guard is
  therefore structural, not a hope about connection reuse.

`TestNetHTTPReplaysOnlyTheKeyedPost` pins both halves, because if a future Go relaxed the
rule the second bullet would silently become false and a retried `fields create` would be
a second field. It also means no test here can assert a request *count* during a dropped
connection; the resume test flips a flag instead.

#### The mint's contract tests, and the one done-when Go cannot reach

The plan's M3 done-when ends "the PIN in the block matches the emailed one in the test
mailer". Go cannot see Rails' mailer — another process's memory — and
`spec/services/shares/mint_spec.rb` already asserts the emailed PIN is the minted one,
where the mailer is visible. So `TestContractTheRenderedPINOpensTheDossier` asserts
something stronger and within reach: the PIN **parsed back out of the rendered block**
opens the dossier through `POST /api/v1/dossiers/:token/view`, and a rotated one does not.
That closes the gap that matters — a correct PIN from the API is worth nothing if the
renderer eats the space in `480 217`, and a holder reading a mangled PIN has no way to
get another.

The mint budget is **10/min per token**, the tightest in the API. Two seeded tokens hold
`shares:mint` (`shares_mint` and `all`), and `contract_mint_test.go` splits across both to
stay inside it — currently six requests each. That headroom is thin: a new mint test
should pick the token with fewer, and `TestContractTheMintBudgetIsPublished` fails if the
server's number ever changes out from under this arithmetic.

#### Three smaller things

- **The picker measures the exception's label with the presets.** "No expiry" is longer
  than any of "24 hours", "7 days", "30 days", "90 days", and it sits in the same column —
  so a width taken from the presets alone pushed the last row's note out of line.
- **The handover's closing sentence is not hard-wrapped.** It embeds a recipient name, and
  a name's width is not knowable, so a wrap that looks right for "Marisol Vega" is wrong
  for a longer one. The terminal is better at this than a literal newline.
- **The cobra backtick bug was still live in two flags.** M2 swept `fields` and `shares`
  and believed it done; `login --name` and the root `--profile` still rendered as
  `--name dossier profiles list` and `--profile default`. Backticks around a command name
  are the house style in every *other* string in this CLI, which is why this keeps
  happening — so `TestNoFlagUsageStringContainsABacktick` now walks the whole command tree
  and fails on any of them.

### What M4 turned on

`open` is the first command with no credential of its own, and the first where a *retry*
is the dangerous operation rather than the safe one. Both facts changed the transport.

**`api.Request.Once`.** A new per-request flag that forbids a second send for any reason,
including the `429` this client retries everywhere else. It sits on the request rather
than on the client because it is a property of the endpoint: `POST .../view` may have
been the one open of a burn-after-read dossier, and a rate limit the CLI cannot
distinguish from a late arrival is not grounds to find out. Expressed as client config it
would have been one `api.New` call away from being lost.

**`Once` cannot cover net/http's own replay, and does not have to.** M3 established that
Go replays a POST on a reused idle connection only when it carries an idempotency header.
`view` carries none, and `open` issues exactly one request per process so its connection
is always fresh — two independent reasons the replay cannot fire.
`TestViewCarriesNothingThatWouldMakeItReplayable` pins the first; every other test in
`open_test.go` counts requests and so pins the second.

**`--document` sends no `view` at all.** This is the sharp end of §8.2 and the easiest
thing to get wrong by being helpful: a document id can only have come from an earlier
open, so "look up the dossier to resolve the id" would spend a burn share in order to
fetch a file from it. The command goes straight to the document endpoint. The contract
test asserts the open count is unchanged afterwards, which is the only way to show it.

**`FetchSigned` is deliberately not `Client.Do`.** `Do` refuses any path outside
`/api/v1`, and that refusal is what makes "no web path" structural rather than careful. A
signed URL is outside it by construction — Tigris in production, `/rails/active_storage/`
under the test Disk service. Relaxing the check for every caller to admit the one request
the CLI does not originate would trade the invariant for a convenience, so the download
runs on a plain `http.Client` and the invariant is restated precisely: *every request the
CLI composes is under `/api/v1`*. It sends no credential, because the capability is the
URL and the host is not ours. `TestOpenDownloadsThroughASeparateTransport` serves the blob
from a second `httptest` server, so `fakeAPI`'s "nothing outside /api/v1" assertion stays
armed and the exception is demonstrated rather than excused.

#### A single-use fixture needs a single owner

The burn share can be opened once per seed. Written as two tests — one for the document
refusal, one for the open — the second to run found a share the first had spent, and
*which* failed depended on source order. They are now one test, `TestContractTheBurnDossier`,
sequenced the way the product defines: refuse a document against a live share (proving it
costs nothing), open it, then watch the second open be refused.

That is M2's order-dependence learning a second time, and it also surfaced a latent
instance of it. `TestContractAuditTrailCarriesRule9sPair` took *the first revoked share in
the list*, which was fine until M4 created a second one by burning it — and a burn's trail
carries `burned`, not `revoked`, so the test failed claiming a product rule had regressed.
It now selects the fixture's revoked share by token. A contract test that says "the first
row" is asserting something about ordering it does not mean to.

#### A test helper that builds JSON by concatenation will eventually lie

`errorEnvelope` interpolated its message and hint into a JSON string. The API's real hints
contain double quotes — `Check the "Retry-After" header (seconds) and back off.` is one —
so pasting a real hint in produced invalid JSON, the client could not decode the envelope,
and the error arrived with an empty code that mapped to exit 1. The failure read as a
broken exit-code mapping and was a broken fixture. It marshals now.

#### Gap 17: the recipient boundary serves the holder API's `not_found` copy

A `404` here renders a hint reading "Check the id and that it belongs to this token's
account" to a caller who has neither a token nor an account. Rendered verbatim anyway, for
the reason every other verbatim rule holds: a client that rewrites the server's copy
becomes a second source of truth for it. The CLI adds its §8.3 line *beside* the hint, not
instead of it, and `TestContractAnUnknownTokenStillCarriesTheHolderAPIsHint` logs a note
the day the API fixes it.

### What M5 turned on

Distribution: `.goreleaser.yaml`, `.github/workflows/release.yml`, a `completion` command
that is more careful than it looks, and the Windows credentials ACL.

**The release verifies its own signature.** The workflow signs `checksums.txt` with cosign
keyless, and then, in the next step, runs the exact `cosign verify-blob` command the
release notes tell a user to run. A signing step that silently produces something
unverifiable is worse than no signing step: it puts a claim in the release notes that
nobody checks until the day somebody does. Signing configuration is also the most
volatile part of a release pipeline — cosign v3 replaced `--output-signature` and
`--output-certificate` with a single `--bundle`, rewriting these very flags — and this
turns that from a user's discovery into a red build.

**`goreleaser check` runs on every push, not at tag time.** GoReleaser promotes soft
deprecations to hard errors on its own schedule; `brews` went from a warning to a build
failure in one minor version, and the repositories that found out at tag time had already
pushed the tag. CI also runs a full snapshot build, which is what proves all five targets
still cross-compile, and asserts the version stamp reached the binary — a broken `ldflags`
stanza produces a perfectly working release in which every binary reports `dev`.

**Casks, not formulas.** `brews` is a hard error as of GoReleaser v2.16. The generated
cask covers Linux as well as macOS, so nothing was lost in the move, and it installs the
completions the archive already ships.

#### Completion may not make a request

Every completion source is local: the command tree from the binary, profile names from
the credentials file, filter vocabularies from the *cached* schema with no fetch on a
miss. This is not fussiness about latency. Completion fires on a keystroke, and against
this API a request spends a published rate-limit budget, writes a row in the holder's
audit trail, and on the recipient boundary can consume a burn-after-read dossier. A shell
that did any of that while someone was still deciding what to type would be indefensible,
and the failure would be invisible — the completions would look perfectly normal.

`TestCompletionNeverTouchesTheNetwork` walks the whole tree with a server wired up that
fails the test if it is called, and with a *working* profile seeded, because an
unauthenticated CLI making no requests proves nothing.

Staleness is tolerated here where the commands refuse it. A completion list is a
suggestion the server validates a moment later, so a retired status costs one clear error
message; refusing to complete because the cache turned 24 hours old costs the feature.

#### The Windows mode is a fiction, and the Unix check believed it

`os.Stat` on Windows synthesises a mode from the read-only attribute: an ordinary private
file reports `0666`. The credentials check was `mode&0o077 != 0`, so it would have refused
every credentials file on every Windows machine — including the one `login` had just
written. The CLI would have broken on its own output, and cross-compiling proved nothing,
because it type-checks perfectly.

So the check is now a platform pair. Unix reads the mode. Windows reads the DACL and asks
the same question — can any account other than this one read the token — and `PermissionError`
carries the finding and the remedy rather than formatting one, because `chmod 600` is
advice a Windows holder cannot follow.

SYSTEM and Administrators are permitted. Refusing them would be theatre: Windows will not
let a file exist that an administrator cannot reach, so the check would reject every
possible file and teach the holder to ignore it. `0600` does not keep a secret from root
either.

Two things fell out of it. `RestrictToOwner` is exported and the document download uses it
too — a saved passport scan is no more public than a token, and `temp.Chmod(0600)` there
was doing nothing on Windows. And a directory needs `0700`, not `0600`: the first version
applied the file mode to the config directory, and the whole store package lost the
ability to write a temp file.

#### A vetted platform is not a tested one

`GOOS=windows go vet` would pass a DACL check that refuses every file, and one that
accepts every file. Every mistake this code can make is a runtime one about SIDs and
access masks. There is now a `windows-latest` job running the store package, where the
platform code and its own tests live, and `TestRestrictToOwnerSatisfiesTheCheck` closes
the loop the cross-compile could not: lock a file down, then confirm the checker agrees.

#### Deferred: the keychain backend

§10 lists an optional OS-keychain backend behind `credentials.backend = "keychain"`. Not
built, and the reason is in §11 Q6 already: the file is auditable, portable to a cron
host, and has no daemon dependency, while keychains differ per OS and fail *silently* over
SSH — which is where a CLI like this one most often runs. It is an upgrade to the
baseline, not a correction of it, and nothing in the product asks for it yet.
