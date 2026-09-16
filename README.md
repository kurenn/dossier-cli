# dossier

A command-line client for a [Dossier](https://dossier.global) vault's API.

Dossier is a personal document vault where every share has an expiry built in. This is
the terminal client: read your identity fields, mint a share with a deadline, watch what
you have released, and open a share as its recipient — from a shell rather than a
browser.

```console
$ dossier shares list
ID  TOKEN      TITLE                 RECIPIENT       STATE     DEADLINE      OPENS  FIELDS
88  K7M2P9QRX  Lease application     Marisol Vega    LIVE      5d 15h left   0      1
87  Q2WXM9KRP  Bank KYC              Tomás Herrera   EXPIRING  1h 07m left   2      3
81  R7KPX2MQ9  Visa appointment      —               EXPIRED   2026-09-01    1      2
```

## Status

**Milestone 0.** The foundations are in place and usable: `version`, `schema`, `login`,
`logout`, `whoami` and `profiles`. The commands that read and write vault data —
`fields`, `documents`, `shares` and `open` — are not built yet, so the listing above is
what the client is being built toward rather than what it does today.

## Install

```bash
go install github.com/kurenn/dossier-cli/cmd/dossier@latest
```

Or build from a checkout:

```bash
git clone https://github.com/kurenn/dossier-cli
cd dossier-cli
go build -o dossier ./cmd/dossier
```

Requires Go 1.26.8 or later — earlier 1.26 patches carry known `crypto/tls` and
`net/http` vulnerabilities on the path this client sends your token over. The result is a single static binary with no runtime.

## Getting started

`dossier` cannot create an API token, and it will tell you so the first time you run
`login`. The API has no token-management surface at all — no endpoint mints, lists,
rotates or revokes one. A token comes from your vault's **Settings → API tokens**, behind
a passkey assertion, and it is shown exactly once.

Once you have one:

```bash
dossier login                        # paste it at the hidden prompt
dossier whoami                       # check which credential is in use
dossier whoami --probe               # find out which scopes it really has
```

In a script or a cron job, pipe it instead — never pass it as an argument, which `dossier`
refuses, because a token on the command line is written to your shell history and visible
in `ps` to every other user on the machine:

```bash
printf %s "$TOKEN" | dossier login --token-stdin
```

Or skip the file entirely and set `DOSSIER_TOKEN` in the environment.

Before you have a token at all, `dossier schema` works: it prints the API's own
description of itself, which is also where this client reads every limit, scope and
filter value it uses.

## How it holds your token

- `~/.config/dossier/credentials.toml`, mode `0600`, in a `0700` directory, written
  through a temp file and a rename so a crash never leaves half a token on disk.
- **It refuses to run if that file is readable by anyone else**, and tells you the
  `chmod` to fix it — the same posture `ssh` takes with a private key, for the same
  reason.
- `~/.config/dossier/config.toml` holds your default profile. Nothing secret is in it.
- `logout` forgets a token. It does **not** revoke it, cannot, and says so: only Settings
  can revoke.

A profile is one (host, token) pair. Two vaults, or two tokens for one vault — a
read-only one for scripts and a mint-capable one you use by hand — are two profiles.

## Exit codes

Every command ends on one of these, so a script can branch on `$?` without parsing prose.

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | Unexpected: transport failure, unparseable response, a bug |
| 2 | Usage: refused before anything was sent |
| 3 | The token does not authenticate |
| 4 | The token lacks the scope this command needs |
| 5 | Not found, or not yours |
| 6 | The request was refused as invalid |
| 7 | Idempotency conflict that retrying cannot fix |
| 8 | Try later: rate limited, or a request already in flight |
| 9 | The share is closed — expired or revoked |
| 10 | The PIN was refused, or is locked |
| 11 | **A mint may have landed and cannot be confirmed** — reconcile before acting |

Code 11 is the one worth reading twice. It means a mint was sent, the server failed
mid-flight, the retry could not establish whether it landed, and *doing nothing* is safer
than trying again. Check `dossier shares list` before acting.

## Output

Human-readable by default, with data in labelled blocks and aligned tables that survive
`grep`, `awk` and `sort`. `--json` passes the API's response body through byte-for-byte —
no reshaping, no wrapper — so `jq` does the rest.

Colour appears only on a share's state word and its countdown, only when stdout is a
terminal, and never when `NO_COLOR` is set or `TERM=dumb`. Every state reads correctly
with colour stripped, because a pipe strips it.

## What it will not do

- **Speak to anything but `/api/v1`.** No web routes, no cookies, no scraped HTML. That
  restriction is enforced in the transport, not left to discipline.
- **Take a token as an argument.** Hidden prompt or stdin, nothing else.
- **Decide anything the server decides.** Whether a share is live, expired or revoked, and
  whether a token is valid, are the server's answers; this client renders them and never
  computes its own.
- **Mint, rotate or revoke a token.** It has no way to, and it is honest about that
  rather than implying otherwise.
- **Default an expiry.** Every mint states a deadline or explicitly states that it has
  none.

## Contributing

```bash
go test ./...                  # the suite, with a coverage floor of 85%
gofmt -l . && go vet ./...     # formatting and vet
go test ./internal/cli -update # rewrite the golden output files
```

See `CLAUDE.md` for the full development loop.

## Licence

MIT. See [LICENSE](LICENSE).
