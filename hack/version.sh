#!/usr/bin/env bash
# Release version management (PET-92).
#
# The repository root holds a VERSION file with the release that main is at,
# e.g. "0.3.0" (no leading v). A PR that changes it is a release request: when
# it merges, the release workflow tags that commit vX.Y.Z and publishes.
#
#   hack/version.sh get             print VERSION
#   hack/version.sh bump <part>     part = major | minor | patch; rewrite VERSION
#                                   (and, where metadata.yaml exists, add its
#                                   clusterctl releaseSeries entry)
#   hack/version.sh check           fail unless VERSION is well formed, not behind
#                                   the newest v* tag, and (where metadata.yaml
#                                   exists) covered by a releaseSeries entry
#
# Pre-releases are written by hand as X.Y.Z-rc.N. They sort below X.Y.Z.
#
# This file is identical in Petatron/hydra and Petatron/cluster-api-provider-hydra;
# change both together.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

VERSION_FILE=VERSION
METADATA=metadata.yaml
SEMVER_RE='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.(0|[1-9][0-9]*))?$'

die() { echo "version: $*" >&2; exit 1; }

read_version() {
	[ -f "$VERSION_FILE" ] || die "$VERSION_FILE is missing"
	local v
	# Exactly one version, optionally followed by newlines ($(...) drops
	# those). Any other whitespace, a CR or a second line is malformed: a
	# release request must not pass CI on a value it does not literally hold.
	v="$(cat "$VERSION_FILE")"
	[[ "$v" =~ $SEMVER_RE ]] || die "$VERSION_FILE holds '$v'; want X.Y.Z or X.Y.Z-rc.N alone on one line, no leading v"
	printf '%s\n' "$v"
}

# Prints a sortable key: major minor patch, then 1 for a release or 0 plus the
# rc number for a pre-release, so X.Y.Z-rc.N < X.Y.Z.
sort_key() {
	[[ "$1" =~ $SEMVER_RE ]] || return 1
	if [ -n "${BASH_REMATCH[4]}" ]; then
		printf '%d %d %d 0 %d\n' "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" "${BASH_REMATCH[3]}" "${BASH_REMATCH[5]}"
	else
		printf '%d %d %d 1 0\n' "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" "${BASH_REMATCH[3]}"
	fi
}

# Exit 0 when $1 sorts strictly after $2.
newer_than() {
	local a b
	a="$(sort_key "$1")" b="$(sort_key "$2")"
	[ "$a" != "$b" ] || return 1
	[ "$(printf '%s\n%s\n' "$a" "$b" | sort -k1,1n -k2,2n -k3,3n -k4,4n -k5,5n | tail -1)" = "$a" ]
}

# The newest well-formed v* tag, without the v; empty when there is none.
latest_tag() {
	local t best=""
	while IFS= read -r t; do
		t="${t#v}"
		[[ "$t" =~ $SEMVER_RE ]] || continue
		if [ -z "$best" ] || newer_than "$t" "$best"; then best="$t"; fi
	done < <(git tag -l 'v*')
	printf '%s\n' "$best"
}

# "major minor" pairs listed under releaseSeries in metadata.yaml.
series_pairs() {
	awk '
		/^[[:space:]]*-[[:space:]]*major:/ { sub(/.*major:[[:space:]]*/, ""); maj = $0; next }
		/^[[:space:]]*minor:/ && maj != "" { sub(/.*minor:[[:space:]]*/, ""); print maj, $0; maj = "" }
	' "$METADATA"
}

has_series() { series_pairs | grep -qx "$1 $2"; }

add_series() {
	local maj="$1" min="$2" contract
	has_series "$maj" "$min" && return 0
	contract="$(awk '/^[[:space:]]*contract:/ { c = $2 } END { print c }' "$METADATA")"
	[ -n "$contract" ] || die "$METADATA has no releaseSeries entry to copy the contract from"
	printf '  - major: %s\n    minor: %s\n    contract: %s\n' "$maj" "$min" "$contract" >> "$METADATA"
	echo "added releaseSeries $maj.$min (contract $contract) to $METADATA; check the contract is still right"
}

cmd_bump() {
	local part="${1:-}" v maj min pat
	v="$(read_version)"
	[[ "$v" =~ $SEMVER_RE ]]
	maj="${BASH_REMATCH[1]}" min="${BASH_REMATCH[2]}" pat="${BASH_REMATCH[3]}"
	case "$part" in
		major) maj=$((maj + 1)) min=0 pat=0 ;;
		minor) min=$((min + 1)) pat=0 ;;
		patch) pat=$((pat + 1)) ;;
		*) die "bump wants major, minor or patch, not '$part'" ;;
	esac
	# metadata.yaml first: if it cannot take the series, VERSION is left alone.
	if [ -f "$METADATA" ]; then add_series "$maj" "$min"; fi
	printf '%s.%s.%s\n' "$maj" "$min" "$pat" > "$VERSION_FILE"
	echo "VERSION $v -> $maj.$min.$pat"
}

cmd_check() {
	local v latest
	v="$(read_version)"
	latest="$(latest_tag)"
	if [ -n "$latest" ] && newer_than "$latest" "$v"; then
		die "VERSION $v is behind the newest tag v$latest"
	fi
	if [ -f "$METADATA" ]; then
		[[ "$v" =~ $SEMVER_RE ]]
		has_series "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" ||
			die "$METADATA has no releaseSeries entry for ${BASH_REMATCH[1]}.${BASH_REMATCH[2]}; clusterctl would refuse v$v (make bump-minor adds it)"
	fi
	if [ "$v" = "$latest" ]; then
		echo "VERSION $v is already released as v$latest; merging this releases nothing"
	else
		echo "VERSION $v is unreleased (newest tag: ${latest:+v}${latest:-none}); merging to main releases v$v"
	fi
}

case "${1:-}" in
	get) read_version ;;
	bump) shift; cmd_bump "$@" ;;
	check) cmd_check ;;
	*) die "usage: $0 get | bump <major|minor|patch> | check" ;;
esac
