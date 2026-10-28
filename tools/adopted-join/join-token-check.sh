#!/bin/sh
# join-token-check.sh -- say whether a machine built from an adopted cluster's
# bootstrap Secret would join that cluster right now.
#
# READ-ONLY. Run it with a kubeconfig for the cluster the machines join -- for
# an adopted cluster that is also where the Secret lives. It changes nothing,
# and it never prints the token: only its id, which is public by design (it is
# the name of the token's own Secret, and cluster-info is signed under it).
#
# Why it exists. A cluster Cluster API did not build gets no join data from
# CABPK, so its MachineDeployment names a hand-made Secret through
# bootstrap.dataSecretName, holding cloud-init with a literal
# `kubeadm join ... --token <id>.<secret>`. That token expires. Past that, a new
# machine boots perfectly and fails to join as unauthorized, which reads like a
# provider bug and is not. join-token-renew.sh fixes it; this says whether you
# need to.
#
# Checks, in the order kubeadm join would trip over them:
#   - the Secret holds exactly one flag-form `kubeadm join` token;
#   - kube-system/bootstrap-token-<id> exists, matches (id and secret), can authenticate, puts
#     the joiner in the kubeadm node group, and has not expired;
#   - cluster-info is signed for that id, which token discovery requires;
#   - the CA hash the join pins is the cluster's CA;
#   - the bootstrap group is bound to the five roles a join needs.
# And two warnings that do not stop a join: a client-side-apply annotation
# holding a full copy of the Secret's data, and cloud-init that never enables
# the kubelet, so the node would not come back after a reboot.
#
# Exit status: 0 when a machine created now would join, 1 when it would not,
# 2 when it cannot tell: the Secret is missing or is not something this can
# read, or the API server could not be reached or refused a read. A read that
# fails is never taken to mean the thing is absent.
set -eu

SECRET=${1:-hydra-worker-0-bootstrap}
NAMESPACE=${NAMESPACE:-default}
GROUP=system:bootstrappers:kubeadm:default-node-token
# Warn when a valid token has less than this left. A replacement takes minutes
# to reach kubeadm join (it installs the runtime and kubeadm first).
MARGIN_SECONDS=${MARGIN_SECONDS:-900}

failed=0
unchecked=0
ok() { printf 'ok    %s\n' "$*"; }
bad() {
	printf 'FAIL  %s\n' "$*"
	failed=1
}
warn() { printf 'warn  %s\n' "$*"; }
die() {
	printf 'FAIL  %s\n' "$*"
	exit 2
}

# RFC 3339 (UTC, Z) to epoch seconds, with GNU date or BSD date.
epoch_of() {
	date -u -d "$1" +%s 2>/dev/null || date -j -u -f '%Y-%m-%dT%H:%M:%SZ' "$1" +%s 2>/dev/null
}

value=$(kubectl -n "$NAMESPACE" get secret "$SECRET" --ignore-not-found -o jsonpath='{.data.value}') ||
	die "could not read Secret $NAMESPACE/$SECRET (kubectl's error is above)"
[ -n "$value" ] || die "Secret $NAMESPACE/$SECRET does not exist or has no \"value\" key"
userdata=$(printf '%s' "$value" | base64 -d)

# Only the flag form. A join driven by a JoinConfiguration file keeps its token
# somewhere this does not look, and saying "no token" about it would be wrong.
joins=$(printf '%s\n' "$userdata" | grep -c 'kubeadm join' || true)
[ "$joins" = 1 ] || die "$NAMESPACE/$SECRET has $joins \`kubeadm join\` lines; this handles exactly one"
join=$(printf '%s\n' "$userdata" | grep 'kubeadm join')
token=$(printf '%s\n' "$join" | sed -n 's/.*--token[ =]\([a-z0-9]\{6\}\.[a-z0-9]\{16\}\).*/\1/p')
[ -n "$token" ] || die "the \`kubeadm join\` in $NAMESPACE/$SECRET has no \`--token <id>.<secret>\`"
id=${token%%.*}
ok "$NAMESPACE/$SECRET joins with token id $id"

