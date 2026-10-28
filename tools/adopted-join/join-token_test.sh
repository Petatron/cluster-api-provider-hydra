#!/bin/sh
# Tests for join-token-check.sh and join-token-renew.sh, against a real
# kube-apiserver: the envtest binaries, started here with bootstrap-token
# authentication and RBAC on, and stopped on exit. Nothing here talks to any
# other cluster.
#
#   make test-adopted-join
#   KUBEBUILDER_ASSETS=<dir with etcd, kube-apiserver, kubectl> sh tools/adopted-join/join-token_test.sh
#
# kube-apiserver alone has no bootstrapsigner -- that is a controller-manager
# controller -- so a loop stands in for it where a test needs cluster-info
# signed. One case waits for a token to expire, so the suite takes a minute
# or two. Needs openssl.
set -u

here=$(cd "$(dirname "$0")" && pwd)
CHECK=${CHECK:-$here/join-token-check.sh}
RENEW=${RENEW:-$here/join-token-renew.sh}
[ -x "${KUBEBUILDER_ASSETS:-}/kube-apiserver" ] || {
	echo "set KUBEBUILDER_ASSETS to a directory with etcd, kube-apiserver and kubectl (make test-adopted-join does)" >&2
	exit 2
}
PORT=${ADOPTED_JOIN_TEST_PORT:-16443}
ETCD_PORT=${ADOPTED_JOIN_TEST_ETCD_PORT:-12379}
# The scripts under test call plain `kubectl`; make it the one matching this
# apiserver.
PATH=$KUBEBUILDER_ASSETS:$PATH
export PATH

