#!/bin/sh
# join-token-renew.sh -- give an adopted cluster's bootstrap Secret a fresh
# kubeadm join token, so the next machine built from it can join.
#
# DRY RUN UNLESS --apply. Without it every write goes to the API server with
# --dry-run=server: admission sees the real objects, nothing is stored, and the
# change to the cloud-init is shown with the token redacted. With --apply it
#   1. mints kube-system/bootstrap-token-<id>, the same Secret `kubeadm token
#      create` writes -- made with kubectl, so no control-plane host access;
#   2. waits for the bootstrapsigner to sign cluster-info for it, which token
#      discovery needs, and deletes the token again if that never happens;
#   3. in one write, which fails if the Secret changed since it was read,
#      replaces the token in its `kubeadm join` line -- nothing else in the
#      cloud-init -- and removes its client-side-apply annotation, which holds
#      a full copy of the old data; and
#   4. reads the Secret back and checks it holds exactly what was written.
#
# No Machine is touched, and running machines are unaffected: the provider
# reads bootstrap data once, when it creates a VM. The next VM created from the
# Secret -- a replacement, a rollout's surge, a scale-up -- uses the new token.
#
# Run it with a kubeconfig for the cluster the machines join, on a host with
# openssl. Tokens are read into a 0700 directory, on tmpfs where the host has
# one, and never printed or put on a command line; only the token id is shown,
# which is public by design.
#
# The token lasts TTL_MINUTES (default 120): long enough for a VM to boot,
# install kubeadm and join; short enough that the copies outliving the join --
# the machine's cidata image on the hypervisor, /var/lib/cloud in the guest --
# soon stop being a credential. Holding a valid token lets anyone join this
# cluster as a node. TTL_MINUTES=0 makes one that never expires.
set -eu

usage() {
	echo "usage: $0 [--apply] [secret]   (env: NAMESPACE, TTL_MINUTES, SIGN_TIMEOUT)" >&2
	exit 2
}
apply=false
case ${1:-} in
--apply)
	apply=true
	shift
	;;
-*) usage ;;
esac
[ $# -le 1 ] || usage
SECRET=${1:-hydra-worker-0-bootstrap}
NAMESPACE=${NAMESPACE:-default}
TTL_MINUTES=${TTL_MINUTES:-120}
SIGN_TIMEOUT=${SIGN_TIMEOUT:-60}
GROUP=system:bootstrappers:kubeadm:default-node-token
case $TTL_MINUTES$SIGN_TIMEOUT in *[!0-9]*) usage ;; esac
dry=
$apply || dry=--dry-run=server

d=""
for base in ${XDG_RUNTIME_DIR:-} /dev/shm; do
	[ -n "$base" ] && [ -d "$base" ] && [ -w "$base" ] && d=$(mktemp -d "$base/join-token.XXXXXX" 2>/dev/null) && break
done
[ -n "$d" ] || d=$(mktemp -d)
chmod 700 "$d"
umask 077
# A token minted but not yet referenced by the Secret is deleted on any exit,
# so a failure part-way never leaves a working credential nobody uses.
minted=""
cleanup() {
	if [ -n "$minted" ]; then
		kubectl -n kube-system delete secret "$minted" --ignore-not-found >/dev/null 2>&1 ||
			echo "could not delete the unused token kube-system/$minted -- delete it by hand" >&2
	fi
	rm -rf "$d"
}
# INT and TERM exit explicitly: in sh a trap runs and the script carries on.
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# occurrences NEEDLE FILE: how many times NEEDLE occurs in FILE. The needle
# reaches awk through its environment, which only this user can read, rather
# than its argv, which anyone on the host can.
occurrences() {
	NEEDLE=$1 awk 'BEGIN { n = 0 } { s = $0; while ((i = index(s, ENVIRON["NEEDLE"])) > 0) { n++; s = substr(s, i + 1) } } END { print n }' "$2"
}
redact() { sed 's/\([a-z0-9]\{6\}\)\.[a-z0-9]\{16\}/\1.<redacted>/g'; }
rfc3339() { date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -r "$1" +%Y-%m-%dT%H:%M:%SZ; }

