#!/usr/bin/env bash
# Regression test for scripts/public-safety.sh's allow-list, AGENTS.md §4.4. Runs the real
# script against disposable temp git repos rather than modelling its regex in a second
# language — the script itself is the thing under test.
#
# (a) a repo containing only the genuine XDG state-dir occurrences from
#     internal/engine/state.go / state_test.go must pass (exit 0) — this is the regression
#     case: an earlier, over-broad bare `\.local/state` allow pattern also caught this, but so
#     would an allow-list that was too broad in the OTHER direction and let anything through.
# (b) a repo containing a genuine internal-hostname-shaped leak — a hostname immediately
#     followed by ".local/state" (built at runtime below, never spelled out contiguously in
#     this file's own source, or this script would flag itself) — must still be flagged
#     (non-zero exit, output naming the match). This is exactly what the earlier bare
#     `\.local/state` pattern failed to catch.
set -euo pipefail
cd "$(dirname "$0")/.."
script="$(pwd)/scripts/public-safety.sh"

fail=0

make_repo() {
	local dir="$1"
	mkdir -p "$dir/scripts"
	cp "$script" "$dir/scripts/public-safety.sh"
	git -C "$dir" init -q -b main
	git -C "$dir" config user.email test@example.invalid
	git -C "$dir" config user.name Test
	git -C "$dir" config commit.gpgsign false
}

commit_all() {
	local dir="$1"
	git -C "$dir" add -A
	git -C "$dir" commit -q -m test
}

# (a) real occurrences must be allowed.
allowed=$(mktemp -d)
flagged=$(mktemp -d)
allowed_out=$(mktemp)
flagged_out=$(mktemp)
trap 'rm -rf "$allowed" "$flagged" "$allowed_out" "$flagged_out"' EXIT
make_repo "$allowed"
cat >"$allowed/state.go" <<'EOF'
package engine

// StateDir is $XDG_STATE_HOME/hoist, else ~/.local/state/hoist — the XDG rule on every
// platform, never ~/Library.
func StateDir() (string, error) {
	return filepath.Join(home, ".local", "state", "hoist"), nil
}
EOF
cat >"$allowed/state_test.go" <<'EOF'
package engine

func TestStateDir(t *testing.T) {
	if !strings.Contains(got, filepath.Join(".local", "state", "hoist")) {
		t.Fatalf("StateDir() = %q, want it to end in .local/state/hoist", got)
	}
}
EOF
commit_all "$allowed"
if ! (cd "$allowed" && ./scripts/public-safety.sh) >"$allowed_out" 2>&1; then
	echo "FAIL: real .local/state occurrences were flagged (should be allowed):" >&2
	cat "$allowed_out" >&2
	fail=1
else
	echo "PASS: real .local/state occurrences (state.go, state_test.go) allowed"
fi

# (b) a genuine leak: a hostname immediately followed by ".local/state" must still be
# flagged. Built via concatenation so the literal shape never appears contiguously in this
# tracked file's own source (it would otherwise trip the real scanner on this very file).
make_repo "$flagged"
host1=foo
host2=something
leak1="${host1}$(printf '%s' '.local/state')"
leak2="${host2}$(printf '%s' '.local/state/path')"
{
	printf 'internal cluster host: %s\n' "$leak1"
	printf 'another one: %s\n' "$leak2"
} >"$flagged/leak.txt"
commit_all "$flagged"
if (cd "$flagged" && ./scripts/public-safety.sh) >"$flagged_out" 2>&1; then
	echo "FAIL: the synthetic (host).local/state leaks were NOT flagged (allow-list too broad):" >&2
	cat "$flagged_out" >&2
	fail=1
else
	if grep -qF "$leak1" "$flagged_out" && grep -qF "$leak2" "$flagged_out"; then
		echo "PASS: the synthetic (host).local/state leaks are flagged"
	else
		echo "FAIL: script exited nonzero but did not name the expected leaks:" >&2
		cat "$flagged_out" >&2
		fail=1
	fi
fi

exit $fail
