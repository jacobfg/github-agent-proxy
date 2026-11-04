#!/usr/bin/env python3
"""Prepare checked source/formula artifacts; publish only after native bottle tests."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess
import urllib.parse

ROOT = Path(__file__).resolve().parents[1]
REPO = "jacobfg/github-agent-proxy"
FORMULA = "github-agent-proxy"
SOURCE_PATTERN = r"(?ms)^  # BEGIN RELEASE SOURCE\n.*?^  # END RELEASE SOURCE"
BOTTLE_PATTERN = r"(?ms)^  bottle do\n.*?^  end\n\n?"
VERSION_PATTERN = r"(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)"


def run(*args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)


def output(*args):
    return subprocess.check_output(args, text=True).strip()


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def version(value):
    if not re.fullmatch(VERSION_PATTERN, value):
        raise ValueError("version must be stable x.y.z without a v prefix")
    return value


def source_url(value):
    return f"https://github.com/{REPO}/releases/download/{version(value)}/{FORMULA}-{value}-source.tar.gz"


def render_formula(template, value, sha256):
    value = version(value)
    if not re.fullmatch(r"[a-f0-9]{64}", sha256):
        raise ValueError("invalid source SHA-256")
    replacement = (f'  # BEGIN RELEASE SOURCE\n  url "{source_url(value)}"\n'
                   f'  version "{value}"\n  sha256 "{sha256}"\n  # END RELEASE SOURCE')
    if len(re.findall(SOURCE_PATTERN, template)) != 1:
        raise ValueError("formula must have one release source marker")
    template = re.sub(BOTTLE_PATTERN, "", template)
    return re.sub(SOURCE_PATTERN, lambda _: replacement, template)


def prepare(destination, release):
    destination.mkdir(parents=True, exist_ok=True)
    value = version(release) if release else "0.0.0"
    commit = output("git", "rev-parse", "HEAD")
    if release:
        tag_commit = output("git", "rev-parse", f"refs/tags/{value}^{{commit}}")
        if commit != tag_commit:
            raise ValueError("release tag must point to the checked-out commit")
        run("git", "merge-base", "--is-ancestor", commit, "origin/main")
    archive = destination / f"{FORMULA}-{value}-source.tar.gz"
    run("git", "archive", "--format=tar.gz", f"--prefix={FORMULA}-{value}/", f"--output={archive}", commit)
    checksum = digest(archive)
    formula = render_formula((ROOT / "Formula" / f"{FORMULA}.rb").read_text(), value, checksum)
    (destination / f"{FORMULA}.rb").write_text(formula)
    manifest = {"version": value, "commit": commit, "release": bool(release),
                "source": archive.name, "sha256": checksum, "source_url": source_url(value)}
    (destination / "source.json").write_text(json.dumps(manifest, indent=2) + "\n")


def verify_source(directory):
    info = json.loads((directory / "source.json").read_text())
    value = version(info["version"])
    if info["source"] != f"{FORMULA}-{value}-source.tar.gz" or info["source_url"] != source_url(value):
        raise ValueError("unexpected source archive identity")
    if not re.fullmatch(r"[a-f0-9]{40}", info["commit"]):
        raise ValueError("invalid source commit")
    if digest(directory / info["source"]) != info["sha256"]:
        raise ValueError("source checksum mismatch")
    if (directory / f"{FORMULA}.rb").read_text() != render_formula(
            (ROOT / "Formula" / f"{FORMULA}.rb").read_text(), value, info["sha256"]):
        raise ValueError("formula differs from the checked-out release template")
    return info


def bottle_assets(directory, value):
    """Validate matrix outputs and translate Homebrew local names to URL names."""
    expected = {"arm64_sequoia", "sequoia"}
    found = set()
    assets = []
    files = sorted(directory.glob("*.bottle.json"))
    if len(files) != len(expected):
        raise ValueError("expected one bottle JSON file per macOS architecture")
    for path in files:
        data = json.loads(path.read_text())
        if len(data) != 1:
            raise ValueError("unexpected formula in bottle metadata")
        key, info = next(iter(data.items()))
        if key != f"jacobfg/github-agent-proxy/{FORMULA}":
            raise ValueError("unexpected bottle tap/formula")
        if info["formula"]["pkg_version"] != value or info["formula"]["name"] != FORMULA:
            raise ValueError("bottle version/name mismatch")
        if info["bottle"]["root_url"] != f"https://github.com/{REPO}/releases/download/{value}":
            raise ValueError("unexpected bottle hosting URL")
        if info["bottle"].get("rebuild", 0) != 0:
            raise ValueError("unexpected bottle rebuild")
        tags = info["bottle"]["tags"]
        if len(tags) != 1:
            raise ValueError("unexpected platform count")
        for tag, details in tags.items():
            if tag not in expected or tag in found:
                raise ValueError("unexpected or duplicate bottle platform")
            found.add(tag)
            local = details["local_filename"]
            public = urllib.parse.unquote(details["filename"])
            if local != f"{FORMULA}--{value}.{tag}.bottle.tar.gz" or public != f"{FORMULA}-{value}.{tag}.bottle.tar.gz":
                raise ValueError("unexpected bottle asset filename")
            if digest(directory / local) != details["sha256"]:
                raise ValueError("bottle checksum mismatch")
            assets.append((directory / local, public))
    if found != expected:
        raise ValueError("incomplete bottle matrix")
    return assets


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    p = sub.add_parser("prepare")
    p.add_argument("--output", type=Path, required=True)
    p.add_argument("--release")
    p = sub.add_parser("verify-source")
    p.add_argument("directory", type=Path)
    p = sub.add_parser("verify-bottles")
    p.add_argument("directory", type=Path)
    p.add_argument("--version", required=True)
    args = parser.parse_args()
    if args.command == "prepare":
        prepare(args.output.resolve(), args.release)
    elif args.command == "verify-source":
        verify_source(args.directory)
    else:
        bottle_assets(args.directory, version(args.version))


if __name__ == "__main__":
    main()
