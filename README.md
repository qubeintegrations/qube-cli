# qube — the QuBe Sync command line

Log in once from a terminal; then get your app's keys, create connections (simulated ones too),
watch requests, receive webhooks on your machine, drive the simulator and push workflows — from a
shell, a script, or an agent.

```bash
qube login                          # opens the dashboard; confirm the code; sandbox apps by default
qube apps                           # what this session may act on
qube use "My App Dev"               # the default app for the commands below
qube env --write .env               # QUBE_URL / QUBE_API_KEY / QUBE_WEBHOOK_SECRET, never printed
qube connections create --simulated --name "Local dev" --use   # --use: the default connection
qube connections create --name "Acme" --webhook-url https://myapp.example/webhooks/qube/connection/42   # its events go there: connected, setup_cancelled, stalled, resumed
qube connections update <connection> --webhook-url ""   # "" stops them
qube qb customers list <connection> --max-returned 5 --wait   # prints QuickBooks' answer
qube qb invoices create <connection> --data @invoice.json
qube match <connection> customer "ACME, Inc." --email ap@acme.example   # the records a name could mean
qube requests list <connection> --state error
qube requests discard <connection> <id>
qube listen --forward-to http://localhost:3000/webhooks/qube --write-env .env   # webhooks reach your local app
qube simulator faults <connection> --next 3100
qube simulator faults <connection> --next lost   # applied, then never answered: ends timed_out (--next ok clears)
qube workflows templates add_transaction --usage             # what the template asks and ends in, before you install it
qube workflows install add_transaction --publish --replace   # safe to repeat; installs the resolve and ensure it calls too
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

## Matching names: `qube match`

`qube match` asks which records in a connection's company file a name could mean, from QuBe Sync's
index of the file's names (realtime and simulated connections only). It answers at once: `exact` (the
one record with that full name), `strong` (likely, not certain: `Acme Inc` and `Acme LLC` are strong
for each other), `candidates` (best first) or `none`, with each record's `list_id`.

```bash
qube match <connection> customer "ACME, Inc." --email ap@acme.example
qube match <connection> customer Kitchen --parent Smith          # a job under a customer
qube match <connection> entity "Riverbend Electric"              # customers, vendors, employees and other names
qube match <connection> item Consulting --exact-only             # exact or none, never a fuzzy match
```

A customer, vendor, employee or other name can be matched on more than its name: `--account-number`,
`--email`, `--phone` and `--postal-code` can make a strong match, and `--city` and `--company-name`
rank candidates. `qube match -h` lists the kinds and flags, and `--json` prints the API's answer. The
first match on a connection starts building its index and is refused until it's in (exit 1). To have
it ready ahead of time, `qube connections name-index <connection> --webhook-url U` starts the build
and has `name_index.ready` sent to U when it's in. A simulated connection builds it as
`qube simulator sync` runs sessions, and `qube connections show` shows its state.

## Receiving webhooks on your machine: `qube listen`

```bash
qube listen --forward-to http://localhost:3000/webhooks/qube --write-env .env
```

`qube listen` gives this machine a QuBe Sync webhook URL for the app
(`https://qubesync.com/relay/rl_...`) and forwards every delivery to it to the `--forward-to` URL,
with the same body and headers. `X-QUBE-Signature` arrives unchanged, so your app checks it with
`QUBE_WEBHOOK_SECRET` (from `qube env`) exactly as it will in production. There's no tunnel to
install, and nothing on your machine is reachable from the internet.

```
Webhook URL for Acme Sandbox (the same every time on this machine):
  https://qubesync.com/relay/rl_4f7k2m9q8t3w6x1z5c0v7b2n4h
Forwarding to http://localhost:3000/webhooks/qube. Ctrl-C to stop.
14:02:11  request 1f0c2a3b…  response_received  → POST /webhooks/qube  200  41 ms
14:02:19  workflow_run.notification  customers_page #3  → POST /webhooks/qube  200  58 ms
14:03:02  request 7d41e9a0…  response_received  → POST /webhooks/qube  500  12 ms  (QuBe will retry)
```

- Pass the URL as `webhook_url`: request answers, a workflow run's webhooks and notifications, and
  name index events all come through it. `--write-env .env` keeps it in `.env` as
  `QUBE_WEBHOOK_URL` and changes nothing else in the file. Without the flag, `qube listen` prints
  the line to add when `./.env` doesn't have it.
- The URL stays the same for the app on this machine, run after run and login after login, and
  every config on the machine shares it.
- A path or query that the `webhook_url` adds after the URL is added to `--forward-to` too: a
  delivery to `<URL>/runs?src=qb` goes to `http://localhost:3000/webhooks/qube/runs?src=qb`.
- Your app's answer is recorded as the delivery's outcome, as for any `webhook_url`. A 2xx
  succeeds. Any other status, a redirect (never followed), no answer within 5 s or no connection
  at all fails, and QuBe Sync retries it. While `qube listen` isn't running, deliveries fail the
  same way, and **Resend webhook** in the dashboard sends one again.