ts=bootstrap-token-$id
found=$(kubectl -n kube-system get secret "$ts" --ignore-not-found -o name) ||
	die "could not read kube-system/$ts, so cannot tell whether token $id is valid (kubectl's error is above)"
if [ -z "$found" ]; then
	bad "token $id does not exist (kube-system/$ts is gone: expired and cleaned up, or deleted) -- a new machine would fail to join as unauthorized"
else
	# field KEY: the token Secret's data KEY, decoded, into $f. Not through
	# $(...), where die would end only the subshell.
	field() {
		f=$(kubectl -n kube-system get secret "$ts" -o jsonpath="{.data.$1}") ||
			die "could not read kube-system/$ts (kubectl's error is above)"
		f=$(printf '%s' "$f" | base64 -d)
	}
	type=$(kubectl -n kube-system get secret "$ts" -o jsonpath='{.type}') ||
		die "could not read kube-system/$ts (kubectl's error is above)"
	[ "$type" = bootstrap.kubernetes.io/token ] || bad "kube-system/$ts has type \"$type\", so the API server does not treat it as a token"
	# The authenticator, and the signer, also require token-id to match the name.
	field token-id
	[ "$f" = "$id" ] || bad "kube-system/$ts has token-id \"$f\", not $id, so the API server refuses the token"
	# Compared in the shell; neither value is passed to another process.
	field token-secret
	[ "$f" = "${token#*.}" ] || bad "kube-system/$ts holds a different secret than $NAMESPACE/$SECRET"
	field usage-bootstrap-authentication
	[ "$f" = true ] || bad "token $id is not usable for authentication"
	field auth-extra-groups
	case ",$f," in
	*,"$GROUP",*) ;;
	*) bad "token $id does not put the joiner in $GROUP, which the kubeadm join roles are bound to" ;;
	esac
	field expiration
	expiration=$f
	if [ -z "$expiration" ]; then
		warn "token $id never expires: it is a standing credential to join this cluster as a node"
	else
		left=$(($(epoch_of "$expiration") - $(date -u +%s)))
		if [ "$left" -le 0 ]; then
			bad "token $id expired at $expiration (the API server refuses it even before the cleaner deletes it)"
		elif [ "$left" -lt "$MARGIN_SECONDS" ]; then
			warn "token $id is valid for only $((left / 60))m more, until $expiration"
		else
			ok "token $id is valid for $((left / 3600))h$((left % 3600 / 60))m more, until $expiration"
		fi
	fi
	jws=$(kubectl -n kube-public get configmap cluster-info -o jsonpath="{.data.jws-kubeconfig-$id}") ||
		die "could not read kube-public/cluster-info (kubectl's error is above)"
	if [ -n "$jws" ]; then
		ok "cluster-info is signed for token $id"
	else
		bad "cluster-info is not signed for token $id, so token discovery fails (the bootstrapsigner controller signs it)"
	fi
fi

cluster_info=$(kubectl -n kube-public get configmap cluster-info -o jsonpath='{.data.kubeconfig}') ||
	die "could not read kube-public/cluster-info (kubectl's error is above)"
cluster_ca_hash=$(printf '%s\n' "$cluster_info" |
	sed -n 's/^ *certificate-authority-data: *//p' | head -n 1 | base64 -d |
	openssl x509 -pubkey -noout | openssl pkey -pubin -outform der | openssl dgst -sha256 -hex | sed 's/.* //')
pinned=$(printf '%s\n' "$join" | sed -n 's/.*--discovery-token-ca-cert-hash[ =]sha256:\([0-9a-f]\{64\}\).*/\1/p')
if [ -z "$pinned" ]; then
	warn "the join pins no CA hash, so it trusts whatever answers at its endpoint"
elif [ "$pinned" = "$cluster_ca_hash" ]; then
	ok "the CA hash the join pins is the cluster's CA"
else
	bad "the CA hash the join pins is not the CA in cluster-info (compared with its first certificate)"
