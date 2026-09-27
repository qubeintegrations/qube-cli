# qube — the QuBe Sync command line

Log in once from a terminal; then get your app's keys, create connections (simulated ones too),
watch requests, drive the simulator and push workflows — from a shell, a script, or an agent.

```bash
qube login                          # opens the dashboard; confirm the code; sandbox apps by default
qube apps                           # what this session may act on
qube use "My App Dev"               # the default app for the commands below
qube env --write .env               # QUBE_URL / QUBE_API_KEY / QUBE_WEBHOOK_SECRET, never printed
qube connections create --simulated --name "Local dev"
qube qb customers list <connection> --max-returned 5 --webhook-url https://your-app.example/qb
qube qb invoices create <connection> --data @invoice.json
qube requests list <connection> --state error
qube requests discard <connection> <id>
qube simulator faults <connection> --next 3100
qube workflows install create_customer_safely --publish
qube workflows push ./chart.json --publish
qube api GET /connections           # any v2 path, with the app's key
```

`qube help` lists every command. Every command takes `--json` (machine output), `--host`, `--app`,
`--timeout` and `--yes`, anywhere on the line. Every command talks to the v2 API.

## QuickBooks operations: `qube qb`

`qube qb` runs any of the QuickBooks operations the v2 API offers (about 260: list, create, update,
delete, void, merge...). They aren't written into the CLI: it reads them from the host's
`/api/v2/openapi.json`, so it always offers what that host offers, flags and body fields included.

```bash
qube qb                                          # every resource and its verbs
qube qb customers                                # one resource's operations
qube qb customers list --help                    # its flags; --help --json prints the full schema
qube qb customers list <connection> --name-starts-with North --max-returned 5
qube qb customers create <connection> --name "Northwind" --bill-address '{"city": "Austin"}'
qube qb txn-void execute <connection> --txn-id 1A2B-3C --txn-void-type Invoice
```

- Query parameters and the body's top-level fields are flags (`max_returned` is `--max-returned`). A boolean
  alone means true (`--iterator`). An object takes JSON, and a list takes the flag once per value or a
  JSON array. Any of these also takes `@file`.
- `--data JSON|@file|-` sends a whole body. Field flags are merged over it.
- Each operation queues a request and prints its id. QuickBooks answers when the connection's Web
  Connector next runs. Pass `--webhook-url` to have the answer sent to you, or look it up with
  `qube requests show <connection> <id>`.
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
in the dashboard as yourself and choose what the session may reach (sandbox apps by default), and the
terminal receives the token directly. That is also what makes the CLI safe to hand to an AI agent: it
can run `qube env --write .env` and `qube connections create --simulated` without ever seeing a
secret, and you can revoke the session with `qube logout`, `qube sessions revoke <id>`, or from
**CLI sessions** in the dashboard. Sessions expire after 30 days.

An app's API key is only ever sent to the host that issued it, and only on `/api/v1` and `/api/v2`
paths; the session token only to `/api/cli`. The OpenAPI document `qube qb` reads is public and is
fetched without either.

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
vets, tests and builds on Linux, macOS and Windows. A release is a tag: `git tag v0.1.0 && git push
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