# --- read, and refuse anything a new token would not fix -------------------
rv=$(kubectl -n "$NAMESPACE" get secret "$SECRET" -o jsonpath='{.metadata.resourceVersion}')
annotated=$(kubectl -n "$NAMESPACE" get secret "$SECRET" -o jsonpath='{.metadata.annotations.kubectl\.kubernetes\.io/last-applied-configuration}')
kubectl -n "$NAMESPACE" get secret "$SECRET" -o jsonpath='{.data.value}' | base64 -d >"$d/old"
[ -s "$d/old" ] || {
	echo "Secret $NAMESPACE/$SECRET has no \"value\" key" >&2
	exit 1
}
[ "$(grep -c 'kubeadm join' "$d/old" || true)" = 1 ] || {
	echo "$NAMESPACE/$SECRET must have exactly one \`kubeadm join\` line" >&2
	exit 1
}
old=$(grep 'kubeadm join' "$d/old" | sed -n 's/.*--token[ =]\([a-z0-9]\{6\}\.[a-z0-9]\{16\}\).*/\1/p')
[ -n "$old" ] || {
	echo "the \`kubeadm join\` in $NAMESPACE/$SECRET has no \`--token <id>.<secret>\` (only the flag form is handled)" >&2
	exit 1
}
[ "$(occurrences "$old" "$d/old")" = 1 ] || {
	echo "the old token appears more than once in $NAMESPACE/$SECRET; refusing to guess which to replace" >&2
	exit 1
}

kubectl -n kube-public get configmap cluster-info -o jsonpath='{.data.kubeconfig}' >"$d/cluster-info"
cluster_ca_hash=$(sed -n 's/^ *certificate-authority-data: *//p' "$d/cluster-info" | head -n 1 | base64 -d |
	openssl x509 -pubkey -noout | openssl pkey -pubin -outform der | openssl dgst -sha256 -hex | sed 's/.* //')
pinned=$(grep 'kubeadm join' "$d/old" | sed -n 's/.*--discovery-token-ca-cert-hash[ =]sha256:\([0-9a-f]\{64\}\).*/\1/p')
if [ -n "$pinned" ] && [ "$pinned" != "$cluster_ca_hash" ]; then
	echo "the CA hash the join pins is not the CA in cluster-info; a new token would not make it join" >&2
	exit 1
fi

# --- mint ------------------------------------------------------------------
rand() { LC_ALL=C tr -dc 'a-z0-9' </dev/urandom | head -c "$1"; }
for _ in 1 2 3 4 5; do
	id=$(rand 6)
	[ -n "$(kubectl -n kube-system get secret "bootstrap-token-$id" --ignore-not-found -o name)" ] || break
done
secret=$(rand 16)
[ ${#id} = 6 ] && [ ${#secret} = 16 ] || {
	echo "could not generate a token" >&2
	exit 1
}
expiration=""
[ "$TTL_MINUTES" = 0 ] || expiration=$(rfc3339 $(($(date -u +%s) + TTL_MINUTES * 60)))
{
	printf 'apiVersion: v1\nkind: Secret\ntype: bootstrap.kubernetes.io/token\n'
	printf 'metadata:\n  name: bootstrap-token-%s\n  namespace: kube-system\n' "$id"
	printf 'stringData:\n  token-id: "%s"\n  token-secret: "%s"\n' "$id" "$secret"
	[ -z "$expiration" ] || printf '  expiration: "%s"\n' "$expiration"
	printf '  usage-bootstrap-authentication: "true"\n  usage-bootstrap-signing: "true"\n'
	printf '  auth-extra-groups: "%s"\n' "$GROUP"
	printf '  description: "kubeadm join token for %s/%s, from join-token-renew.sh"\n' "$NAMESPACE" "$SECRET"
} >"$d/token.yaml"
kubectl create ${dry:+"$dry"} -f "$d/token.yaml" >/dev/null
if $apply; then
	minted=bootstrap-token-$id
	waited=0
	until [ -n "$(kubectl -n kube-public get configmap cluster-info -o jsonpath="{.data.jws-kubeconfig-$id}")" ]; do
		[ "$waited" -lt "$SIGN_TIMEOUT" ] || {
			echo "cluster-info was not signed for token $id within ${SIGN_TIMEOUT}s (is the bootstrapsigner controller running?); deleted it, the Secret is unchanged" >&2
			exit 1
		}
		sleep 1
		waited=$((waited + 1))
	done
fi

# --- rewrite the join line, and only it -------------------------------------
OLD=$old NEW=$id.$secret awk '{
	i = index($0, ENVIRON["OLD"])
	if (i) $0 = substr($0, 1, i - 1) ENVIRON["NEW"] substr($0, i + length(ENVIRON["OLD"]))
	print
}' "$d/old" >"$d/new"
changed=$(awk 'NR == FNR { a[FNR] = $0; n = FNR; next } a[FNR] != $0 { c++; line = FNR } END { if (FNR != n) print "length"; else print c + 0 ":" line }' "$d/old" "$d/new")
if [ "${changed%%:*}" != 1 ] || [ "$(occurrences "$old" "$d/new")" != 0 ] || [ "$(occurrences "$id.$secret" "$d/new")" != 1 ]; then
	echo "the rewritten cloud-init is not the old one with just the token replaced; nothing written" >&2
	exit 1
