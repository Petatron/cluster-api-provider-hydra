#!/bin/sh
# etcd-backup -- take one verified, encrypted etcd snapshot of a Cluster API
# workload cluster, from its management cluster, then prune old snapshots.
#
# It runs in the management cluster on purpose (PET-23). The etcd CA a client
# certificate is signed with already lives there, put there by Cluster API, and
# a backup taken from the outside survives the cluster it protects: a workload
# cluster that is broken cannot be relied on to back itself up.
#
# MODE=backup (the default), one run:
#   1. find the cluster's control-plane Machines and their IPv4 InternalIPs
#   2. use the first member that answers a linearizable read -- a member cut
#      off from quorum would still hand over a snapshot, of stale data. A read,
#      not `endpoint health`: that also fails on any alarm, and under NOSPACE,
#      exactly when a fresh snapshot is wanted before compacting, it would
#      refuse a member that can serve one perfectly well
#   3. snapshot it and verify the snapshot (etcdutl status, size floor, keys)
#   4. encrypt it to the age recipients, write it, and only then
#   5. prune old snapshots of this cluster, after checking the plan is sane
#
# MODE=check-cert: fail when the client certificate expires within
# CERT_WARN_DAYS. Run on its own schedule, so an expiring certificate has its
# own alert instead of masking real backup failures under one that is already
# firing.
#
# The plaintext snapshot exists only in WORK_DIR, which the CronJob backs with
# memory. It holds every Secret in the cluster, so it never touches a disk.
set -eu
# pipefail where the shell has it (busybox ash in the image does); dash, which
# CI tests under, does not -- every pipeline that matters is checked explicitly.
if (set -o pipefail) 2>/dev/null; then set -o pipefail; fi

MODE=${MODE:-backup}
CLUSTER_NAME=${CLUSTER_NAME:-}
CLUSTER_NAMESPACE=${CLUSTER_NAMESPACE:-default}
AGE_RECIPIENTS_FILE=${AGE_RECIPIENTS_FILE:-/etc/etcd-backup/age/recipients.txt}
CERT_DIR=${CERT_DIR:-/etc/etcd-backup/client}
BACKUP_DIR=${BACKUP_DIR:-/backup}
WORK_DIR=${WORK_DIR:-/work}
KEEP_HOURLY=${KEEP_HOURLY:-48}
KEEP_DAILY=${KEEP_DAILY:-14}
MIN_BYTES=${MIN_BYTES:-1048576}
CERT_WARN_DAYS=${CERT_WARN_DAYS:-30}
KUBE_API=${KUBE_API:-https://kubernetes.default.svc}
# Separate on purpose: --command-timeout bounds the whole snapshot stream, so a
# value short enough to give up quickly on a dead address would cap the size of
# database that can be backed up.
HEALTH_TIMEOUT=${HEALTH_TIMEOUT:-5s}
SNAPSHOT_TIMEOUT=${SNAPSHOT_TIMEOUT:-600s}
SA_DIR=${SA_DIR:-/var/run/secrets/kubernetes.io/serviceaccount}

log() { printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }

is_count() {
	case $1 in '' | *[!0-9]*) return 1 ;; esac
}

# snapshot_name CLUSTER STAMP -> the one file name this tool writes.
snapshot_name() { printf '%s-%s.db.age\n' "$1" "$2"; }

# is_snapshot_of CLUSTER FILE -> whether FILE is exactly a snapshot this tool
# wrote for CLUSTER. Anything else in the directory -- an operator's file,
# another cluster's snapshots, a partial write -- is never touched by pruning.
# A glob, not a regular expression: a cluster name may contain dots.
is_snapshot_of() {
	case $2 in
	"$1"-[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9][0-9][0-9][0-9][0-9][0-9]Z.db.age) return 0 ;;
	esac
	return 1
}

# snapshots_of CLUSTER -> reads names on stdin, prints this cluster's snapshots.
snapshots_of() {
	while IFS= read -r _f; do
		if is_snapshot_of "$1" "$_f"; then printf '%s\n' "$_f"; fi
	done
}

