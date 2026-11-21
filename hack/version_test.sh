#!/usr/bin/env bash
# Tests for hack/version.sh (PET-92): parsing, ordering against tags, bumping,
# and metadata.yaml releaseSeries handling. Each case runs the real script in a
# throwaway git repository with its own tags, so nothing here touches this
# checkout. Run with `make test-version` or `bash hack/version_test.sh`.
#
# This file is identical in Petatron/hydra and Petatron/cluster-api-provider-hydra;
# change both together.
set -u

here="$(cd "$(dirname "$0")" && pwd)"
script="$here/version.sh"

fails=0
pass() { printf 'ok   %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1"; fails=$((fails + 1)); }

sandbox=""
cleanup() { if [ -n "$sandbox" ] && [ -d "$sandbox" ]; then rm -rf -- "$sandbox"; fi; }
trap cleanup EXIT

# new_repo [TAG...]: a fresh repository holding a copy of the script, with an
# annotated tag on its only commit for each TAG.
new_repo() {
	cleanup
	sandbox="$(mktemp -d)"
	mkdir "$sandbox/hack"
	cp "$script" "$sandbox/hack/version.sh"
	(
		cd "$sandbox" &&
			git init -q &&
			git -c user.email=t@t -c user.name=t commit -q --allow-empty -m init &&
			for t in "$@"; do git -c user.email=t@t -c user.name=t tag -a "$t" -m "$t"; done
	)
}

# run ARGS...: run the script in the sandbox; sets $rc and $out (stdout+stderr).
run() {
	out="$(cd "$sandbox" && bash hack/version.sh "$@" 2>&1)"
	rc=$?
}

# expect_rc NAME WANT_RC [GREP]: assert the last run's exit code, and that its
# output matches GREP (extended regex) when given.
expect_rc() {
	if [ "$rc" -ne "$2" ]; then
		fail "$1 (exit $rc, want $2): $out"
	elif [ -n "${3:-}" ] && ! printf '%s' "$out" | grep -qE "$3"; then
		fail "$1 (output does not match /$3/): $out"
	else
		pass "$1"
	fi
}

expect_version() {
	local got
	got="$(cat "$sandbox/VERSION")"
	if [ "$got" = "$2" ]; then pass "$1"; else fail "$1 (VERSION is '$got', want '$2')"; fi
}

series_count() { grep -cE "^[[:space:]]*minor:[[:space:]]*$1\$" "$sandbox/metadata.yaml"; }

# --- reading VERSION ---------------------------------------------------------

new_repo
printf '0.3.0\n' > "$sandbox/VERSION"; run get; expect_rc "get: trailing newline" 0 '^0\.3\.0$'
printf '0.3.0' > "$sandbox/VERSION"; run get; expect_rc "get: no trailing newline" 0 '^0\.3\.0$'
printf '0.4.0-rc.2\n' > "$sandbox/VERSION"; run get; expect_rc "get: rc" 0 '^0\.4\.0-rc\.2$'

for bad in 'v0.3.0' '0 . 3 . 0' ' 0.3.0' '0.3.0 ' '0.3' '0.3.0.1' '01.2.3' '0.3.0-beta.1' \
	'0.3.0-rc.01' '0.3.0-rc' "0.3.0\n0.4.0" "0.3.0\r" ''; do
	printf '%b\n' "$bad" > "$sandbox/VERSION"
	run get
	expect_rc "get rejects '$(printf '%s' "$bad" | tr '\r\n' 'RN')'" 1 'want X\.Y\.Z'
done

rm -f "$sandbox/VERSION"; run get; expect_rc "get: missing VERSION" 1 'missing'

# --- check against tags --------------------------------------------------------

new_repo
echo 0.1.0 > "$sandbox/VERSION"; run check; expect_rc "check: no tags yet" 0 'unreleased .*none'

new_repo v0.1.0 v0.2.0
echo 0.2.0 > "$sandbox/VERSION"; run check; expect_rc "check: equal to newest tag" 0 'already released'
echo 0.2.1 > "$sandbox/VERSION"; run check; expect_rc "check: ahead of newest tag" 0 'releases v0\.2\.1'
echo 0.1.9 > "$sandbox/VERSION"; run check; expect_rc "check: behind newest tag" 1 'behind the newest tag v0\.2\.0'
echo 0.1.0 > "$sandbox/VERSION"; run check; expect_rc "check: equal to an older tag" 1 'behind'

new_repo v0.2.0 v-latest vfoo v1.2 v9.9.9.9
echo 0.2.1 > "$sandbox/VERSION"; run check; expect_rc "check: ignores non-semver tags" 0 'newest tag: v0\.2\.0'

new_repo v0.10.0 v0.9.0
echo 0.9.5 > "$sandbox/VERSION"; run check; expect_rc "check: numeric not lexical (0.10 > 0.9)" 1 'v0\.10\.0'

# --- pre-release ordering -----------------------------------------------------

new_repo v0.3.0 v0.4.0-rc.2 v0.4.0-rc.10
echo 0.4.0-rc.9 > "$sandbox/VERSION"; run check; expect_rc "rc: rc.9 < rc.10" 1 'behind the newest tag v0\.4\.0-rc\.10'
echo 0.4.0-rc.11 > "$sandbox/VERSION"; run check; expect_rc "rc: rc.11 > rc.10" 0 'releases v0\.4\.0-rc\.11'
echo 0.4.0 > "$sandbox/VERSION"; run check; expect_rc "rc: final > its rcs" 0 'releases v0\.4\.0'

new_repo v0.4.0
echo 0.4.0-rc.12 > "$sandbox/VERSION"; run check; expect_rc "rc: rc < its final" 1 'behind'
echo 0.4.1-rc.1 > "$sandbox/VERSION"; run check; expect_rc "rc: next patch's rc > previous final" 0 'releases'

# --- bumping -------------------------------------------------------------------

new_repo
echo 0.3.4 > "$sandbox/VERSION"; run bump patch; expect_rc "bump patch" 0; expect_version "bump patch result" 0.3.5
echo 0.3.4 > "$sandbox/VERSION"; run bump minor; expect_rc "bump minor" 0; expect_version "bump minor result" 0.4.0
echo 0.3.4 > "$sandbox/VERSION"; run bump major; expect_rc "bump major" 0; expect_version "bump major result" 1.0.0
echo 0.4.0-rc.2 > "$sandbox/VERSION"; run bump patch; expect_rc "bump drops rc" 0; expect_version "bump drops rc result" 0.4.1
echo 0.3.4 > "$sandbox/VERSION"; run bump nope; expect_rc "bump rejects unknown part" 1 'major, minor or patch'
expect_version "bump nope leaves VERSION" 0.3.4
run bump; expect_rc "bump rejects missing part" 1 'major, minor or patch'
echo v0.3.4 > "$sandbox/VERSION"; run bump patch; expect_rc "bump refuses malformed VERSION" 1 'want X\.Y\.Z'
expect_version "bump on malformed leaves VERSION" v0.3.4
if [ -e "$sandbox/metadata.yaml" ]; then fail "bump without metadata.yaml created one"; else pass "bump without metadata.yaml leaves it absent"; fi

# --- metadata.yaml releaseSeries ---------------------------------------------------

write_metadata() {
	cat > "$sandbox/metadata.yaml" <<'YAML'
# comment
apiVersion: clusterctl.cluster.x-k8s.io/v1alpha3
kind: Metadata
releaseSeries:
  - major: 0
    minor: 1
    contract: v1beta1
  - major: 0
    minor: 2
    contract: v1beta2
YAML
}

new_repo v0.2.0
write_metadata
echo 0.2.0 > "$sandbox/VERSION"; run check; expect_rc "metadata: covered series passes" 0 'already released'
echo 0.2.1 > "$sandbox/VERSION"; run bump patch; expect_rc "metadata: bump patch" 0
if [ "$(series_count 2)" = 1 ] && [ "$(series_count 3)" = 0 ]; then
	pass "metadata: bump patch adds no series"
else
	fail "metadata: bump patch changed releaseSeries: $(cat "$sandbox/metadata.yaml")"
fi

echo 0.2.0 > "$sandbox/VERSION"; run bump minor; expect_rc "metadata: bump minor" 0 'added releaseSeries 0\.3 \(contract v1beta2\)'
if [ "$(series_count 3)" = 1 ] && [ "$(tail -3 "$sandbox/metadata.yaml")" = "$(printf '  - major: 0\n    minor: 3\n    contract: v1beta2')" ]; then
	pass "metadata: bump minor appends 0.3 with the last contract"
else
	fail "metadata: bump minor wrote: $(cat "$sandbox/metadata.yaml")"
fi
run check; expect_rc "metadata: bumped version passes check" 0 'releases v0\.3\.0'

echo 0.2.9 > "$sandbox/VERSION"; run bump minor; expect_rc "metadata: existing series not re-added" 0
if [ "$(series_count 3)" = 1 ]; then pass "metadata: no duplicate series"; else fail "metadata: duplicated 0.3: $(cat "$sandbox/metadata.yaml")"; fi

echo 0.3.0 > "$sandbox/VERSION"; run bump major; expect_rc "metadata: bump major" 0 'added releaseSeries 1\.0'
run check; expect_rc "metadata: major series passes check" 0 'releases v1\.0\.0'

new_repo v0.2.0
write_metadata
echo 0.5.0 > "$sandbox/VERSION"; run check; expect_rc "metadata: missing series fails check" 1 'no releaseSeries entry for 0\.5'

new_repo v0.2.0
printf 'releaseSeries:\n  - major: 0\n    minor: 2\n    contract: v1beta1\n  - major: 0\n    minor: 3\n    contract: v1beta2\n' > "$sandbox/metadata.yaml"
echo 0.3.0 > "$sandbox/VERSION"; run bump minor; expect_rc "metadata: contract copied from the last entry" 0 'contract v1beta2'

new_repo v0.2.0
printf 'releaseSeries: []\n' > "$sandbox/metadata.yaml"
echo 0.2.0 > "$sandbox/VERSION"; run bump minor; expect_rc "metadata: no entry to copy a contract from" 1 'no releaseSeries entry to copy'
expect_version "metadata: failed series add leaves VERSION" 0.2.0

# --- tag-ok -------------------------------------------------------------------------

new_repo
for good in v0.3.0 v10.20.30 v0.4.0-rc.1 v1.0.0-rc.12; do
	run tag-ok "$good"; expect_rc "tag-ok accepts $good" 0
done
# shellcheck disable=SC2016 # literal $(...) and backticks are the point: they must be refused
for bad in '0.3.0' 'v0.3' 'v01.2.3' 'v0.3.0-beta.1' 'v0.3.0;id' 'v0.3.0$(id)' 'v0.3.0 x' "v0.3.0'" 'v0.3.0`id`' ''; do
	run tag-ok "$bad"; expect_rc "tag-ok rejects '$bad'" 1 'is not vX\.Y\.Z'
done

# --- release-state, against a stub gh on PATH --------------------------------------

stub="$(mktemp -d)"
cat > "$stub/gh" <<'STUB'
#!/usr/bin/env bash
# Answers `gh api repos/<repo>/releases/tags/<tag>` as GH_STUB says.
printf '%s\n' "$*" >> "$GH_STUB_LOG"
case "$GH_STUB" in
	present) echo '{"tag_name":"x"}' ;;
	absent) echo 'gh: Not Found (HTTP 404)' >&2; exit 1 ;;
	error) echo 'gh: Server Error (HTTP 502)' >&2; exit 1 ;;
	denied) echo 'gh: Resource not accessible by integration (HTTP 403)' >&2; exit 1 ;;
	offline) echo 'error connecting to api.github.com' >&2; exit 1 ;;
