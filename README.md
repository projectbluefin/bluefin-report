# Bluefin Report

A GTK4/Libadwaita app that turns a [`systemd-report`](https://www.freedesktop.org/software/systemd/man/latest/systemd-report.html)
snapshot into a reviewed, privacy-scrubbed GitHub issue. You see exactly what
leaves the machine before anything is sent; it is published with your own `gh`
login as a gist plus an issue in the right Bluefin repository.

## Requirements

- systemd 262 (`systemd-report`)
- GTK 4 and libadwaita 1 (≥ 1.6) shared libraries from your OS
- [`gh`](https://cli.github.com/) — sign-in happens inside the app

## Install

```bash
brew install --cask projectbluefin/tap/bluefin-report
```

## Usage

```bash
bluefin-report                   # open the app
bluefin-report --dry-run         # full flow, but gh calls are logged, not run
bluefin-report --print-summary   # collect a snapshot and print the overview, no UI
```

## Privacy

- Removed before you ever see it: hostname, machine ID, boot ID, machine-info,
  hardware serial numbers, system UUID and asset tags, report-signing keys,
  your username, MAC/IPv4/IPv6 addresses, email addresses, and home paths
  (`/home/<user>` → `~`). Pattern redaction is best-effort; the Review page
  lists every removal with a count.
- Never sent unless you opt in per entry: file contents (`io.systemd.Files.*`)
  and user-session metrics.
- Sent byte-for-byte as shown in Review; editing anything withdraws consent.
- Raw snapshots stay in memory. Drafts keep only the reviewed payload under
  `$XDG_STATE_HOME/bluefin-report/drafts/` (mode 0700/0600). Discarding a
  draft never deletes anything already on GitHub.

## Development

```bash
make test           # go test -race ./...
make run            # build and run with --dry-run
make lint           # gofmt + go vet
make validate-data  # desktop-file-validate (+ appstreamcli if present)
```

Licensed under Apache-2.0.