# prune_plan CLUSTER KEEP_HOURLY KEEP_DAILY JUST_WRITTEN
# Reads file names on stdin and prints, one per line, those to delete.
#
# Kept, as a union:
#   - the KEEP_HOURLY newest snapshots (by count: a retried run adds one)
#   - the first snapshot of each of the KEEP_DAILY newest UTC days
#   - JUST_WRITTEN, unconditionally
# Never considered: anything is_snapshot_of rejects.
#
# One awk program, no sort or head: a helper failing mid-pipeline would
# otherwise shrink the keep list and grow the delete list, and every pipeline
# here ends in a command that would hide that. Names compare as strings, and
# sort chronologically because the stamp is fixed-width UTC. check_prune_plan
# checks the result independently before anything is deleted.
prune_plan() {
	_cluster=$1 _hourly=$2 _daily=$3 _new=$4
	is_count "$_hourly" && [ "$_hourly" -ge 1 ] || die "KEEP_HOURLY must be a whole number >= 1, got '$_hourly'"
	is_count "$_daily" || die "KEEP_DAILY must be a whole number >= 0, got '$_daily'"
	snapshots_of "$_cluster" | awk -v nh="$_hourly" -v nd="$_daily" -v new="$_new" -v plen="${#_cluster}" '
		NF && !($0 in seen) { seen[$0] = 1; name[++n] = $0 }
		END {
			for (i = 1; i <= n; i++) {
				newer = 0
				for (j = 1; j <= n; j++) if (name[j] "" > name[i] "") newer++
				if (newer < nh + 0) keep[name[i]] = 1
				d = substr(name[i], plen + 2, 8) ""
				if (!(d in first) || name[i] "" < first[d] "") first[d] = name[i]
			}
			for (d in first) {
				newer = 0
				for (e in first) if (e "" > d "") newer++
				if (newer < nd + 0) keep[first[d]] = 1
			}
			keep[new] = 1
			for (i = 1; i <= n; i++) if (!(name[i] in keep)) print name[i]
		}'
}

# check_prune_plan CLUSTER JUST_WRITTEN KEEP_HOURLY LISTING DELETES
# The last line of defence before rm: refuses a plan that deletes the backup
# just written, anything that is not this cluster's snapshot, anything not in
# the listing, or so much that fewer than KEEP_HOURLY snapshots would remain.
check_prune_plan() {
	_cluster=$1 _new=$2 _hourly=$3 _listing=$4 _deletes=$5
	_mine=$(printf '%s\n' "$_listing" | snapshots_of "$_cluster" | awk 'NF' | wc -l | tr -d ' ')
	_gone=0
	for _f in $_deletes; do
		[ "$_f" != "$_new" ] || { log "prune plan deletes the backup just written"; return 1; }
		is_snapshot_of "$_cluster" "$_f" || { log "prune plan deletes $_f, which is not a $_cluster snapshot"; return 1; }
		printf '%s\n' "$_listing" | grep -Fqx -- "$_f" || { log "prune plan deletes $_f, which is not in the directory"; return 1; }
		_gone=$((_gone + 1))
	done
	_floor=$_hourly
	[ "$_mine" -ge "$_floor" ] || _floor=$_mine
	[ $((_mine - _gone)) -ge "$_floor" ] || { log "prune plan leaves $((_mine - _gone)) of $_mine snapshots, under $_floor"; return 1; }
}

