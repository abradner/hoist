#!/usr/bin/env bash
# Fail if anything that looks like a private address, internal hostname, or a real
# kube context leaks into tracked files. Public repo: see AGENTS.md §4.
set -euo pipefail
cd "$(dirname "$0")/.."

# Patterns are deliberately broad; a false positive costs a placeholder rename, a miss costs
# a leak that lives in history forever. Add a pattern here the first time one gets past review.
patterns=(
  '\b10\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\b'
  '\b192\.168\.[0-9]{1,3}\.[0-9]{1,3}\b'
  '\b172\.(1[6-9]|2[0-9]|3[01])\.[0-9]{1,3}\.[0-9]{1,3}\b'
  '\.(local|lan|internal|home\.arpa)\b'
  '\.asn\.casa\b'
  'admin@athena'
)
# Known-benign matches (file names, XDG paths). Keep this list short and specific.
# `\.local"[[:space:]]*,[[:space:]]*"state` matches the split filepath.Join(home, ".local",
# "state", ...) shape internal/engine/state.go and its test use for the real XDG state-dir
# default ($XDG_STATE_HOME, else ~/.local/state) — not a bare ".local"-terminated string, so
# it does not also swallow a genuine internal .local hostname written some other way.
allow='settings\.local\.json|\.local/share|\.local/bin|\.local/state|\.local"[[:space:]]*,[[:space:]]*"state'
status=0
for p in "${patterns[@]}"; do
  if hits=$(git ls-files -z | xargs -0 grep -nE -- "$p" 2>/dev/null | grep -v '^scripts/public-safety.sh:' | grep -vE -- "$allow"); then
    echo "public-safety: pattern '$p' matched:" >&2
    echo "$hits" >&2
    status=1
  fi
done
exit $status