esac
STUB
chmod +x "$stub/gh"
state() { GH_STUB="$1" GH_STUB_LOG="$stub/log" GITHUB_REPOSITORY="${3-o/r}" PATH="$stub:$PATH" run release-state "$2"; }

new_repo
state present v0.3.0; expect_rc "release-state: present" 0 '^present$'
state absent v0.3.0; expect_rc "release-state: 404 is absent" 0 '^absent$'
state error v0.3.0; expect_rc "release-state: 5xx fails, not absent" 1 'cannot tell.*HTTP 502'
state denied v0.3.0; expect_rc "release-state: 403 fails, not absent" 1 'cannot tell.*HTTP 403'
state offline v0.3.0; expect_rc "release-state: network error fails" 1 'cannot tell'
state absent 'v0.3.0;id'; expect_rc "release-state: refuses a malformed tag" 1 'is not vX\.Y\.Z'
state absent v0.3.0 ''; expect_rc "release-state: needs GITHUB_REPOSITORY" 1 'needs GITHUB_REPOSITORY'
if grep -qx 'api repos/o/r/releases/tags/v0.3.0' "$stub/log" && ! grep -q ';id' "$stub/log"; then
	pass "release-state: asks for the right endpoint, never with a bad tag"
else
	fail "release-state: gh was called as: $(cat "$stub/log")"
fi
rm -rf -- "$stub"

# --- usage -------------------------------------------------------------------------

new_repo
run; expect_rc "usage with no command" 1 'usage'
run frobnicate; expect_rc "usage with unknown command" 1 'usage'

echo
if [ "$fails" -ne 0 ]; then
	echo "$fails test(s) failed"
	exit 1
fi
echo "all version.sh tests passed"