# control_plane_endpoints -> https://IP:2379 for every IPv4 InternalIP of every
# control-plane Machine of the cluster that is not being deleted, in the order
# the Machines list them. That list also carries pod and VIP addresses; they
# fail mutual TLS or the dial, so they cost time, never correctness.
control_plane_endpoints() {
	_sel="cluster.x-k8s.io%2Fcluster-name%3D${CLUSTER_NAME}%2Ccluster.x-k8s.io%2Fcontrol-plane"
	# The bearer token goes in a header file, not argv, where ps would show it.
	printf 'Authorization: Bearer %s\n' "$(cat "$SA_DIR/token")" >"$WORK_DIR/auth"
	_json=$(curl -sS --fail-with-body --max-time 20 --cacert "$SA_DIR/ca.crt" -H "@$WORK_DIR/auth" \
		"$KUBE_API/apis/cluster.x-k8s.io/v1beta2/namespaces/$CLUSTER_NAMESPACE/machines?labelSelector=$_sel" 2>&1) ||
		die "listing Machines of $CLUSTER_NAMESPACE/$CLUSTER_NAME failed: $(printf '%s' "$_json" | tail -c 300)"
	rm -f "$WORK_DIR/auth"
	printf '%s' "$_json" | jq -r '.items[] | select(.metadata.deletionTimestamp == null)
		| .status.addresses[]? | select(.type == "InternalIP") | .address
		| select(test("^[0-9]+\\.[0-9]+\\.[0-9]+\\.[0-9]+$"))
		| "https://\(.):2379"' || die "could not read the Machines list"
}

etcdctl_() {
	etcdctl --dial-timeout=3s \
		--cacert "$CERT_DIR/ca.crt" --cert "$CERT_DIR/tls.crt" --key "$CERT_DIR/tls.key" "$@"
}

# errors_of FILE -> etcdctl's stderr on one line. Its last line is a generic
# "Error: ..." and the cause is on the first, so keep all of it.
errors_of() { tr '\n' ' ' <"$1"; }

check_cert() {
	is_count "$CERT_WARN_DAYS" || die "CERT_WARN_DAYS must be a whole number, got '$CERT_WARN_DAYS'"
	_days=$(expr "$CERT_WARN_DAYS" + 0)
	_end=$(openssl x509 -noout -enddate -in "$CERT_DIR/tls.crt") || die "cannot read $CERT_DIR/tls.crt"
	if ! openssl x509 -noout -checkend "$((_days * 86400))" -in "$CERT_DIR/tls.crt" >/dev/null; then
		die "the etcd client certificate expires within $_days days (${_end#notAfter=}); renew it with etcd-backup-client-cert.sh"
	fi
	log "the etcd client certificate is valid beyond $_days days (${_end#notAfter=})"
}