t=$(mktemp -d)
etcd_pid=""
apiserver_pid=""
signer=""
signer_on() {
	(while :; do
		for s in $($K -n kube-system get secrets --field-selector type=bootstrap.kubernetes.io/token -o jsonpath='{range .items[*]}{.metadata.name}{" "}{end}' 2>/dev/null); do
			id=${s#bootstrap-token-}
			[ -n "$($K -n kube-public get cm cluster-info -o jsonpath="{.data.jws-kubeconfig-$id}" 2>/dev/null)" ] ||
				$K -n kube-public patch cm cluster-info --type=merge -p "{\"data\":{\"jws-kubeconfig-$id\":\"stand-in\"}}" >/dev/null 2>&1
		done
		sleep 1
	done) &
	signer=$!
}
signer_off() {
	[ -z "$signer" ] || kill "$signer" 2>/dev/null
	wait "$signer" 2>/dev/null
	signer=""
}

# halt PID: SIGTERM, then SIGKILL if it is still there after 20 seconds.
halt() {
	[ -n "$1" ] || return 0
	kill "$1" 2>/dev/null || return 0
	i=0
	while kill -0 "$1" 2>/dev/null && [ "$i" -lt 20 ]; do
		sleep 1
		i=$((i + 1))
	done
	kill -9 "$1" 2>/dev/null
	wait "$1" 2>/dev/null
}
# The apiserver first: its graceful shutdown can hang for good once etcd is gone.
stop() {
	signer_off
	halt "$apiserver_pid"
	halt "$etcd_pid"
	rm -rf "$t"
}
trap stop EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# --- a throwaway control plane --------------------------------------------------
mkdir -p "$t/certs" "$t/etcd"
openssl genrsa -out "$t/certs/sa.key" 2048 2>/dev/null
openssl rsa -in "$t/certs/sa.key" -pubout -out "$t/certs/sa.pub" 2>/dev/null
admin=$(LC_ALL=C tr -dc 'a-z0-9' </dev/urandom | head -c 32)
# Two users with less than admin, given just enough RBAC below to show how the
# check reads a refusal.
reader=$(LC_ALL=C tr -dc 'a-z0-9' </dev/urandom | head -c 32)
reader2=$(LC_ALL=C tr -dc 'a-z0-9' </dev/urandom | head -c 32)
{
	printf '%s,admin,admin,"system:masters"\n' "$admin"
	printf '%s,reader,reader\n%s,reader2,reader2\n' "$reader" "$reader2"
} >"$t/certs/tokens.csv"
"$KUBEBUILDER_ASSETS/etcd" --data-dir "$t/etcd" --listen-client-urls "http://127.0.0.1:$ETCD_PORT" \
	--advertise-client-urls "http://127.0.0.1:$ETCD_PORT" --listen-peer-urls "http://127.0.0.1:$((ETCD_PORT + 1))" \
	>"$t/etcd.log" 2>&1 &
etcd_pid=$!
"$KUBEBUILDER_ASSETS/kube-apiserver" --etcd-servers="http://127.0.0.1:$ETCD_PORT" \
	--secure-port="$PORT" --bind-address=127.0.0.1 --cert-dir "$t/certs" \
	--token-auth-file "$t/certs/tokens.csv" --enable-bootstrap-token-auth=true --authorization-mode=RBAC \
	--service-account-issuer=https://kubernetes.default.svc --service-account-key-file "$t/certs/sa.pub" \
	--service-account-signing-key-file "$t/certs/sa.key" --service-cluster-ip-range=10.96.0.0/24 \
	>"$t/apiserver.log" 2>&1 &
apiserver_pid=$!
cat >"$t/kubeconfig" <<EOF
apiVersion: v1
kind: Config
clusters: [{name: t, cluster: {server: "https://127.0.0.1:$PORT", insecure-skip-tls-verify: true}}]
users: [{name: admin, user: {token: "$admin"}}]
contexts: [{name: t, context: {cluster: t, user: admin}}]
current-context: t
EOF
export KUBECONFIG="$t/kubeconfig"
sed "s/$admin/$reader/" "$t/kubeconfig" >"$t/kubeconfig-reader"
sed "s/$admin/$reader2/" "$t/kubeconfig" >"$t/kubeconfig-reader2"
# Nothing listens on port 1: an API server that cannot be reached.
sed "s|127.0.0.1:$PORT|127.0.0.1:1|" "$t/kubeconfig" >"$t/kubeconfig-unreachable"
K="kubectl --request-timeout=10s"
waited=0
until [ "$($K get --raw /readyz 2>/dev/null)" = ok ]; do
	[ "$waited" -lt 60 ] || {
		echo "kube-apiserver did not become ready; last lines of its log:" >&2
		tail -n 20 "$t/apiserver.log" >&2
		exit 1
	}
	sleep 1
	waited=$((waited + 1))
done
server=https://127.0.0.1:$PORT

# --- assertions -----------------------------------------------------------------
fails=0
passes=0
section() { printf '\n=== %s\n' "$*"; }
pass() {
	passes=$((passes + 1))
	printf 'ok   %s\n' "$1"
}
fail() {
	fails=$((fails + 1))
	printf 'FAIL %s\n' "$1"
}
# expect NAME COMMAND...
expect() {
	name=$1
	shift
	if "$@"; then pass "$name"; else fail "$name"; fi
}
# says OUTPUT PATTERN: OUTPUT has a line matching the basic regex PATTERN.
says() { printf '%s\n' "$1" | grep -q -- "$2"; }
lacks() { ! says "$1" "$2"; }
show() { printf '%s\n' "$1" | sed 's/^/    | /'; }

tokens() { $K -n kube-system get secrets --field-selector type=bootstrap.kubernetes.io/token -o name | wc -l | tr -d ' '; }
rv() { $K get secret hydra-worker-0-bootstrap -o jsonpath='{.metadata.resourceVersion}'; }
userdata() { $K get secret hydra-worker-0-bootstrap -o jsonpath='{.data.value}' | base64 -d; }
current_token() { userdata | sed -n 's/.*--token \([a-z0-9]\{6\}\.[a-z0-9]\{16\}\).*/\1/p'; }
whoami_with() { kubectl --kubeconfig=/dev/null --server="$server" --insecure-skip-tls-verify --token="$1" auth whoami -o jsonpath='{.status.userInfo.username} {.status.userInfo.groups}' 2>&1; }


ca_hash=$(openssl x509 -pubkey -noout -in "$t/certs/apiserver.crt" | openssl pkey -pubin -outform der | openssl dgst -sha256 -hex | sed 's/.* //')
OLD=abc123.0123456789abcdef

# fixture [TOKEN [CAHASH [EXTRA_LINE]]]: a fresh cluster-info and bootstrap Secret,
# made by client-side apply -- as hand-made Secrets usually are -- so it carries
# the last-applied annotation. The cloud-init has the shape of a real one, the
# quoting-sensitive providerID line included.
fixture() {
	$K -n kube-system delete secrets --field-selector type=bootstrap.kubernetes.io/token --ignore-not-found >/dev/null
	$K delete secret hydra-worker-0-bootstrap --ignore-not-found >/dev/null
	$K -n kube-public delete cm cluster-info --ignore-not-found >/dev/null
	printf 'apiVersion: v1\nclusters:\n- cluster:\n    certificate-authority-data: %s\n    server: %s\n  name: ""\nkind: Config\n' \
		"$(base64 <"$t/certs/apiserver.crt" | tr -d '\n')" "$server" >"$t/cluster-info"
	$K -n kube-public create cm cluster-info --from-file=kubeconfig="$t/cluster-info" >/dev/null
	cat >"$t/cloud-init" <<EOF
#cloud-config
package_update: true
runcmd:
  - printf 'KUBELET_EXTRA_ARGS=--provider-id=hydra://libvirt/%s\n' "\$(tr 'A-Z' 'a-z' < /sys/class/dmi/id/product_uuid)" > /etc/default/kubelet
  - apt-get install -y "kubelet=1.35.3-*" "kubeadm=1.35.3-*"
  - apt-mark hold kubelet kubeadm
  - kubeadm join 127.0.0.1:$PORT --token ${1:-$OLD} --discovery-token-ca-cert-hash sha256:${2:-$ca_hash} --cri-socket unix:///run/containerd/containerd.sock
${3:-  - echo joined}
EOF
	$K create secret generic hydra-worker-0-bootstrap --type=cluster.x-k8s.io/secret --from-file=value="$t/cloud-init" \
		--dry-run=client -o yaml | $K apply -f - >/dev/null
	$K label secret hydra-worker-0-bootstrap cluster.x-k8s.io/cluster-name=hydra >/dev/null
}

# The RBAC kubeadm init leaves for the bootstrap group.
G=system:bootstrappers:kubeadm:default-node-token
for r in kubeadm:nodes-kubeadm-config kubeadm:kubelet-config; do
	$K -n kube-system create role "$r" --verb=get --resource=configmaps >/dev/null
	$K -n kube-system create rolebinding "$r" --role="$r" --group="$G" >/dev/null
done
$K create clusterrolebinding kubeadm:kubelet-bootstrap --clusterrole=system:node-bootstrapper --group="$G" >/dev/null
$K create clusterrolebinding kubeadm:node-autoapprove-bootstrap \
	--clusterrole=system:certificates.k8s.io:certificatesigningrequests:nodeclient --group="$G" >/dev/null
$K create clusterrole kubeadm:get-nodes --verb=get --resource=nodes >/dev/null
$K create clusterrolebinding kubeadm:get-nodes --clusterrole=kubeadm:get-nodes --group="$G" >/dev/null
# reader can read the Secret and cluster-info but not kube-system's Secrets;
# reader2 can read those too, but cannot list RBAC bindings.
for u in reader reader2; do
	$K create role "$u-secrets" --verb=get --resource=secrets >/dev/null
	$K create rolebinding "$u-secrets" --role="$u-secrets" --user="$u" >/dev/null
	$K -n kube-public create role "$u-cluster-info" --verb=get --resource=configmaps >/dev/null
	$K -n kube-public create rolebinding "$u-cluster-info" --role="$u-cluster-info" --user="$u" >/dev/null
done
$K -n kube-system create role reader2-tokens --verb=get --resource=secrets >/dev/null
$K -n kube-system create rolebinding reader2-tokens --role=reader2-tokens --user=reader2 >/dev/null

# --- check ----------------------------------------------------------------------
section "check: the token is gone"
fixture
out=$(sh "$CHECK" 2>&1)
rc=$?
show "$out"
expect "exits 1" [ "$rc" = 1 ]
expect "says the token does not exist" says "$out" "^FAIL  token abc123 does not exist"
expect "warns about the last-applied copy" says "$out" "^warn .*last-applied-configuration"
expect "warns the kubelet is never enabled" says "$out" "^warn .*never enables the kubelet"
expect "finds the RBAC" says "$out" "^ok    $G is bound to the roles a join needs"
expect "never prints the token's secret" sh -c '! printf "%s\n" "$1" | grep -q 0123456789abcdef' _ "$out"

section "check: no warning when the cloud-init enables the kubelet"
fixture "$OLD" "$ca_hash" "  - systemctl enable kubelet"
out=$(sh "$CHECK" 2>&1)
expect "no kubelet warning" sh -c '! printf "%s\n" "$1" | grep -q "never enables the kubelet"' _ "$out"
fixture "$OLD" "$ca_hash" "  - systemctl enable --now kubelet"
out=$(sh "$CHECK" 2>&1)
expect "none for enable --now either" lacks "$out" "never enables the kubelet"
fixture "$OLD" "$ca_hash" "  - systemctl enable containerd && systemctl restart kubelet"
out=$(sh "$CHECK" 2>&1)
expect "warns when another unit is enabled and the kubelet only restarted" says "$out" "never enables the kubelet"

section "check: an API server it cannot reach"
out=$(KUBECONFIG=$t/kubeconfig-unreachable sh "$CHECK" 2>&1)
rc=$?
show "$out"
expect "exits 2, not 1" [ "$rc" = 2 ]
expect "says it could not read the Secret" says "$out" "^FAIL  could not read Secret"

# --- renew ----------------------------------------------------------------------
section "renew: a dry run stores nothing"
fixture
before=$(rv)
out=$(sh "$RENEW" 2>&1)
rc=$?
show "$out"
expect "exits 0" [ "$rc" = 0 ]
expect "the Secret is unchanged" [ "$(rv)" = "$before" ]
expect "no token is created" [ "$(tokens)" = 0 ]
expect "shows one changed line" [ "$(printf '%s\n' "$out" | grep -c '^    [-+] ')" = 2 ]
expect "never prints a token's secret" sh -c '! printf "%s\n" "$1" | grep -qE "[a-z0-9]{6}\.[a-z0-9]{16}"' _ "$out"

section "renew --apply, but cluster-info is never signed: rolled back"
before=$(rv)
out=$(SIGN_TIMEOUT=3 sh "$RENEW" --apply 2>&1)
rc=$?
show "$out"
expect "exits non-zero" [ "$rc" != 0 ]
expect "the Secret is unchanged" [ "$(rv)" = "$before" ]
expect "the minted token is deleted again" [ "$(tokens)" = 0 ]

section "renew --apply"
signer_on
cp "$t/cloud-init" "$t/before"
out=$(sh "$RENEW" --apply 2>&1)
rc=$?
show "$out"
expect "exits 0" [ "$rc" = 0 ]
expect "exactly one token exists" [ "$(tokens)" = 1 ]
token=$(current_token)
c=0
[ -n "$token" ] && [ "$token" != "$OLD" ] && c=1
expect "the Secret names a new token" [ "$c" = 1 ]
userdata >"$t/after"
expect "only the join line differs" [ "$(diff "$t/before" "$t/after" | grep -c '^[<>]')" = 2 ]
expect "every other byte is kept" [ "$(wc -c <"$t/before")" = "$(wc -c <"$t/after")" ]
expect "the last-applied annotation is gone" [ -z "$($K get secret hydra-worker-0-bootstrap -o jsonpath='{.metadata.annotations.kubectl\.kubernetes\.io/last-applied-configuration}')" ]
expect "type and labels are kept" [ "$($K get secret hydra-worker-0-bootstrap -o jsonpath='{.type}/{.metadata.labels.cluster\.x-k8s\.io/cluster-name}')" = cluster.x-k8s.io/secret/hydra ]
who=$(whoami_with "$token")
printf '    whoami with the new token: %s\n' "$who"
expect "the API server authenticates it in the kubeadm group" sh -c 'case "$1" in "system:bootstrap:${2%%.*} "*"$3"*) exit 0 ;; esac; exit 1' _ "$who" "$token" "$G"
out=$(sh "$CHECK" 2>&1)
rc=$?
show "$out"
expect "check now exits 0" [ "$rc" = 0 ]

section "check: the bootstrap group cannot get Nodes"
$K delete clusterrolebinding kubeadm:get-nodes >/dev/null
out=$(sh "$CHECK" 2>&1)
rc=$?
expect "exits 1" [ "$rc" = 1 ]
expect "names the missing binding" says "$out" "^FAIL  $G is not bound to ClusterRole kubeadm:get-nodes"
$K create clusterrolebinding kubeadm:get-nodes --clusterrole=kubeadm:get-nodes --group="$G" >/dev/null

section "check: a kubeconfig that cannot read kube-system's Secrets"
out=$(KUBECONFIG=$t/kubeconfig-reader sh "$CHECK" 2>&1)
rc=$?
show "$out"
expect "exits 2, not 1" [ "$rc" = 2 ]
expect "does not call the token missing" lacks "$out" "does not exist"
expect "says it could not read the token" says "$out" "^FAIL  could not read kube-system/bootstrap-token-"

section "check: a kubeconfig that cannot list RBAC bindings"
out=$(KUBECONFIG=$t/kubeconfig-reader2 sh "$CHECK" 2>&1)
rc=$?
show "$out"
expect "exits 2, not 0 or 1" [ "$rc" = 2 ]
expect "says the roles were not checked" says "$out" "^warn  could not list ClusterRoleBindings"
expect "reports no binding as missing" lacks "$out" "is not bound to"
expect "still checks the token" says "$out" "^ok    token .* is valid"

section "renew --apply twice more: each run works, earlier tokens are left to expire"
out=$(sh "$RENEW" --apply 2>&1)
rc=$?
show "$out"
expect "the second run exits 0" [ "$rc" = 0 ]
expect "two tokens exist" [ "$(tokens)" = 2 ]
expect "it says the previous token still exists" says "$out" "still exists and stays valid"
out=$(sh "$RENEW" --apply 2>&1)
rc=$?
expect "the third run exits 0" [ "$rc" = 0 ]
expect "type is still cluster.x-k8s.io/secret" [ "$($K get secret hydra-worker-0-bootstrap -o jsonpath='{.type}')" = cluster.x-k8s.io/secret ]

section "renew --apply when the Secret changes under it: refused, rolled back"
mkdir -p "$t/shim"
real=$(command -v kubectl)
cat >"$t/shim/kubectl" <<EOF
#!/bin/sh
# Change the Secret just before the renew script writes it.
case "\$*" in *"patch secret"*) "$real" annotate secret hydra-worker-0-bootstrap race=\$\$ --overwrite >/dev/null ;; esac
exec "$real" "\$@"
EOF
chmod +x "$t/shim/kubectl"
n=$(tokens)
before=$(current_token)
out=$(PATH=$t/shim:$PATH sh "$RENEW" --apply 2>&1)
rc=$?
show "$out"
expect "exits non-zero" [ "$rc" != 0 ]
expect "the Secret keeps its previous token" [ "$(current_token)" = "$before" ]
expect "the token it minted is deleted" [ "$(tokens)" = "$n" ]

