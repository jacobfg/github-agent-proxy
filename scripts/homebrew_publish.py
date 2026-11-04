#!/usr/bin/env python3
"""Publish validated bottles and update only the formula on main."""
import argparse
import json
from pathlib import Path
import re
import shutil
import subprocess
import tempfile

from homebrew_release import FORMULA, REPO, ROOT, bottle_assets, digest, output, run, verify_source, render_formula


def release_info(value):
    result = subprocess.run(["gh", "release", "view", value, "--repo", REPO, "--json", "isDraft"], capture_output=True, text=True)
    if result.returncode == 0:
        return json.loads(result.stdout)
    # A network/auth error must not be mistaken for an absent release.
    if "release not found" not in result.stderr.lower():
        raise RuntimeError("could not inspect the GitHub release")
    return None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("source", type=Path)
    parser.add_argument("bottles", type=Path)
    args = parser.parse_args()
    source, bottles = args.source.resolve(), args.bottles.resolve()
    info = verify_source(source)
    if not info["release"]:
        raise ValueError("development builds cannot be published")
    value = info["version"]
    assets = bottle_assets(bottles, value)
    if output("git", "rev-parse", f"refs/tags/{value}^{{commit}}") != info["commit"]:
        raise ValueError("release tag changed")
    run("git", "fetch", "origin", "main")
    run("git", "merge-base", "--is-ancestor", info["commit"], "origin/main")
    current = output("git", "show", "origin/main:Formula/github-agent-proxy.rb")
    if render_formula(current, value, info["sha256"]) != (source / "github-agent-proxy.rb").read_text():
        raise ValueError("formula implementation changed on main since the release tag; refusing to overwrite")
    match = re.search(r'^  version "([0-9.]+)"$', current, re.MULTILINE)
    if match and tuple(map(int, match[1].split('.'))) > tuple(map(int, value.split('.'))):
        raise ValueError("refusing to replace a newer formula release")

    run("brew", "tap", "jacobfg/github-agent-proxy", str(ROOT))
    tap = Path(output("brew", "--repository", "jacobfg/github-agent-proxy"))
    run("git", "-C", str(tap), "remote", "set-url", "origin", f"https://github.com/{REPO}")
    if subprocess.run(["brew", "help", "trust"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0:
        run("brew", "trust", "--formula", "jacobfg/github-agent-proxy/github-agent-proxy")
    formula = tap / "Formula/github-agent-proxy.rb"
    shutil.copyfile(source / "github-agent-proxy.rb", formula)
    run("brew", "bottle", "--merge", "--write", "--no-commit", *map(str, sorted(bottles.glob("*.bottle.json"))), cwd=bottles)
    run("brew", "style", "jacobfg/github-agent-proxy/github-agent-proxy")
    generated = formula.read_bytes()
    (bottles / "github-agent-proxy.rb").write_bytes(generated)

    with tempfile.TemporaryDirectory(prefix="proxy-release-") as temporary:
        upload = Path(temporary)
        shutil.copyfile(source / info["source"], upload / info["source"])
        for path, public_name in assets:
            shutil.copyfile(path, upload / public_name)
        checksums = "".join(f"{digest(path)}  {path.name}\n" for path in sorted(upload.iterdir()))
        (upload / "SHA256SUMS").write_text(checksums)
        release = release_info(value)
        if release is None:
            run("gh", "release", "create", value, "--repo", REPO, "--verify-tag", "--draft", "--title", value,
                "--notes", "Homebrew bottles built from source and tested on Intel and Apple Silicon macOS. See docs/homebrew.md for installation and sandbox setup.")
            release = {"isDraft": True}
        if release["isDraft"]:
            run("gh", "release", "upload", value, "--repo", REPO, "--clobber", *map(str, sorted(upload.iterdir())))
            run("gh", "release", "edit", value, "--repo", REPO, "--draft=false")
        else:
            # A retry may finish the formula update after a successful release.
            # Published assets are immutable: compare rather than overwrite.
            downloaded = upload / "existing"
            downloaded.mkdir()
            for path in sorted(upload.iterdir()):
                if not path.is_file():
                    continue
                run("gh", "release", "download", value, "--repo", REPO, "--dir", str(downloaded), "--pattern", path.name)
                if digest(path) != digest(downloaded / path.name):
                    raise ValueError("published release assets differ; refusing to overwrite")

    # Only this generated formula is changed. If main advances, a normal push
    # fails safely; rerun this job to retry without replacing published assets.
    run("git", "checkout", "-B", "homebrew-release-update", "origin/main")
    (ROOT / "Formula/github-agent-proxy.rb").write_bytes(generated)
    run("git", "add", "Formula/github-agent-proxy.rb")
    if subprocess.run(["git", "diff", "--cached", "--quiet"], check=False).returncode == 0:
        return
    run("git", "-c", "user.name=github-actions[bot]", "-c", "user.email=41898282+github-actions[bot]@users.noreply.github.com",
        "-c", "commit.gpgsign=false", "commit", "-m", f"Update Homebrew formula to {value}")
    run("git", "push", "origin", "HEAD:main")


if __name__ == "__main__":
    main()
