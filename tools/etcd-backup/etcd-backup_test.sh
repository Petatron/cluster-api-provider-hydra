#!/bin/sh
# Tests for etcd-backup.sh: what pruning deletes and what it must never touch,
# and -- through stub binaries on PATH -- that a failing step deletes nothing.
# Run with `sh tools/etcd-backup/etcd-backup_test.sh`; needs jq.
set -u

here=$(cd "$(dirname "$0")" && pwd)
script="$here/etcd-backup.sh"
ETCD_BACKUP_LIB=1
# shellcheck source=etcd-backup.sh
. "$script"
# The script runs under set -e; the tests must survive a failing case.
set +e

fails=0
pass() { printf 'ok   %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1"; fails=$((fails + 1)); }

# expect NAME WANT GOT
expect() {
	if [ "$2" = "$3" ]; then pass "$1"; else
		fail "$1"
		printf '  want: %s\n  got:  %s\n' "$(printf '%s' "$2" | tr '\n' ' ')" "$(printf '%s' "$3" | tr '\n' ' ')"
	fi
}

# snaps CLUSTER DAY FIRST_HOUR LAST_HOUR -> hourly snapshot names, one per line.
snaps() {
	h=$3
	while [ "$h" -le "$4" ]; do printf '%s-%sT%02d0000Z.db.age\n' "$1" "$2" "$h"; h=$((h + 1)); done
}

# --- is_snapshot_of: exactly our names, nothing else ---------------------------
is_snapshot_of wl0 "wl0-20261016T030000Z.db.age" && pass "own snapshot recognised" || fail "own snapshot recognised"
for other in \
	"wl0-20261016T030000Z.db" \
	".partial-wl0-20261016T030000Z.db.age" \
	"wl0-x-20261016T030000Z.db.age" \
	"xwl0-20261016T030000Z.db.age" \
	"wl0-20261016T0300Z.db.age" \
	"wl0-20261016T030000Z.db.age.bak" \
	"notes.txt"; do
	is_snapshot_of wl0 "$other" && fail "rejects $other" || pass "rejects $other"
done
# A dot in the cluster name is literal, not a wildcard.
is_snapshot_of a.b "aXb-20261016T030000Z.db.age" && fail "dot in name is literal" || pass "dot in name is literal"

# --- prune_plan ------------------------------------------------------------------
in=$(snaps wl0 20261016 0 9)
got=$(printf '%s\n' "$in" | prune_plan wl0 48 14 wl0-20261016T090000Z.db.age)
expect "under the hourly count deletes nothing" "" "$got"

# 3 days x 24 hourly, keep 24 hourly + 2 daily: day 1 goes entirely (its first
# is outside the 2 newest days); day 2 keeps only its first.
in=$( (snaps wl0 20261014 0 23; snaps wl0 20261015 0 23; snaps wl0 20261016 0 23) )
got=$(printf '%s\n' "$in" | prune_plan wl0 24 2 wl0-20261016T230000Z.db.age | sort)
want=$( (snaps wl0 20261014 0 23; snaps wl0 20261015 1 23) | sort)
expect "keeps newest 24 hourly plus first-of-day for 2 days" "$want" "$got"

got=$(printf '%s\n' "$in" | prune_plan wl0 24 0 wl0-20261016T230000Z.db.age | sort)
want=$( (snaps wl0 20261014 0 23; snaps wl0 20261015 0 23) | sort)
expect "KEEP_DAILY=0 keeps only the hourly window" "$want" "$got"

# The snapshot just written survives even when it is not newest by name.
in=$( (snaps wl0 20261016 0 23; printf 'wl0-20261015T120000Z.db.age\n') )
got=$(printf '%s\n' "$in" | prune_plan wl0 3 0 wl0-20261015T120000Z.db.age)
case $got in *wl0-20261015T120000Z.db.age*) fail "never plans deleting the snapshot just written" ;; *) pass "never plans deleting the snapshot just written" ;; esac

in=$( (snaps wl0 20261016 0 23; printf '%s\n' notes.txt .partial-wl0-20261016T230000Z.db.age wl0-x-20261001T000000Z.db.age other-20261001T000000Z.db.age) )
got=$(printf '%s\n' "$in" | prune_plan wl0 1 0 wl0-20261016T230000Z.db.age)
case $got in *notes.txt* | *.partial-* | *wl0-x-* | *other-*) fail "never touches foreign or partial files" ;; *) pass "never touches foreign or partial files" ;; esac
expect "deletes all but the newest with KEEP_HOURLY=1" "$(snaps wl0 20261016 0 22)" "$(printf '%s\n' "$got" | sort)"

