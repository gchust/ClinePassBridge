#!/usr/bin/env python3
"""Validate and version fork releases without modifying committed upstream files."""

import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys

UPSTREAM = "xiao-qiu-qiu/ClinePassBridge"
CUSTOM_BRANCH = "custom/main"
BASE_TAG = re.compile(r"v[0-9]+\.[0-9]+\.[0-9]+")
FORK_TAG = re.compile(r"(v[0-9]+\.[0-9]+\.[0-9]+)-fork\.([1-9][0-9]*)")
VERSION_LINE = re.compile(r'^const Version = "([^"]+)"$', re.MULTILINE)


def command(*args, check=True):
    result = subprocess.run(args, text=True, capture_output=True, check=False)
    if check and result.returncode:
        raise RuntimeError(f"{args[0]} failed: {result.stderr.strip()}")
    return result


def git(*args):
    return command("git", *args).stdout.strip()


def api(path, missing_ok=False, paginate=False):
    args = ["gh", "api", path]
    if paginate:
        args += ["--paginate", "--slurp"]
    result = command(*args, check=False)
    if result.returncode:
        if missing_ok and "HTTP 404" in result.stderr:
            return None
        raise RuntimeError(result.stderr.strip())
    return json.loads(result.stdout)


def next_tag(base, tags):
    pattern = re.compile(re.escape(base) + r"-fork\.([1-9][0-9]*)")
    revisions = [int(m.group(1)) for tag in tags if (m := pattern.fullmatch(tag))]
    return f"{base}-fork.{max(revisions, default=0) + 1}"


def select_base(releases, is_ancestor):
    releases = sorted(releases, key=lambda r: r.get("published_at") or "", reverse=True)
    for release in releases:
        tag = release["tag_name"]
        if not release["draft"] and not release["prerelease"] and BASE_TAG.fullmatch(tag):
            if is_ancestor(tag):
                return tag
    raise ValueError("No stable upstream release tag is an ancestor of the merged commit")


def validate_pr(pr, repo):
    if not pr.get("merged") or pr["base"]["ref"] != CUSTOM_BRANCH:
        raise ValueError("Only a PR merged into custom/main can be released")
    if pr["base"]["repo"]["full_name"].lower() != repo.lower():
        raise ValueError("The pull request belongs to another repository")
    commit = pr.get("merge_commit_sha") or ""
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ValueError("The pull request has no valid merge commit")
    return commit


def check_versions(root):
    registry = json.loads((root / "marketplace/registry.json").read_text())
    plugin = registry["plugins"][0]
    if (registry["schema_version"] != 1 or plugin["id"] != "clinepassbridge"
            or plugin["install"]["type"] != "github-release"):
        raise ValueError("Unexpected upstream marketplace schema")
    source = (root / "internal/bridge/types.go").read_text()
    matches = VERSION_LINE.findall(source)
    if matches != [plugin["version"]]:
        raise ValueError("Plugin and registry versions must match")
    return registry, source


def stamp(root, tag, repo):
    if not FORK_TAG.fullmatch(tag):
        raise ValueError("Expected vX.Y.Z-fork.N")
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo):
        raise ValueError("Invalid repository name")
    registry, source = check_versions(root)
    version = tag.removeprefix("v")
    (root / "internal/bridge/types.go").write_text(
        VERSION_LINE.sub(f'const Version = "{version}"', source)
    )
    plugin = registry["plugins"][0]
    plugin.update(version=version, repository=f"https://github.com/{repo}",
                  homepage=f"https://github.com/{repo}",
                  logo=f"https://raw.githubusercontent.com/{repo}/{tag}/logo.png")
    (root / "marketplace/registry.json").write_text(
        json.dumps(registry, ensure_ascii=False, indent=2) + "\n"
    )
    check_versions(root)


def fork_tags():
    tags = {}
    for tag in git("tag", "--list", "v*-fork.*").splitlines():
        if FORK_TAG.fullmatch(tag):
            tags[tag] = git("rev-parse", f"{tag}^{{commit}}")
    return tags


def prepare(repo, number):
    pr = api(f"repos/{repo}/pulls/{number}")
    commit = validate_pr(pr, repo)
    git("fetch", "origin", "--tags",
        f"+refs/heads/{CUSTOM_BRANCH}:refs/remotes/origin/{CUSTOM_BRANCH}")
    if command("git", "merge-base", "--is-ancestor", commit,
               f"origin/{CUSTOM_BRANCH}", check=False).returncode:
        raise ValueError("Merge commit is not reachable from origin/custom/main")
    # Fetching all upstream tags also handles feature merges before the next sync.
    git("fetch", f"https://github.com/{UPSTREAM}.git", "--tags")
    pages = api(f"repos/{UPSTREAM}/releases?per_page=100", paginate=True)
    releases = [release for page in pages for release in page]
    base = select_base(releases, lambda tag: command(
        "git", "merge-base", "--is-ancestor", tag, commit, check=False
    ).returncode == 0)
    # Never overwrite a conflicting upstream marker in the fork.
    git("push", "origin", f"refs/tags/{base}")
    for _ in range(20):
        git("fetch", "origin", "--tags")
        tags = fork_tags()
        existing = [tag for tag, sha in tags.items() if sha == commit]
        if len(existing) > 1:
            raise ValueError("Multiple fork tags already reference this merge commit")
        if existing:
            tag = existing[0]
            base = FORK_TAG.fullmatch(tag).group(1)
            break
        tag = next_tag(base, tags)
        # GitHub creates refs atomically. Concurrent PRs cannot claim the same tag.
        result = command(
            "gh", "api", f"repos/{repo}/git/refs", "--method", "POST",
            "-f", f"ref=refs/tags/{tag}", "-f", f"sha={commit}", check=False,
        )
        if result.returncode == 0:
            break
        if "Reference already exists" not in result.stdout + result.stderr:
            raise RuntimeError(result.stderr.strip())
    else:
        raise RuntimeError("Could not allocate a fork tag after 20 concurrent attempts")
    release = api(f"repos/{repo}/releases/tags/{tag}", missing_ok=True)
    values = {"tag": tag, "version": tag.removeprefix("v"),
              "base_tag": base, "commit": commit,
              "publish": str(release is None or release["draft"]).lower()}
    if os.environ.get("GITHUB_OUTPUT"):
        with open(os.environ["GITHUB_OUTPUT"], "a") as output:
            for key, value in values.items():
                output.write(f"{key}={value}\n")
    print(json.dumps(values, indent=2))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="operation", required=True)
    sub.add_parser("check")
    stamp_parser = sub.add_parser("stamp")
    stamp_parser.add_argument("--tag", required=True)
    stamp_parser.add_argument("--repo", required=True)
    prepare_parser = sub.add_parser("prepare")
    prepare_parser.add_argument("--repo", required=True)
    prepare_parser.add_argument("--pr", required=True, type=int)
    args = parser.parse_args()
    if args.operation == "check":
        check_versions(Path.cwd())
    elif args.operation == "stamp":
        stamp(Path.cwd(), args.tag, args.repo)
    else:
        prepare(args.repo, args.pr)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, RuntimeError, KeyError) as error:
        print(f"::error::{error}", file=sys.stderr)
        sys.exit(1)
