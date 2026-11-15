.PHONY: build test run lint validate-data bump screenshots pot mo data

BUILD_DIR=build
SCREENSHOT_DIR ?= $(BUILD_DIR)/screenshots
PYTHON ?= python3
export CGO_ENABLED=0

# Translations (GNU gettext): po/POTFILES lists the files with translatable
# text, po/LINGUAS the languages that have a po/<lang>.po.
DOMAIN = bluefin-report
POTFILES := $(shell cat po/POTFILES)
LINGUAS := $(shell sed 's/#.*//' po/LINGUAS)
DESKTOP = io.projectbluefin.Report.desktop
METAINFO = io.projectbluefin.Report.metainfo.xml
# Compiled catalogs; the app looks for them in locale/ beside its binary.
# Release archives take them from ./locale (make mo LOCALEDIR=locale).
LOCALEDIR ?= $(BUILD_DIR)/locale

build:
	go build -trimpath -o $(BUILD_DIR)/bluefin-report ./cmd/bluefin-report

# -race needs cgo.
test:
	CGO_ENABLED=1 go test -race ./...

run: build
	./$(BUILD_DIR)/bluefin-report --dry-run

# Requires a supported GTK/Libadwaita runtime, AT-SPI, Xvfb, xdotool and ImageMagick.
screenshots: build
	xvfb-run -a -s "-screen 0 1280x1024x24" dbus-run-session -- \
		$(PYTHON) scripts/capture-screenshots.py --binary $(BUILD_DIR)/bluefin-report --output $(SCREENSHOT_DIR)

lint:
	test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }
	go vet ./...
	go mod tidy -diff

validate-data:
	desktop-file-validate data/io.projectbluefin.Report.desktop
	@if command -v appstreamcli >/dev/null 2>&1; then \
		appstreamcli validate --no-net data/io.projectbluefin.Report.metainfo.xml; \
	else echo "==> skipping metainfo: appstreamcli not installed"; fi

# Translation template. Needs GNU gettext 0.25 or newer, the first to read Go.
XGETTEXT = xgettext --from-code=UTF-8 --add-comments=TRANSLATORS: --package-name=$(DOMAIN) \
	--msgid-bugs-address=https://github.com/projectbluefin/bluefin-report/issues --output=po/$(DOMAIN).pot
pot:
	$(XGETTEXT) --language=Go --keyword --keyword=G --keyword=NG:1,2 --keyword=N $(filter %.go,$(POTFILES))
	$(XGETTEXT) --join-existing $(filter-out %.go,$(POTFILES))

mo: $(foreach l,$(LINGUAS),$(LOCALEDIR)/$(l)/LC_MESSAGES/$(DOMAIN).mo)

$(LOCALEDIR)/%/LC_MESSAGES/$(DOMAIN).mo: po/%.po
	@mkdir -p $(@D)
	msgfmt --check --output-file=$@ $<

# Desktop file and metainfo with every translation merged in.
data: $(BUILD_DIR)/data/$(DESKTOP) $(BUILD_DIR)/data/$(METAINFO)

$(BUILD_DIR)/data/$(DESKTOP): data/$(DESKTOP) po/LINGUAS $(LINGUAS:%=po/%.po)
	@mkdir -p $(@D)
	msgfmt --desktop --template=$< -d po --output-file=$@

$(BUILD_DIR)/data/$(METAINFO): data/$(METAINFO) po/LINGUAS $(LINGUAS:%=po/%.po)
	@mkdir -p $(@D)
	msgfmt --xml --template=$< -d po --output-file=$@

# Releases are cut deliberately, not per commit: YY.MM.N (scripts/next-version.sh).
# Tag only what main ships, so release notes compare against the previous tag.
bump:
	@git fetch --quiet origin main && \
	if [ "$$(git rev-parse HEAD)" != "$$(git rev-parse origin/main)" ]; then \
		echo "HEAD is not origin/main. Check out and pull main before bumping the version."; \
		exit 1; \
	fi
	@if [ -n "$$(git status --porcelain)" ]; then \
		echo "Working directory is not clean. Commit or stash changes before bumping the version."; \
		exit 1; \
	fi
	$(MAKE) lint test
	@version=$$(./scripts/next-version.sh) && \
		git tag -a $$version -m "Version $$version" && \
		echo "Tagged $$version. Publish it with: git push origin $$version"