got=$(printf '' | prune_plan wl0 48 14 wl0-20261016T230000Z.db.age)
expect "empty directory deletes nothing" "" "$got"

for bad in "0 14" "x 14" "-1 14" "48 x" "48 -1"; do
	set -- $bad
	if (printf 'wl0-20261016T000000Z.db.age\n' | prune_plan wl0 "$1" "$2" wl0-20261016T000000Z.db.age) >/dev/null 2>&1; then
		fail "refuses KEEP_HOURLY=$1 KEEP_DAILY=$2"
	else
		pass "refuses KEEP_HOURLY=$1 KEEP_DAILY=$2"
	fi
done

# --- check_prune_plan: the guard in front of rm -----------------------------------
listing=$( (snaps wl0 20261016 0 9; printf 'notes.txt\n') )
new=wl0-20261016T090000Z.db.age
check_prune_plan wl0 "$new" 3 "$listing" "$(snaps wl0 20261016 0 6)" 2>/dev/null &&
	pass "guard accepts a sane plan" || fail "guard accepts a sane plan"
for case in "the new backup|$new" "a foreign file|notes.txt" "a file not in the listing|wl0-20261001T000000Z.db.age" \
	"too much (leaves 2 of 10, keep 3)|$(snaps wl0 20261016 0 7)"; do
	label=${case%%|*}
	plan=${case#*|}
	check_prune_plan wl0 "$new" 3 "$listing" "$plan" 2>/dev/null &&
		fail "guard refuses deleting $label" || pass "guard refuses deleting $label"
done

# --- backup end to end, with stubs --------------------------------------------------
t=$(mktemp -d)
trap 'rm -rf "$t"' EXIT
mkdir -p "$t/bin" "$t/sa" "$t/certs" "$t/work"
printf 'tok\n' >"$t/sa/token"
printf 'ca\n' >"$t/sa/ca.crt"
printf '# test\nage1test\n' >"$t/recipients.txt"

# curl: a Machine being deleted first, then two live control-plane Machines,
# the first listing an IPv6 address before its node IP. Order matters: the
# filters are only proven if what they drop would otherwise be tried first.
cat >"$t/bin/curl" <<'EOF'
#!/bin/sh
cat <<'JSON'
{"items":[
 {"metadata":{"deletionTimestamp":"x"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.0.9"}]}},
 {"metadata":{},"status":{"addresses":[{"type":"InternalIP","address":"fd00::1"},{"type":"InternalIP","address":"10.0.0.1"},{"type":"Hostname","address":"cp-a"}]}},
 {"metadata":{},"status":{"addresses":[{"type":"InternalIP","address":"10.0.0.2"}]}}
]}
JSON
EOF
# etcdctl: `endpoint health` passes only for endpoints in $HEALTHY. A snapshot
# succeeds from ANY endpoint -- as from a real member cut off from quorum, which
# still serves its own stale data. Only the health check keeps it out.
cat >"$t/bin/etcdctl" <<'EOF'
#!/bin/sh
ep=""; out=""; cmd=""
while [ $# -gt 0 ]; do
	case $1 in
	--endpoints) ep=$2; shift ;;
	endpoint) cmd=health ;;
	snapshot) cmd=save; out=$3; shift 2 ;;
	esac
	shift
done
echo "$cmd $ep" >>"$CALLS"
if [ "$cmd" = health ]; then
	case " $HEALTHY " in *" $ep "*) exit 0 ;; *) echo "unhealthy $ep" >&2; exit 1 ;; esac
