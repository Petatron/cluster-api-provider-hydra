# Releasing the provider

A release is a git tag `vX.Y.Z` on `main`. Pushing it runs `.github/workflows/release.yml`, which:

1. re-runs the generated-file check, lint, unit tests and the etcd-backup tests, and refuses to
   publish if any fail;
2. builds and pushes `ghcr.io/petatron/cluster-api-provider-hydra:vX.Y.Z` for **linux/amd64 and
   linux/arm64**;
3. renders `out/infrastructure-components.yaml` pointing at exactly that image, plus
   `out/metadata.yaml`;
4. creates the GitHub Release `vX.Y.Z` with those two files attached, which is what `clusterctl`
   downloads.

Every merge to `main` runs steps 1–3 too, publishing `sha-<12 hex>` images, but makes no release.

## Before tagging

- **`metadata.yaml` covers the version.** Each `major.minor` you tag needs a `releaseSeries` entry
  naming the Cluster API contract it implements (v1beta2 for Cluster API v1.14). clusterctl refuses
  a release whose series is missing. Add the entry in the PR that bumps the minor version.
- **Main is green**, including the latest Release run.
- **Versioning.** While the API is `v1alpha1` releases stay at `v0.x`. Bump the minor for new
  behaviour or any CRD change, and the patch for fixes only. A CRD change that is not
  backward-compatible must say so in the release notes.

## Tagging

```bash
git fetch origin
git tag -a vX.Y.Z -m vX.Y.Z origin/main
git push origin vX.Y.Z
```

The image and the Release appear when the workflow finishes, in about seven minutes.

## After tagging

```bash
# The release has both assets
gh release view vX.Y.Z -R Petatron/cluster-api-provider-hydra --json assets --jq '.assets[].name'

# The image is public and multi-arch
docker buildx imagetools inspect ghcr.io/petatron/cluster-api-provider-hydra:vX.Y.Z
```

Then install it into a throwaway kind cluster with the clusterctl config shown in the README
(`clusterctl init --infrastructure hydra:vX.Y.Z`), and check that the provider Deployment becomes
Available and that `clusterctl upgrade plan` lists it.

## Testing a release before tagging

clusterctl reads `file://` repositories laid out as `<dir>/<version>/<file>`, so a local build can be
installed exactly the way a release would be:

```bash
make docker-build IMG=localhost/cluster-api-provider-hydra:dev
make release-manifests IMG=localhost/cluster-api-provider-hydra:dev
mkdir -p /tmp/repo/infrastructure-hydra/v0.1.0
cp out/infrastructure-components.yaml out/metadata.yaml /tmp/repo/infrastructure-hydra/v0.1.0/
cat > /tmp/repo/clusterctl.yaml <<YAML
providers:
  - name: hydra
    type: InfrastructureProvider
    url: file:///tmp/repo/infrastructure-hydra/v0.1.0/infrastructure-components.yaml
YAML

kind create cluster --name hydra-release-test --image kindest/node:v1.35.8
kind load docker-image localhost/cluster-api-provider-hydra:dev --name hydra-release-test
clusterctl init --config /tmp/repo/clusterctl.yaml \
  --core cluster-api:v1.14.0 --bootstrap kubeadm:v1.14.0 --control-plane kubeadm:v1.14.0 \
  --infrastructure hydra:v0.1.0 --wait-providers
clusterctl upgrade plan --config /tmp/repo/clusterctl.yaml
kind delete cluster --name hydra-release-test
```

Use a kind node image at a Kubernetes version Cluster API v1.14 supports for management clusters.
kind's own default can be newer.