section "renew --apply when the write lands but its response is lost: the token is kept"
cat >"$t/shim/kubectl" <<EOF
#!/bin/sh
# Make the write, then fail as if the response had never arrived.
case "\$*" in *"patch secret"*) "$real" "\$@"; exit 1 ;; esac
exec "$real" "\$@"
EOF
n=$(tokens)
before=$(current_token)
out=$(PATH=$t/shim:$PATH sh "$RENEW" --apply 2>&1)
rc=$?
show "$out"
expect "exits 0" [ "$rc" = 0 ]
expect "the Secret names the new token" [ "$(current_token)" != "$before" ]
expect "the token it names exists" [ -n "$($K -n kube-system get secret "bootstrap-token-$(current_token | cut -d. -f1)" --ignore-not-found -o name)" ]
expect "says the write landed" says "$out" "holds what was written"

section "renew --apply when the write's outcome cannot be read back: the token is kept"
cat >"$t/shim/kubectl" <<EOF
#!/bin/sh
# Make the write and fail; from then on, every read of the Secret fails too.
case "\$*" in
*"patch secret"*) "$real" "\$@"; : >"$t/shim/written"; exit 1 ;;
*"get secret hydra-worker-0-bootstrap"*) [ ! -e "$t/shim/written" ] || { echo "error: connection lost" >&2; exit 1; } ;;
esac
exec "$real" "\$@"
EOF
n=$(tokens)
out=$(PATH=$t/shim:$PATH sh "$RENEW" --apply 2>&1)
rc=$?
show "$out"
expect "exits non-zero" [ "$rc" != 0 ]
expect "the token it minted is kept" [ "$(tokens)" = $((n + 1)) ]
expect "says so, and how to tidy up" says "$out" "^Kept token .* in case it did"
expect "never prints a token's secret" lacks "$out" "[a-z0-9]\{6\}\.[a-z0-9]\{16\}"