fi
head -c "${SNAP_BYTES:-2000000}" /dev/zero >"$out"
EOF
cat >"$t/bin/etcdutl" <<'EOF'
#!/bin/sh
[ -z "${ETCDUTL_FAIL:-}" ] || exit 1
echo '{"hash":1,"revision":42,"totalKey":"'"${KEYS:-7}"'","totalSize":2000000}'
EOF
cat >"$t/bin/age" <<'EOF'
#!/bin/sh
[ -z "${AGE_FAIL:-}" ] || exit 1
out=""; while [ $# -gt 0 ]; do case $1 in -o) out=$2; shift ;; esac; in=$1; shift; done
cp "$in" "$out"
EOF
chmod +x "$t/bin/"*

# run_backup DIR [VAR=VALUE ...] -> runs the real script against the stubs.
run_backup() {
	_dir=$1; shift
	: >"$t/calls"
	env PATH="$t/bin:$PATH" MODE=backup CLUSTER_NAME=wl0 CLUSTER_NAMESPACE=default \
		SA_DIR="$t/sa" CERT_DIR="$t/certs" WORK_DIR="$t/work" BACKUP_DIR="$_dir" \
		AGE_RECIPIENTS_FILE="$t/recipients.txt" KEEP_HOURLY=5 KEEP_DAILY=2 \
		CALLS="$t/calls" HEALTHY="https://10.0.0.1:2379 https://10.0.0.2:2379" "$@" \
		sh "$script" 2>"$t/log"
}

# seed DIR -> 8 old snapshots and an operator file.
seed() { rm -rf "$1"; mkdir -p "$1"; for f in $(snaps wl0 20200101 0 7) notes.txt; do : >"$1/$f"; done; }
count() { ls -1A "$1" | snapshots_of wl0 | awk 'NF' | wc -l | tr -d ' '; }

d="$t/b1"; seed "$d"
run_backup "$d" && pass "a clean run succeeds" || { fail "a clean run succeeds"; cat "$t/log"; }
# 5 newest (the new one + 4 old) and the first of each of the 2 days (the
# new one, and 2020-01-01T00).
expect "a clean run keeps 5 newest + first-of-day for 2 days" "6" "$(count "$d")"
[ -e "$d/notes.txt" ] && pass "a clean run keeps the operator's file" || fail "a clean run keeps the operator's file"
ls -1A "$d" | grep -q '^\.partial-' && fail "a clean run leaves no partial" || pass "a clean run leaves no partial"
[ -z "$(ls -A "$t/work")" ] && pass "no plaintext left in WORK_DIR" || fail "no plaintext left in WORK_DIR"
grep -q 'fd00' "$t/calls" && fail "skips IPv6 addresses" || pass "skips IPv6 addresses"
grep -q '10.0.0.9' "$t/calls" && fail "skips a Machine being deleted" || pass "skips a Machine being deleted"

for failure in "ETCDUTL_FAIL=1|etcdutl fails" "AGE_FAIL=1|age fails" "KEYS=0|the snapshot has no keys" \
	"SNAP_BYTES=10|the snapshot is under the floor" "HEALTHY=none|no member is healthy"; do
	var=${failure%%|*}
	label=${failure#*|}
	d="$t/f"; seed "$d"
	if run_backup "$d" "$var"; then fail "fails when $label"; else pass "fails when $label"; fi
	expect "deletes nothing when $label" "8" "$(count "$d")"
	ls -1A "$d" | grep -q '^\.partial-' && fail "no partial left when $label" || pass "no partial left when $label"
done

# The first member is unhealthy (cut off from quorum): use the second.
d="$t/b2"; seed "$d"
run_backup "$d" HEALTHY="https://10.0.0.2:2379" && pass "skips an unhealthy member" || { fail "skips an unhealthy member"; cat "$t/log"; }
grep -q 'from https://10.0.0.2:2379' "$t/log" && pass "snapshots the healthy member" || fail "snapshots the healthy member"

# A future-dated snapshot: write the backup, prune nothing, fail loudly.
d="$t/b3"; seed "$d"; : >"$d/wl0-29991231T000000Z.db.age"
if run_backup "$d"; then fail "fails when a snapshot is dated in the future"; else pass "fails when a snapshot is dated in the future"; fi
expect "prunes nothing when a snapshot is dated in the future" "10" "$(count "$d")"

# A stale partial from a SIGKILLed run is cleaned up; another cluster's is not.
d="$t/b4"; seed "$d"; : >"$d/.partial-wl0-20200101T000000Z.db.age"; : >"$d/.partial-other-20200101T000000Z.db.age"
run_backup "$d" >/dev/null
[ -e "$d/.partial-wl0-20200101T000000Z.db.age" ] && fail "removes its own stale partial" || pass "removes its own stale partial"
[ -e "$d/.partial-other-20200101T000000Z.db.age" ] && pass "leaves another cluster's partial" || fail "leaves another cluster's partial"

echo
if [ "$fails" -ne 0 ]; then echo "$fails test(s) failed"; exit 1; fi
echo "all tests passed"