- `--json` prints the URL as the first line, then one JSON object per delivery, a line each:
  `at`, `summary` (what it is: kind, ids and event), `path`, `status` or `error`, and `ms`.
- It works with sandbox apps only, and read-only sessions may use it.
- A second `qube listen` for the same URL (in another terminal, or under another config) takes
  over, and the first one stops. Ctrl-C stops it (exit 130).
- `qube listen --forget` deletes this machine's URL for the app (it asks first; `--yes` answers
  for a script), and the next run gets a new one. With `--forward-to` too, it then listens on the
  new URL. You can listen from 10 machines per app; an 11th takes over the URL that was used least
  recently, says so, and that URL stops working.

## Checking your webhook endpoint: `qube check webhooks`

```bash
qube check webhooks http://localhost:3000/webhooks/qube --operation orders --record 42
```

`qube check webhooks URL` sends signed test deliveries to your app's own webhook endpoint and grades the HTTP
answers. `URL` is the base your app builds each webhook URL from (`QUBE_WEBHOOK_URL`). `--operation SLUG` is one of the
operation slugs your webhook route maps, and `--record ID` is the id of a record in your app that keeps a
connection (a shop, an account): `URL/connection/<ID>` is that connection's webhook URL. It signs with the app's
webhook secret, fetched the way `qube env` fetches it and kept in memory: the secret is never printed, logged
or written, and neither is anything your app answers. One request per check, each tied to a rule of QuBe
Sync's integration rules:

| Check | Sends | Passes when |
| --- | --- | --- |
| `bad-signature-401` | a run event with a wrong `v1` | 401 |
| `stale-timestamp-401` | a correct signature over a timestamp 10 minutes old | 401 |
| `raw-body-signature` | a correct signature over a body with unusual spacing, key order and `\u` escapes (`--operation`) | 2xx: the signature is over the raw bytes |
| `unknown-operation-404` | a correct signature, to `URL/qube-check-unknown/1` | 404 |
| `fast-2xx` | a run event to `URL/<--operation>/qube-check-5` | 2xx within 5 s |
| `connection-event-2xx` | with `--record ID`, a `connection.connected` for another connection, to `URL/connection/<ID>` | 2xx within 5 s |

```
CHECK                  RULE                                   SENT                         EXPECTED   GOT         RESULT
bad-signature-401      verify-signature                       wrong v1                     401        401 in 4ms  pass
stale-timestamp-401    refuse-stale-timestamp                 t 10 min old                 401        200 in 3ms  FAIL
...
4 passed, 2 failed, 0 skipped.
```

- **Exit codes.** 1 when a check fails, with the reason under the table; a skipped check (`--operation` and
  `--record` are what some need) isn't a failure. `--json` prints the same as data: `checks` (each with `rule`,
  `path`, `expected`, `got`, `status`, `elapsed_ms`, `result` and `detail`), `summary` and `ok`.
- **The deliveries are fake.** Their ids, references and delivery ids say `qube-check`, and no record of your
  app's matches them, so an app that is right logs and skips them. Each request carries `X-QUBE-Check: <check>`.
- **Where it sends.** It refuses QuBe Sync's own host, a `qube listen` relay URL included (QuBe Sync serves
  nothing there: check the `--forward-to` URL instead). A URL that isn't `localhost`, `127.0.0.1` or `[::1]`
  needs `--yes`, and says so. A production app needs `--production`. A redirect is reported and never followed,
  and a request with no answer in 10 s fails. It needs a read and write session, not an act-as grant, as
  `qube env` does.

## Production apps

In a production app, every request that changes something asks first, naming the app: every
`qube qb` operation except `list` (each one writes to the customer's real company file), creating or
changing a connection (its password, onboarding link and name index too), discarding a request,
driving the simulator, every workflow change, run, answer or cancellation, and `qube api` with any
method but `GET`. Reads never ask, and neither do `workflows validate`, `connections qwc` and
`match`, which change nothing. Removing something (a connection, a workflow, a run) asks in any
app. A script answers with `--yes`. Without a terminal, or under `--json`, a request that would ask
is refused until `--yes` is given, so nothing reaches a production company file by accident.
Sessions reach production apps only when you choose **every app** at `qube login`.

## QuBe Sync staff

These commands work only for QuBe Sync staff, with a session approved for staff permissions;
`qube help` leaves them out, and `qube help staff` lists them.

```bash
qube login --admin                                  # staff permissions; the session lasts 12 hours
qube admin status --check                           # service health; exit 1 when something needs attention
qube admin tail --production                        # events as they happen; Ctrl-C stops
qube act-as "Acme Corp" --reason "Onboarding: install and test invoice sync" --for 2h
qube workflows install invoice_sync --publish       # in Acme Corp's sandbox app
qube act-as --end
```

- `qube act-as` asks for a short-lived grant in a customer's organization, recorded with its
  reason. Until it ends, every command but `qube admin` works there, with the grant's apps, and
  prints `(acting as Acme Corp · sandbox apps · read and write · 1h 52m left)` on stderr, under
  `--json` too. A grant that ends stops the next command rather than letting it run in your own
  organization. `qube env` is unavailable while acting.
- A production grant (`--production`), and any grant an agent's session asks for, is approved in
  the browser. `--no-wait` prints the link and exits; `qube act-as --resume`, or the next command
  once it's approved, collects it.
- An AI agent gets its own session in its own config:
  `QUBE_CONFIG=~/.config/qube/agent.json qube login --agent "Claude Code" --admin`. Its `--yes`
  answers the CLI's questions only: the server holds it to its grant's access, write budget and
  rate limit.
- The grant lives in the config file under the host's session (`acting_as`, or `pending_act_as`
  while it waits for approval), with the same 0600 permissions as the session.

