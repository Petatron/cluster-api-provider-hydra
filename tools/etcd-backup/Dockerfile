# etcd-backup: etcdctl/etcdutl from the official etcd image, plus a shell,
# age, curl, jq and openssl. The official image is distroless -- it has the
# binaries but no shell to drive them -- so they are copied into alpine.
#
# Keep ETCD_IMAGE at the workload clusters' etcd version: snapshot save works
# across minor versions, but etcdutl's snapshot status should read the format
# the members write.
ARG ETCD_IMAGE=registry.k8s.io/etcd:3.6.6-0

FROM ${ETCD_IMAGE} AS etcd

FROM alpine:3.22
# Links the ghcr package to this repository. A new package can still start out
# private; the management cluster pulls without credentials, so make it public.
LABEL org.opencontainers.image.source=https://github.com/Petatron/cluster-api-provider-hydra
LABEL org.opencontainers.image.description="Encrypted etcd backups of Cluster API workload clusters, taken from the management cluster"
RUN apk add --no-cache age curl jq openssl
COPY --from=etcd /usr/local/bin/etcdctl /usr/local/bin/etcdutl /usr/local/bin/
COPY --chmod=0755 etcd-backup.sh /usr/local/bin/etcd-backup
# The tests run against stubs, so prove here that the real binaries start in
# this image: a moved or dynamically linked one fails the build, not the first
# scheduled backup.
RUN etcdctl version && etcdutl version && age --version && jq --version && curl --version >/dev/null && openssl version
ENTRYPOINT ["/usr/local/bin/etcd-backup"]
