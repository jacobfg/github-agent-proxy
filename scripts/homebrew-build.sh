#!/usr/bin/env bash
# Run on a fresh build runner. Uses a local checkout as the tap, without a PAT.
set -euo pipefail
source_dir="$1"
bottle_dir="$2"
repo_dir="$(pwd)"
formula=jacobfg/github-agent-proxy/github-agent-proxy
export HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_INSTALL_CLEANUP=1
python3 scripts/homebrew_release.py verify-source "$source_dir"
if brew list --versions github-agent-proxy >/dev/null 2>&1; then
  printf 'github-agent-proxy is already installed; use a clean build runner.\n' >&2
  exit 1
fi
brew tap jacobfg/github-agent-proxy "$repo_dir"
tap_dir="$(brew --repository jacobfg/github-agent-proxy)"
# Keep the public tap identity in bottle receipts, not the CI checkout path.
git -C "$tap_dir" remote set-url origin https://github.com/jacobfg/github-agent-proxy
if brew help trust >/dev/null 2>&1; then
  brew trust --formula "$formula"
fi
mkdir -p "$tap_dir/Formula" "$bottle_dir"
cp "$source_dir/github-agent-proxy.rb" "$tap_dir/Formula/github-agent-proxy.rb"
# Seed the cache with the exact archive destined for the release. This allows
# build/test/bottle verification to finish before publishing any release assets.
archive="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["source"])' "$source_dir/source.json")"
version="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["version"])' "$source_dir/source.json")"
cache="$(brew --cache --build-from-source "$formula")"
mkdir -p "$(dirname "$cache")"
cp "$source_dir/$archive" "$cache"
brew style "$formula"
brew install --build-bottle --verbose "$formula"
brew test --verbose "$formula"
(
  cd "$bottle_dir"
  brew bottle --json --no-rebuild \
    --root-url="https://github.com/jacobfg/github-agent-proxy/releases/download/$version" "$formula"
)
# Prove that the produced bottle can be poured and tested, not just compiled.
brew uninstall "$formula"
bottles=("$bottle_dir"/*.bottle.tar.gz)
test "${#bottles[@]}" -eq 1
brew install "${bottles[0]}"
brew test --verbose "$formula"