### Building integrations ("build it free")

A prospect's request becomes a build: a plan (the manifest), stages the server runs or an agent
moves, and gates a person approves in the browser. `qube admin` reads and drives it. Reading needs
`admin.read` (a request's text also `admin.read_customer_data`); changing anything needs
`builds.manage`, which `qube login --admin` asks for and an agent's session can hold. Nothing here
approves a gate or reopens a build: that is the admin page's, for a person.

```bash
qube admin integration-requests --status submitted         # the requests waiting for a build
qube admin integration-request REQUEST_ID                  # what they sent, the kit, the app, the latest build
qube admin builds start REQUEST_ID                         # start build n + 1
qube admin build BUILD_ID                                  # stages, gates, what's next, the test company's scenarios
qube admin build BUILD_ID manifest set plan.json           # save the plan; every problem prints as path: message
qube admin integration-request REQUEST_ID set-app APP_ID   # the sandbox app it installs and tests in
qube admin build BUILD_ID stage install done --output '{"installed": ["record_payment"]}'
qube admin build BUILD_ID test-company --wait              # runs on the server; each scenario prints as it ends
qube admin integration-request REQUEST_ID kit files --out ./kit
qube admin integration-request REQUEST_ID kit set --mapping-out out.rb --mapping-in in.rb
```

- `manifest set FILE|-` sends the file as written. A 422 prints the server's message and then each
  problem as `path: message` (under `--json`, the server's body is on stdout).
- `test-company --wait[=10m]` reads the build every few seconds until the stage is done or failed,
  prints each scenario's result once as it lands, and exits 1 on a failure or when the wait runs
  out (the stage keeps running on the server). The global `--timeout D` sets the limit too. Under
  `--json` the results go to stderr and stdout holds the build.
- `kit files --out DIR` writes the generated kit's files under `DIR`, creating directories. It
  refuses an absolute path or one with `..`, a file that is a link or a directory, and any
  existing file without `--force`, and it checks every path before it writes the first.
- `kit set` sends only the fields you give (`--stack`, `--http`, `--mapping-out FILE`,
  `--mapping-in FILE`); an empty value clears one.
- `stage STAGE running|done|failed [--error MSG] [--output JSON|@FILE]`: `failed` needs an error,
  and the stage keeps the output it has when `--output` is left out.

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

`qube login --agent NAME` says an AI agent will hold the session: the person approving sees the
name, and the session can revoke only itself, never your other sessions.

The CLI calls the API with the session token, naming the app it acts as, so revoking a session cuts
it off at once, and the server holds a read-only session to reads. The token goes only to the host
that issued it. The app's API key is read only for `qube env`, and only by a read-and-write session.
The OpenAPI document `qube qb` reads is public and is fetched without the token.

### Read-only sessions

`qube login --read-only` asks for a session that can look but not change anything; whoever approves
it in the dashboard sees the request preselected and has the last word. A read-only session can list
and show everything its reach allows, run QuickBooks queries (`qube qb ... list`), validate charts,
match names, download a connection's QWC file and run `qube listen`. It can't create, change,
delete, discard, run or answer anything (a workflow's question, say), and it can't read an app's API
key or webhook secret, so `qube env` isn't available to it.
The server enforces this; the CLI also refuses such a command before sending it. `qube status` and
`qube sessions` show each session's access.

## Configuration

| What | Where |
| --- | --- |
| Sessions (one per host), default host and default app | `$XDG_CONFIG_HOME/qube/credentials.json` (`~/Library/Application Support/qube/` on macOS, `%AppData%\qube\` on Windows), mode 0600, written atomically. `QUBE_CONFIG` overrides the path. |
| Host | `--host`, else `QUBE_HOST`, else the host you last logged in to (`qube use --host H` changes it), else `https://qubesync.com`. |
| HTTP timeout | `--timeout 30` (seconds) or `2m`; `QUBE_TIMEOUT`. Default 60 s. Ctrl-C cancels any command (exit 130). |
| Device id | `$XDG_CONFIG_HOME/qube/device-id`, beside the credentials (`QUBE_CONFIG` doesn't move it): a random id for this machine, so `qube listen` gets the same URL for an app every time. Nothing secret. |
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
```

### Releasing

A release is a tag on `main`: `git tag v0.3.0 && git push origin v0.3.0` runs
[goreleaser](.goreleaser.yaml) from `.github/workflows/release.yml`, which tests, builds the six
binaries (the tag, without its `v`, becomes `qube version`), writes `checksums.txt` and publishes the
GitHub release with notes made from the commits since the previous tag.

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