section "renew --apply when kube-system's Secrets cannot be read: nothing created"
cat >"$t/shim/kubectl" <<EOF
#!/bin/sh
case "\$*" in *"get secret bootstrap-token-"*) echo 'Error from server (Forbidden): secrets is forbidden' >&2; exit 1 ;; esac
exec "$real" "\$@"
EOF
n=$(tokens)
before=$(rv)
out=$(PATH=$t/shim:$PATH sh "$RENEW" --apply 2>&1)
rc=$?
show "$out"
c=0
[ "$rc" != 0 ] && [ "$(tokens)" = "$n" ] && [ "$(rv)" = "$before" ] && c=1
expect "refused, nothing created or written" [ "$c" = 1 ]
expect "says why" says "$out" "could not read kube-system's Secrets to pick an unused token id"

section "renew --apply when cluster-info cannot be read while waiting for the signature"
cat >"$t/shim/kubectl" <<EOF
#!/bin/sh
case "\$*" in *"jws-kubeconfig-"*) echo 'Error from server (Forbidden): configmaps "cluster-info" is forbidden' >&2; exit 1 ;; esac
exec "$real" "\$@"
EOF
n=$(tokens)
before=$(rv)
out=$(SIGN_TIMEOUT=2 PATH=$t/shim:$PATH sh "$RENEW" --apply 2>&1)
rc=$?
show "$out"
c=0
[ "$rc" != 0 ] && [ "$(tokens)" = "$n" ] && [ "$(rv)" = "$before" ] && c=1
expect "rolled back, the Secret unchanged" [ "$c" = 1 ]
expect "reports the failed read, not a missing signature" says "$out" "^could not read kube-public/cluster-info to see whether"

