#!/usr/bin/env python3
"""Select a stable calendar release without mutating Git or GitHub."""

import argparse
import os
import re
import subprocess
import json
import urllib.error
import urllib.parse
import urllib.request

STABLE_TAG = re.compile(r"v?[0-9]+\.[0-9]+\.[0-9]+\Z")
NUMERIC_TAG = re.compile(r"[0-9]+\.[0-9]+\.[0-9]+\Z")
RELEASE_FILES = {"go.mod", "go.sum", "Makefile", ".goreleaser.yaml", ".goreleaser.yml", ".github/workflows/release.yml"}


def git(*args):
    return subprocess.check_output(["git", *args], text=True).strip()


def relevant(path):
    return path in RELEASE_FILES or path.startswith(("cmd/", "internal/", "po/", "scripts/")) or (
        path.startswith("data/") and not path.startswith("data/screenshots/")
    )


def previous_tag(exclude=""):
    tags = [tag for tag in git("tag", "--merged", "HEAD").splitlines() if STABLE_TAG.fullmatch(tag) and tag != exclude]
    if not tags:
        return ""
    return git("describe", "--tags", "--abbrev=0", *[arg for tag in tags for arg in ("--match", tag)], "HEAD")


def published_release(tag):
    repository = os.environ.get("GH_REPO") or os.environ.get("GITHUB_REPOSITORY")
    token = os.environ.get("GH_TOKEN")
    if not repository or not token:
        raise ValueError("release recovery requires GH_REPO or GITHUB_REPOSITORY and GH_TOKEN")
    request = urllib.request.Request(
        f"https://api.github.com/repos/{repository}/releases/tags/{urllib.parse.quote(tag, safe='')}",
        headers={"Authorization": f"Bearer {token}", "Accept": "application/vnd.github+json"},
    )
    try:
        with urllib.request.urlopen(request) as response:
            release = json.load(response)
    except urllib.error.HTTPError as error:
        error.close()
        if error.code == 404:
            return False
        raise
    return not release["draft"] and not release["prerelease"]


def plan(event, ref, sha, default_branch, requested_publish):
    commit = git("rev-parse", "HEAD")
    if git("rev-parse", f"{sha}^{{commit}}") != commit:
        raise ValueError("checkout must match the immutable event SHA")
    publish = event in {"schedule", "push"} or (event == "workflow_dispatch" and requested_publish)
    if event not in {"push", "schedule", "workflow_dispatch", "pull_request", "merge_group"}:
        raise ValueError("unsupported release event")
    tag = ""
    if event == "push":
        tag = ref.removeprefix("refs/tags/")
        if ref != f"refs/tags/{tag}" or not NUMERIC_TAG.fullmatch(tag):
            raise ValueError("tag releases require a stable numeric X.Y.Z tag")
        if git("rev-parse", f"refs/tags/{tag}^{{commit}}") != commit:
            raise ValueError("event tag does not point to the event commit")
        subprocess.run(["git", "merge-base", "--is-ancestor", commit, f"refs/remotes/origin/{default_branch}"], check=True)
    elif publish and ref != f"refs/heads/{default_branch}":
        raise ValueError("scheduled and manual publication is restricted to the default branch")
    previous = previous_tag(tag)
    changed = git("diff", "--no-renames", "--name-only", "-z", previous, "HEAD").split("\0") if previous else git("ls-files", "-z").split("\0")
    has_changes = any(relevant(path) for path in changed)
    # Read-only PR, merge-queue and manual previews build even docs-only refs.
    run = event == "push" or has_changes or not publish
    # Recover only a tag allocated at this exact source commit, never an older
    # unpublished release or a preview. All GitHub access here is read-only.
    if (
        publish and event != "push" and not has_changes
        and NUMERIC_TAG.fullmatch(previous)
        and git("rev-parse", f"refs/tags/{previous}^{{commit}}") == commit
        and not published_release(previous)
    ):
        tag = previous
        run = True
    if not tag:
        tag = subprocess.check_output(["bash", "scripts/next-version.sh"], text=True).strip()
        if not NUMERIC_TAG.fullmatch(tag):
            raise ValueError("next-version.sh did not return a stable numeric tag")
    return {"run": str(run).lower(), "publish": str(publish and run).lower(), "tag": tag, "sha": commit, "previous": previous}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--event", required=True)
    parser.add_argument("--ref", required=True)
    parser.add_argument("--sha", required=True)
    parser.add_argument("--default-branch", required=True)
    parser.add_argument("--publish", choices=("true", "false"), default="false")
    args = parser.parse_args()
    result = plan(args.event, args.ref, args.sha, args.default_branch, args.publish == "true")
    for key, value in result.items():
        print(f"{key}={value}")
    if output := os.environ.get("GITHUB_OUTPUT"):
        with open(output, "a", encoding="utf-8") as stream:
            for key, value in result.items():
                stream.write(f"{key}={value}\n")
    if not result["run"] == "true":
        print("No release-relevant changes since the latest reachable stable tag; skipping publication.")


if __name__ == "__main__":
    main()