backup() {
	[ -n "$CLUSTER_NAME" ] || die "CLUSTER_NAME is not set"
	is_count "$KEEP_HOURLY" && [ "$KEEP_HOURLY" -ge 1 ] || die "KEEP_HOURLY must be a whole number >= 1"
	is_count "$KEEP_DAILY" || die "KEEP_DAILY must be a whole number >= 0"
	is_count "$MIN_BYTES" || die "MIN_BYTES must be a whole number"
	[ -s "$AGE_RECIPIENTS_FILE" ] || die "no age recipients at $AGE_RECIPIENTS_FILE; refusing to write an unencrypted backup"
	[ -d "$BACKUP_DIR" ] && [ -w "$BACKUP_DIR" ] || die "backup directory $BACKUP_DIR is missing or not writable"

	stamp=$(date -u +%Y%m%dT%H%M%SZ)
	name=$(snapshot_name "$CLUSTER_NAME" "$stamp")
	plain="$WORK_DIR/snapshot.db"
	partial="$BACKUP_DIR/.partial-$name"
	# A trapped signal must still end the run: in sh the trap runs and the
	# script carries on, so INT and TERM exit explicitly, and EXIT cleans up.
	trap 'rm -f "$plain" "$plain.part" "$partial" "$WORK_DIR/auth" "$WORK_DIR/err"' EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM
	# Leftovers of a run that was SIGKILLed: this cluster's partials only,
	# matched as strictly as pruning matches -- a glob on the name would also
	# take a cluster whose name merely starts with this one's.
	for f in "$BACKUP_DIR"/.partial-*; do
		[ -e "$f" ] || continue
		b=${f##*/}
		if is_snapshot_of "$CLUSTER_NAME" "${b#.partial-}"; then rm -f -- "$f"; fi
	done

	endpoints=$(control_plane_endpoints)
	[ -n "$endpoints" ] || die "no control-plane InternalIPs found for $CLUSTER_NAMESPACE/$CLUSTER_NAME"

	saved=""
	for ep in $endpoints; do
		if ! etcdctl_ --command-timeout="$HEALTH_TIMEOUT" --endpoints "$ep" \
			get --consistency=l --keys-only --limit=1 "" --from-key >/dev/null 2>"$WORK_DIR/err"; then
			log "skipping $ep: $(errors_of "$WORK_DIR/err")"
			continue
		fi
		rm -f "$plain" "$plain.part"
		if etcdctl_ --command-timeout="$SNAPSHOT_TIMEOUT" --endpoints "$ep" \
			snapshot save "$plain" >/dev/null 2>"$WORK_DIR/err"; then
			saved=$ep
			break
		fi
		log "snapshot from $ep failed: $(errors_of "$WORK_DIR/err")"
	done
	[ -n "$saved" ] || die "no etcd member of $CLUSTER_NAME answered a linearizable read and produced a snapshot"

	status=$(etcdutl snapshot status "$plain" -w json) || die "etcdutl could not read the snapshot from $saved"
	keys=$(printf '%s' "$status" | jq -r '.totalKey') || die "unreadable etcdutl status"
	rev=$(printf '%s' "$status" | jq -r '.revision') || die "unreadable etcdutl status"
	bytes=$(wc -c <"$plain" | tr -d ' ')
	is_count "$keys" && [ "$keys" -ge 1 ] || die "snapshot from $saved has no keys"
	[ "$bytes" -ge "$MIN_BYTES" ] || die "snapshot from $saved is $bytes bytes, under the $MIN_BYTES-byte floor"

	age -R "$AGE_RECIPIENTS_FILE" -o "$partial" "$plain" || die "age encryption failed"
	mv "$partial" "$BACKUP_DIR/$name" || die "could not move the backup into place"
	rm -f "$plain"
	log "wrote $BACKUP_DIR/$name from $saved: revision $rev, $keys keys, $bytes bytes before encryption"

	# Only now, with a verified backup written, is anything deleted.
	listing=$(ls -1A "$BACKUP_DIR") || die "could not list $BACKUP_DIR; nothing pruned"
	newest=$(printf '%s\n' "$listing" | snapshots_of "$CLUSTER_NAME" | awk '$0 "" > m "" { m = $0 } END { print m }')
	if [ "$newest" != "$name" ]; then
		# A snapshot newer than this one means the clock jumped at some point.
		# Pruning by name would then keep the wrong files, so prune nothing.
		die "$newest is newer than the backup just written ($name); the clock has moved backwards. Nothing pruned. Check the node's clock, then move or remove the future-dated files"
	fi
	deletes=$(printf '%s\n' "$listing" | prune_plan "$CLUSTER_NAME" "$KEEP_HOURLY" "$KEEP_DAILY" "$name") ||
		die "could not plan pruning; nothing pruned"
	check_prune_plan "$CLUSTER_NAME" "$name" "$KEEP_HOURLY" "$listing" "$deletes" ||
		die "refusing an unsafe prune plan; nothing pruned"
	prune_failed=0
	for f in $deletes; do
		if rm -f -- "$BACKUP_DIR/$f"; then log "pruned $f"; else log "could not prune $f"; prune_failed=1; fi
	done
	[ "$prune_failed" -eq 0 ] || die "pruning failed; the new backup was written"
}

main() {
	case $MODE in
	backup) backup ;;
	check-cert) check_cert ;;
	*) die "unknown MODE '$MODE' (backup or check-cert)" ;;
	esac
}

# Sourced by the tests with ETCD_BACKUP_LIB=1, which want the functions only.
[ "${ETCD_BACKUP_LIB:-}" = 1 ] || main "$@"