fi
line=${changed#*:}

value=$(base64 <"$d/new" | tr -d '\n')
# One JSON patch, so the token and the annotation change together or not at
# all, and its test op makes it conditional on the version read above: a Secret
# someone changed since then is refused, not overwritten with its older self.
#
# Not server-side apply. On a Secret made by client-side apply, as these are,
# kubectl's apply migrates every field the annotation lists to the new field
# manager -- type included -- and the next run, not restating type, removes it.
{
	printf '[{"op":"test","path":"/metadata/resourceVersion","value":"%s"}' "$rv"
	printf ',{"op":"replace","path":"/data/value","value":"%s"}' "$value"
	[ -z "$annotated" ] || printf ',{"op":"remove","path":"/metadata/annotations/kubectl.kubernetes.io~1last-applied-configuration"}'
	printf ']\n'
} >"$d/patch.json"
kubectl -n "$NAMESPACE" patch secret "$SECRET" --type=json --patch-file="$d/patch.json" \
	--field-manager=join-token-renew ${dry:+"$dry"} >/dev/null || {
	echo "could not write $NAMESPACE/$SECRET -- changed since it was read? Nothing was written" >&2
	exit 1
}
minted=""

old_id=${old%%.*}
until_text=${expiration:-never -- a standing credential to join this cluster}
if ! $apply; then
	echo "DRY RUN -- the API server accepted every write and stored none. With --apply this would:"
	echo "  create kube-system/bootstrap-token-<new id>, expiring after ${TTL_MINUTES}m (0 = never)"
	echo "  change line $line of $NAMESPACE/$SECRET, and no other:"
	sed -n "${line}p" "$d/old" | redact | sed 's/^/    - /'
	sed -n "${line}p" "$d/new" | redact | sed "s/^/    + /; s/$id\.<redacted>/<new id>.<redacted>/"
	[ -z "$annotated" ] || echo "  remove its last-applied-configuration annotation"
	exit 0
fi

[ "$(kubectl -n "$NAMESPACE" get secret "$SECRET" -o jsonpath='{.data.value}')" = "$value" ] || {
	echo "read-back of $NAMESPACE/$SECRET does not match what was written -- check it by hand" >&2
	exit 1
}
echo "Stored $NAMESPACE/$SECRET: it now joins with token $id (was $old_id), valid until $until_text."
echo "Only line $line changed.${annotated:+ Removed its last-applied-configuration annotation.}"
if [ -n "$(kubectl -n kube-system get secret "bootstrap-token-$old_id" --ignore-not-found -o name)" ]; then
	echo "Token $old_id still exists and stays valid until it expires; delete kube-system/bootstrap-token-$old_id to revoke it now."
fi
echo "join-token-check.sh $SECRET should now report that a new machine would join."
