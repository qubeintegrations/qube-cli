# qube — the QuBe Sync command line

Log in once from a terminal; then get your app's keys, create connections (simulated ones too),
watch requests, drive the simulator and push workflows — from a shell, a script, or an agent.

```bash
qube login                          # opens the dashboard; confirm the code; sandbox apps by default
qube apps                           # what this session may act on
qube use "My App Dev"               # the default app for the commands below
qube env --write .env               # QUBE_URL / QUBE_API_KEY / QUBE_WEBHOOK_SECRET, never printed
qube connections create --simulated --name "Local dev" --use   # --use: the default connection
qube qb customers list <connection> --max-returned 5 --wait   # prints QuickBooks' answer
qube qb invoices create <connection> --data @invoice.json
qube requests list <connection> --state error
qube requests discard <connection> <id>
qube simulator faults <connection> --next 3100
qube simulator faults <connection> --next lost   # applied, then never answered: ends timed_out (--next ok clears)
qube workflows install create_customer_safely --publish
qube workflows push ./chart.json --publish
qube api GET /connections           # any v2 path, as the current app
```

`qube help` lists every command. Every command takes `--json` (machine output), `--host`, `--app`,
`--connection`, `--timeout` and `--yes`, anywhere on the line. Every command talks to the v2 API.

## A default connection

Commands that act on a connection take it as their first argument, and may leave it out once one is
chosen: `qube use --connection <id|name>` remembers one for the current app (`none` forgets it), and
`--connection <id>` picks one for a single command. `qube connections create --use` makes the new
connection the default, `qube connections list` marks it with `*`, and `qube status` shows it.
`qube connections delete` always needs the connection named.

```bash
qube use --connection "Local dev"
qube qb customers list --max-returned 5 --wait
qube requests list --state error
qube requests show <request-id>
```

## QuickBooks operations: `qube qb`

`qube qb` runs any of the QuickBooks operations the v2 API offers (about 260: list, create, update,
delete, void, merge...). They aren't written into the CLI: it reads them from the host's
`/api/v2/openapi.json`, so it always offers what that host offers, flags and body fields included.

```bash
qube qb                                          # every resource and its verbs
qube qb customers                                # one resource's operations
qube qb customers list --help                    # its flags; --help --json prints the full schema
qube qb invoices create --example > invoice.json # the API's example body, to edit
qube qb invoices create <connection> --data @invoice.json
qube qb customers list <connection> --name-starts-with North --max-returned 5
qube qb customers create <connection> --name "Northwind" --bill-address '{"city": "Austin"}'
qube qb txn-void execute <connection> --txn-id 1A2B-3C --txn-void-type Invoice
qube qb items list <connection> --iterator --max-returned 100 --wait   # every page
```

- Query parameters and the body's top-level fields are flags (`max_returned` is `--max-returned`), the
  fields of each alternative of a `oneOf` body included (`--txn-data-ext-type SalesOrder --txn-id ...`
  on `data-exts update`). A boolean alone means true (`--iterator`). An object takes JSON, and a list
  takes the flag once per value or a JSON array. Any of these also takes `@file`.