section "renew --apply when the Secret changes again right after the write: the token is kept"
cat >"$t/shim/kubectl" <<EOF
#!/bin/sh
# Let the write through, then change the Secret before it is read back.
case "\$*" in *"patch secret"*)
	"$real" "\$@" || exit
	"$real" patch secret hydra-worker-0-bootstrap --type=merge -p '{"data":{"value":"Y2hhbmdlZAo="}}' >/dev/null
	exit 0 ;;
esac
exec "$real" "\$@"
EOF
n=$(tokens)
out=$(PATH=$t/shim:$PATH sh "$RENEW" --apply 2>&1)
rc=$?
show "$out"
expect "exits non-zero" [ "$rc" != 0 ]
expect "the token it wrote is kept" [ "$(tokens)" = $((n + 1)) ]
expect "says so" says "$out" "^Kept token [a-z0-9]\{6\}\. Run join-token-check.sh"
# Back to a Secret naming a live, signed token for the cases below.
fixture
sh "$RENEW" --apply >/dev/null 2>&1

section "check: a token whose token-id does not match, one with a different secret, then an unsigned cluster-info"
id=$(current_token | cut -d. -f1)
$K -n kube-system patch secret "bootstrap-token-$id" --type=merge -p '{"stringData":{"token-id":"zzzzzz"}}' >/dev/null
out=$(sh "$CHECK" 2>&1)
rc=$?
expect "different token-id: exits 1" [ "$rc" = 1 ]
expect "different token-id: says so" says "$out" "^FAIL  kube-system/bootstrap-token-$id has token-id \"zzzzzz\""
$K -n kube-system patch secret "bootstrap-token-$id" --type=merge -p "{\"stringData\":{\"token-id\":\"$id\"}}" >/dev/null
$K -n kube-system patch secret "bootstrap-token-$id" --type=merge -p '{"stringData":{"token-secret":"ffffffffffffffff"}}' >/dev/null
out=$(sh "$CHECK" 2>&1)
rc=$?
expect "different secret: exits 1" [ "$rc" = 1 ]
expect "different secret: says so" says "$out" "holds a different secret"
signer_off
$K -n kube-public patch cm cluster-info --type=json -p "[{\"op\":\"remove\",\"path\":\"/data/jws-kubeconfig-$id\"}]" >/dev/null
out=$(sh "$CHECK" 2>&1)
rc=$?
expect "unsigned: exits 1" [ "$rc" = 1 ]
expect "unsigned: says so" says "$out" "^FAIL  cluster-info is not signed"

