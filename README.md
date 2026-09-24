# qube — the QuBe Sync command line

Log in once from a terminal; then get your app's keys, create connections (simulated ones too),
watch requests, drive the simulator and push workflows — from a shell, a script, or an agent.

```bash
qube login                          # opens the dashboard; confirm the code; sandbox apps by default
qube apps                           # what this session may act on
qube use "My App Dev"               # the default app for the commands below
qube env --write .env               # QUBE_URL / QUBE_API_KEY / QUBE_WEBHOOK_SECRET, never printed
qube connections create --simulated --name "Local dev"
qube requests list <connection>
qube simulator faults <connection> --next 3100
qube workflows push ./chart.json --publish
qube api GET "/api/v2/connections/<id>/customers?max_returned=5"
```

`qube help` lists every command. Every command takes `--json` (machine output), `--host`, `--app`
and `--timeout`, anywhere on the line.

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
paths; the session token only to `/api/cli`.

## Configuration

| What | Where |
| --- | --- |
| Sessions (one per host), default host and default app | `$XDG_CONFIG_HOME/qube/credentials.json` (`~/Library/Application Support/qube/` on macOS, `%AppData%\qube\` on Windows), mode 0600, written atomically. `QUBE_CONFIG` overrides the path. |
| Host | `--host`, else `QUBE_HOST`, else the host you last logged in to (`qube use --host H` changes it), else `https://qubesync.com`. |
| HTTP timeout | `--timeout 30` (seconds) or `2m`; `QUBE_TIMEOUT`. Default 60 s. Ctrl-C cancels any command (exit 130). |
| Browser | `QUBE_NO_BROWSER=1` keeps `qube login` from opening one (the URL is always printed). |

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
