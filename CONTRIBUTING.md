# Contributing to Bluefin Report

- Read [AGENTS.md](AGENTS.md) and the [design plan](systemd-report-design-plan.md) first.
- Open an issue before large behavioral changes. Issues and pull requests are driven by Prow;
  see [how issues and PRs work here](https://github.com/projectbluefin/common/blob/main/docs/skills/label-workflow.md).
- Pull request titles and commits follow [Conventional Commits](https://www.conventionalcommits.org/)
  (`feat:`, `fix:`, `docs:`, `ci:`, ...). AI-assisted commits carry an `Assisted-by:` trailer.

## Build and test

Needs Go (version in `go.mod`), Make, and a CGO toolchain for the race detector.
GTK 4 and libadwaita are runtime dependencies only. Translation targets need GNU
gettext: `make mo data` (run by GoReleaser for every release archive) works with any
recent version, `make pot` needs 0.25 or newer. See [Translations](README.md#translations).

```sh
make build
make lint test validate-data
./build/bluefin-report --dry-run   # full flow; gh calls are logged, not run
```

## Releases

Releases use calendar versions `YY.MM.N` (N starts at 1 each month), allocated by
the existing `scripts/next-version.sh`. The Releases workflow checks on Tuesdays
at 08:23 UTC. It skips publication when only README/gallery changes have landed
since the latest reachable stable tag, including historical `v` tags.

The release must pass lint, the full race-test suite, desktop metadata validation,
release-planning tests, and live screenshot capture before any tag is pushed.
GoReleaser publishes both Linux architectures, checksums, and the captured images
with their source/binary/fixture provenance. The same workflow publishes directly
after creating its tag: it does not depend on a second workflow being triggered
by a `GITHUB_TOKEN` tag push.

If a run allocates a tag but fails before publication, the next scheduled or
manual publishing run can reuse that numeric tag only at the exact same source
commit and only while no published stable release exists. The publication
boundary never moves an existing tag. This recovers tag allocation, not cleanup
of assets from a partially published release.

Use the **Releases** workflow's **Run workflow** control for an on-demand preview.
`publish` defaults to false: previews build snapshot archives and screenshot
artifacts without creating a tag, release, or README-refresh PR. PRs and merge
groups also run read-only previews. Publishing manually requires the default
branch and `publish: true`; scheduled releases are not cut per commit.

Read-only previews have separate concurrency groups and cannot replace a queued
publication. Running publications are never cancelled. GitHub can still replace
a pending publishing run with a newer publishing run in the same group.

For maintainer recovery, `make bump` still allocates an annotated tag on a clean,
up-to-date `main` after checks. Push the printed tag command to run the same gated
release flow. Do not hand-pick version numbers. Agents must not cut or push real
release tags while implementing or verifying automation.

### Live README screenshots

`make screenshots` launches the compiled app with `--dry-run`, supplies only the
existing snapshot test fixtures, and navigates using AT-SPI and keyboard actions.
It captures Overview, Metrics, and Review without publishing anything. It needs
a supported GTK/Libadwaita runtime (Libadwaita 1.8 or later), Python's PyGObject and
pyatspi bindings, native introspection typelibs (`gobject-introspection` on Fedora),
Xvfb, D-Bus/AT-SPI, xdotool, ImageMagick, and an Adwaita icon theme.
The release workflow provides these in Fedora 44 instead of relying on an old
runner desktop. Local captures write to `build/screenshots`; `SCREENSHOT_DIR` and
`PYTHON` are Make overrides.

On Linuxbrew, keep native library overrides confined to the app so the system
D-Bus/accessibility daemons do not load incompatible libraries:

```sh
SCREENSHOT_LIBRARY_PATH=/home/linuxbrew/.linuxbrew/lib \
  make screenshots PYTHON=/usr/bin/python3
```

Each published release attaches `screenshot-overview.png`, `screenshot-metrics.png`,
`screenshot-review.png`, and `screenshot-provenance.json`. A bot opens or updates
one PR changing only the four tracked assets under `data/screenshots`; the README
uses those stable relative image paths. Merging that PR refreshes the gallery.
Identical images do not open a new PR or cause another release.

When newer released images already match the default branch, the updater closes
an obsolete owned screenshot PR. Retained bot branches after a squash/rebase merge
are reconciled with a non-forced two-parent commit using the current default tree.
Unowned branches, ambiguous PR history, and changes outside the four screenshot
assets fail closed rather than overwriting contributor work.

The README-refresh job uses the existing organization secrets
`MERGERAPTOR_APP_ID` and `MERGERAPTOR_PRIVATE_KEY`. The GitHub App must be installed
for this repository with **Contents: write** and **Pull requests: write**. Its
installation token is scoped to this repository and these permissions, so the
bot's PR triggers normal CI (unlike a PR created with `GITHUB_TOKEN`). Missing
credentials or permissions fail the refresh job explicitly; they never authorize
a direct write to the default branch.