section "expiry: a 1-minute token is refused once it lapses"
fixture
signer_on
TTL_MINUTES=1 sh "$RENEW" --apply >/dev/null 2>&1
signer_off
token=$(current_token)
sleep 65
out=$(sh "$CHECK" 2>&1)
rc=$?
show "$out"
expect "check exits 1" [ "$rc" = 1 ]
expect "check says it expired" says "$out" "expired at"
who=$(whoami_with "$token")
printf '    whoami with the expired token: %s\n' "$who"
expect "the API server refuses it" sh -c 'printf "%s" "$1" | grep -qi unauthorized' _ "$who"

section "TTL_MINUTES=0: a token that never expires, and check warns"
fixture
signer_on
TTL_MINUTES=0 sh "$RENEW" --apply >/dev/null 2>&1
out=$(sh "$CHECK" 2>&1)
rc=$?
show "$out"
expect "check exits 0" [ "$rc" = 0 ]
expect "check warns it never expires" says "$out" "never expires"

section "renew --apply on a cloud-init with no final newline: every byte kept"
fixture
printf '%s' "$(cat "$t/cloud-init")" >"$t/no-newline"
$K create secret generic hydra-worker-0-bootstrap --type=cluster.x-k8s.io/secret --from-file=value="$t/no-newline" \
	--dry-run=client -o yaml | $K apply -f - >/dev/null