- `--data JSON|@file|-` sends a whole body. Field flags are merged over it.
- `--help` is built from the spec too. Descriptions are rendered from their Markdown (links print as
  `text (url)`), and the flags show the rules they are part of, read from the body's JSON Schema. The
  flags that stand alone come first; then each choice (a `oneOf`, or fields that exclude each other)
  is a block, `Exactly one of:` or `At most one of:`, listing each alternative's flags with `or:`
  between them, so `data-exts update` shows the list-object, transaction and other shapes side by
  side. Other rules sit beside the flags they bind: which lists combine (`(can be combined with
  --journal-credit-line)`), a choice inside an alternative (a query's name filters), and an
  object's keys with the required ones named (`at most one of: rate | rate_percent`). Query
  parameters can't carry such a rule in OpenAPI, so for them it is read from the sentence the
  API's generator writes in each description ("Choose at most one of: ..."), as it is for a list
  that combines only optionally. Help ends with an example: the API's own example body for the
  operation, as a `--data` command (shortened when it is long), or a query's required parameters.
- `--example` prints that example body as indented JSON, to save, edit and send with `--data @file`.
- Shell completion (`qube completion bash|zsh|fish`) completes resources, verbs, an operation's flags
  and the values of a flag that lists them (`--active-status <TAB>`), all from the cached list, so a
  TAB never waits on the network.
- Each operation queues a request and prints its id. QuickBooks answers when the connection's Web
  Connector next runs. `--wait` waits here and prints the answer: every page of an iterated
  query, with the exit status 1 if it failed. It waits up to 10 minutes; `--wait=30m` sets another
  limit, and Ctrl-C stops waiting but leaves the request queued. Otherwise pass `--webhook-url` to have
  the answer sent to you, or look it up later with `qube requests show <connection> <id>`.
  (`--wait` polls, which suits a terminal or a script; an integration uses `webhook_url`.)
- QuickBooks keeps an iteration only for the Web Connector session that fetched its first page. If that
  session ends partway (QuickBooks is closed, the Web Connector is stopped), the next page fails as
  `error` (`quickbooks_connection_error`) and `--wait` prints the pages it got and exits 1. Queue the
  query again to get the rest; it starts over from the first page.
- `qube workflows run ... --wait` and `qube workflows decide ... --wait` wait the same way, until the run
  ends (exit 1 if it failed or was cancelled) or needs a decision.
- The list is cached per host in `$XDG_CACHE_HOME/qube/` (`~/Library/Caches/qube/` on macOS;
  `QUBE_CACHE_DIR` overrides) and read again after a day, when it names something the list doesn't have,
  or on `qube qb --refresh`. If the host can't be reached, an older list is used, with a warning.

## Production apps

In a production app, every request that changes something asks first, naming the app: every
`qube qb` operation except `list` (each one writes to the customer's real company file), creating or
changing a connection (its password and onboarding link too), discarding a request, driving the
simulator, every workflow change, run, answer or cancellation, and `qube api` with any method but `GET`.
Reads never ask, and neither do `workflows validate` and `connections qwc`, which change nothing.
Removing something (a connection, a workflow, a run) asks in any app. A script answers with `--yes`.
Without a terminal, or under `--json`, a request that would ask is refused until `--yes` is given, so
nothing reaches a production company file by accident. Sessions reach production apps only when you
choose **every app** at `qube login`.

## Install

Binaries for macOS, Linux and Windows (amd64 and arm64) are attached to each
[release](https://github.com/qubeintegrations/qube-cli/releases): download the archive for your
platform, unpack it and put `qube` on your `PATH`. With a Go toolchain (1.16 or newer):

```bash
go install github.com/qubeintegrations/qube-cli/cmd/qube@latest
```

Or with Homebrew on macOS:

```bash
brew install qubeintegrations/tap/qube
```

`qube version` prints the release you have. Shell completion:

```bash
eval "$(qube completion bash)"        # or zsh / fish; see `qube completion --help`
```

## Why a device-code login

The token a session holds is never typed, pasted or printed: `qube login` shows a code, you confirm it
in the dashboard as yourself and choose what the session may reach (sandbox apps by default) and
whether it may change anything (read-only or read and write), and the terminal receives the token
directly. That is also what makes the CLI safe to hand to an AI agent: it
can run `qube env --write .env` and `qube connections create --simulated` without ever seeing a
secret, and you can revoke the session with `qube logout`, `qube sessions revoke <id>`, or from
**CLI sessions** in the dashboard. Sessions expire after 30 days.

The CLI calls the API with the session token, naming the app it acts as, so revoking a session cuts
it off at once, and the server holds a read-only session to reads. The token goes only to the host
that issued it. The app's API key is read only for `qube env`, and only by a read-and-write session.
The OpenAPI document `qube qb` reads is public and is fetched without the token.

### Read-only sessions

`qube login --read-only` asks for a session that can look but not change anything; whoever approves
it in the dashboard sees the request preselected and has the last word. A read-only session can list
and show everything its reach allows, run QuickBooks queries (`qube qb ... list`), validate charts
and download a connection's QWC file. It can't create, change, delete, discard, run or answer
anything, and it can't read an app's API key or webhook secret, so `qube env` isn't available to it.
The server enforces this; the CLI also refuses such a command before sending it. `qube status` and
`qube sessions` show each session's access.

## Configuration

| What | Where |
| --- | --- |
| Sessions (one per host), default host and default app | `$XDG_CONFIG_HOME/qube/credentials.json` (`~/Library/Application Support/qube/` on macOS, `%AppData%\qube\` on Windows), mode 0600, written atomically. `QUBE_CONFIG` overrides the path. |
| Host | `--host`, else `QUBE_HOST`, else the host you last logged in to (`qube use --host H` changes it), else `https://qubesync.com`. |
| HTTP timeout | `--timeout 30` (seconds) or `2m`; `QUBE_TIMEOUT`. Default 60 s. Ctrl-C cancels any command (exit 130). |
| Browser | `QUBE_NO_BROWSER=1` keeps `qube login` from opening one (the URL is always printed). |
| `qube qb` operation list | `$XDG_CACHE_HOME/qube/openapi-v2-<host>.json` (`~/Library/Caches/qube/` on macOS, `%LocalAppData%\qube\` on Windows). `QUBE_CACHE_DIR` overrides the directory. Holds nothing secret. |

Exit codes: 0 ok, 1 the command failed, 2 the command line was wrong, 130 interrupted.
Under `--json`, lists print the server's data (`requests list` prints `{"data", "meta"}`), detail
views (`requests show`, `workflows runs <id>`) always print JSON, and notes go to stderr.

## Develop

```bash
go build -o qube ./cmd/qube && ./qube --host dev.qubesync.com login
go vet ./... && gofmt -l . && go test ./...
```

Stdlib only; Go 1.16 is the floor so it builds on old CI images. CI (`.github/workflows/ci.yml`)
vets, tests and builds on Linux, macOS and Windows.

### Checking `qube qb` against a host's spec

`internal/ops/spec_check_test.go` renders every operation's help and holds it to the spec: no
Markdown left raw (no backtick, `](` or `###`), no URL cut or broken across lines, every request
example binding through `--data` to the same JSON body (and `--example` printing it), and every field
of a choice showing its rule. In every `go test` (and so in CI, which has no spec) it runs over
`internal/ops/testdata/qbxml_subset.json`, a few operations extracted from the qube app's generated
spec, and `testdata/help.golden` holds the help they render. Neither is edited by hand: with the
qube checkout beside this one (or `QUBE_REPO` naming it), `go test ./internal/ops` fails when the
subset is out of date, and `go test ./internal/ops -update` rewrites both. To check a whole spec, the
one a host serves:

```bash
curl -sf https://qubesync.com/api/v2/openapi.json -o /tmp/v2.json   # or your --host
QUBE_SPEC=/tmp/v2.json go test ./internal/ops -run TestParseServedSpec -v
``` A release is a tag: `git tag v0.1.0 && git push
--tags` runs [goreleaser](.goreleaser.yaml) from `.github/workflows/release.yml`, which builds the
six binaries, writes `checksums.txt` and publishes the GitHub release.

### Homebrew

The release job also writes the cask `Casks/qube.rb` into the public
[`qubeintegrations/homebrew-tap`](https://github.com/qubeintegrations/homebrew-tap) repository when the
`HOMEBREW_TAP_GITHUB_TOKEN` secret is set on this repository (a fine-grained token with *Contents:
read and write* on the tap repository only). Without the secret the cask is only generated under
`dist/` and the release still succeeds. It is a cask rather than a formula because that is how
goreleaser now ships prebuilt binaries; Homebrew resolves `qubeintegrations/tap/qube` either way. Users install with `brew install qubeintegrations/tap/qube`
and upgrade with `brew upgrade qube`.

The server side of the login (`/api/cli/*`, the **CLI sessions** pages) lives in the QuBe Sync
application; this repository is only the client.

## License

MIT, see [LICENSE](LICENSE).