fi

endpoint=$(printf '%s\n' "$join" | sed -n 's/.*kubeadm join \([^ -][^ ]*\).*/\1/p')
server=$(printf '%s\n' "$cluster_info" | sed -n 's|^ *server: *https://||p' | head -n 1)
if [ "$endpoint" = "$server" ]; then
	ok "the join targets $endpoint, the endpoint cluster-info names"
else
	warn "the join targets \"$endpoint\" but cluster-info names \"$server\" -- fine only if both reach this API server"
fi

# The roles kubeadm init binds to the node group: look up its own Node before
# TLS bootstrap (kubeadm join fails on anything but NotFound), create a CSR,
# have it approved, and read the kubeadm and kubelet configuration.
roles_bound_to_group='{{range .items}}{{$r := .roleRef.name}}{{range .subjects}}{{if eq .name "'$GROUP'"}}{{$r}}{{"\n"}}{{end}}{{end}}{{end}}'
unbound=0
if bound=$(kubectl get clusterrolebindings -o go-template="$roles_bound_to_group"); then
	for role in kubeadm:get-nodes system:node-bootstrapper system:certificates.k8s.io:certificatesigningrequests:nodeclient; do
		printf '%s\n' "$bound" | grep -qx "$role" || {
			bad "$GROUP is not bound to ClusterRole $role"
			unbound=1
		}
	done
else
	warn "could not list ClusterRoleBindings, so the ClusterRoles a join needs were not checked"
	unchecked=1
fi
if bound=$(kubectl -n kube-system get rolebindings -o go-template="$roles_bound_to_group"); then
	for role in kubeadm:nodes-kubeadm-config kubeadm:kubelet-config; do
		printf '%s\n' "$bound" | grep -qx "$role" || {
			bad "$GROUP is not bound to Role kube-system/$role"
			unbound=1
		}
	done
else
	warn "could not list RoleBindings in kube-system, so the Roles a join needs were not checked"
	unchecked=1
fi
[ "$unbound" = 1 ] || [ "$unchecked" = 1 ] || ok "$GROUP is bound to the roles a join needs"

annotation=$(kubectl -n "$NAMESPACE" get secret "$SECRET" -o jsonpath='{.metadata.annotations.kubectl\.kubernetes\.io/last-applied-configuration}') ||
	die "could not read Secret $NAMESPACE/$SECRET (kubectl's error is above)"
if [ -n "$annotation" ]; then
	warn "$NAMESPACE/$SECRET carries a full copy of its data, token included, in its last-applied-configuration annotation (join-token-renew.sh removes it)"
fi
# A heuristic: cloud-init can say this many ways. It looks for the kubelet unit
# itself being enabled, not `enable` and `kubelet` anywhere on one line.
printf '%s\n' "$userdata" | grep -Eq 'systemctl[[:space:]]+enable([[:space:]]+--now)?[[:space:]]+kubelet(\.service)?([[:space:];&|]|$)' ||
	warn "$NAMESPACE/$SECRET never enables the kubelet: a node built from it joins, but does not come back after a reboot"

users=$(kubectl -n "$NAMESPACE" get machinedeployments,machinesets,machines -o jsonpath="{range .items[?(@.spec.template.spec.bootstrap.dataSecretName==\"$SECRET\")]}{.kind}/{.metadata.name} {end}{range .items[?(@.spec.bootstrap.dataSecretName==\"$SECRET\")]}{.kind}/{.metadata.name} {end}" 2>/dev/null || true)
[ -z "$users" ] || printf 'info  used by %s\n' "$users"

if [ "$failed" = 1 ]; then
	echo "A machine created now from $NAMESPACE/$SECRET would NOT join. Run join-token-renew.sh first."
	exit 1
elif [ "$unchecked" = 1 ]; then
	echo "Cannot tell whether a machine created now from $NAMESPACE/$SECRET would join: some checks could not run (warnings above)."
	exit 2
fi
echo "A machine created now from $NAMESPACE/$SECRET would join."