userdata >"$t/before"
out=$(sh "$RENEW" --apply 2>&1)
rc=$?
show "$out"
userdata >"$t/after"
expect "exits 0" [ "$rc" = 0 ]
expect "the size is unchanged" [ "$(wc -c <"$t/before")" = "$(wc -c <"$t/after")" ]
expect "it still has no final newline" [ -n "$(tail -c 1 "$t/after")" ]

# The signer stays on below, so a refusal can only come from the guard tested.
section "renew refuses what a new token would not fix"
fixture "$OLD" "$(printf '%064d' 0)"
before=$(rv)
out=$(sh "$RENEW" --apply 2>&1)
rc=$?
show "$out"
c=0
[ "$rc" != 0 ] && [ "$(tokens)" = 0 ] && [ "$(rv)" = "$before" ] && c=1
expect "CA mismatch: refused, nothing created" [ "$c" = 1 ]
expect "CA mismatch: says why" says "$out" "a new token would not make it join"
out=$(sh "$CHECK" 2>&1)
expect "CA mismatch: check reports it" says "$out" "^FAIL  the CA hash"
fixture "$OLD" "$ca_hash" "  - kubeadm join 127.0.0.1:$PORT --token $OLD"
out=$(sh "$RENEW" --apply 2>&1)
rc=$?
show "$out"
c=0
[ "$rc" != 0 ] && [ "$(tokens)" = 0 ] && c=1
expect "two joins: refused, nothing created" [ "$c" = 1 ]
expect "two joins: says why" says "$out" "exactly one .kubeadm join. line"
fixture '$(TOKEN)'
out=$(sh "$RENEW" --apply 2>&1)
rc=$?
show "$out"
c=0
[ "$rc" != 0 ] && [ "$(tokens)" = 0 ] && c=1
expect "no token: refused, nothing created" [ "$c" = 1 ]
expect "no token: says why" says "$out" "has no .--token"
sh "$CHECK" >/dev/null 2>&1
expect "no token: check exits 2" [ "$?" = 2 ]
signer_off

printf '\n%s passed, %s failed\n' "$passes" "$fails"
[ "$fails" = 0 ]
