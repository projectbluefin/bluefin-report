# AGENTS.md

Bluefin Report: Go + GTK4/Libadwaita via [puregotk](https://codeberg.org/puregotk/puregotk) (no CGO for builds).
Design and status: [systemd-report-design-plan.md](systemd-report-design-plan.md).

## Layout

- `cmd/bluefin-report`: entry point and flags (`--dry-run`, `--print-summary`)
- `internal/snapshot`: runs `systemd-report` and parses it
- `internal/privacy`: scrubs the payload before anything leaves the machine
- `internal/extras`: unprivileged probes systemd-report can't provide (image ref/tag/digest, CPU, GPU, GNOME) and the opt-in diagnostics (boot errors, crashes, previous-boot kernel problems, desktop/update/network/portal warnings, disks and graphics, extensions, Flatpaks)
- `internal/publish`: issue, labels, report artifact (gist fallback) and screenshots through `gh`, resumable by report ID
- `internal/ui`: the GTK app
- `data/`: desktop file, AppStream metainfo, icon

## Rules

- puregotk caches callbacks by function-pointer address in a fixed, never-freed table.
  Connect signal handlers once per widget; never create a fresh callback per event.
  Work from goroutines reaches the main thread only through `post()` in `internal/ui/ui.go`,
  which reuses one package-level callback.
- Tests never call the real `gh` for writes. Use `--dry-run` to drive the full flow by hand;
  it logs `gh` calls instead of running them.
- Releases are calendar versions `YY.MM.N`, allocated by `scripts/next-version.sh`.
  The scheduled release workflow publishes only after checks and live screenshots;
  manual workflow runs default to a non-publishing snapshot. Never release per commit.
  Agents do not cut or push actual release tags while developing or verifying automation.
- Never weaken the privacy scrubber to make a test pass; the attachment must not contain hostname or username.

## Checks

```sh
make lint test validate-data
```
