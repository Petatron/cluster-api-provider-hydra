# Releasing the provider

**A release is a PR that changes `VERSION`** (PET-92). The file at the repository root holds the
version `main` is at, without the `v` — `0.2.0`. When a commit reaches `main` with a `VERSION` that
has no tag yet (normally the bump's merge commit; a later one if that run was queued behind another
and superseded), `.github/workflows/release.yml`:

1. re-runs the generated-file check, lint, unit tests and the etcd-backup tests, and refuses to
   publish if any fail;
2. creates the annotated tag `vX.Y.Z` on that commit, then builds and pushes `ghcr.io/petatron/cluster-api-provider-hydra:vX.Y.Z` for **linux/amd64 and
   linux/arm64**, and tags the same image `sha-<12 hex>` like every other main commit;
3. renders `out/infrastructure-components.yaml` pointing at exactly that image, plus
   `out/metadata.yaml`;
4. creates the GitHub Release `vX.Y.Z` with those two files attached, which is what `clusterctl`
   downloads. A version with a `-` (`0.3.0-rc.1`) is marked a pre-release. The etcd-backup image is
   published last, under both `vX.Y.Z` and `sha-<12 hex>` too.

Every other merge to `main` runs steps 1–3 under `sha-<12 hex>` only, with no tag and no release.
If `VERSION` on `main` is not releasable (malformed, behind the newest tag, or missing from
`metadata.yaml`), the `sha-` images are still published and the run then fails.

## Cutting a release

```bash
git switch -c pet-NN/release-vX.Y.Z origin/main
make bump-patch      # fixes only
make bump-minor      # new behaviour or any CRD change; also adds the metadata.yaml releaseSeries entry
make bump-major      # reserved for leaving v1alpha1
git add VERSION metadata.yaml && git commit -m "chore: release vX.Y.Z"
```

Open the PR as usual (`[PET-NN] Release vX.Y.Z`). The bump can also ride in the PR that carries the
change. Its **`VERSION is releasable`** check (`make check-version`, in `verify.yml`) fails when
`VERSION` is malformed, behind the newest `v*` tag, or has no `metadata.yaml` `releaseSeries` entry
for its `major.minor` — clusterctl refuses such a release. The check's notice says which tag the
merge will create.

A pre-release is written by hand: put `0.3.0-rc.1` in `VERSION`. `make bump-*` drops the `-rc.N`
suffix while it bumps, so to finalise `0.3.0-rc.2` write `0.3.0` by hand too.

Merging releases. The image and the Release appear when the workflow finishes, in about seven
minutes. Runs on `main` are serialised, and a tag the workflow creates with `GITHUB_TOKEN` does not
start another run, so a version cannot be released twice. If a run fails, see
[Recovering a release](#recovering-a-release).

## Before merging a bump

- **Main is green**, including the latest Release run.
- **Versioning.** While the API is `v1alpha1` releases stay at `v0.x`. Bump the minor for new
  behaviour or any CRD change, and the patch for fixes only. A CRD change that is not
  backward-compatible must say so in the release notes.
- **The contract is right.** `make bump-minor` copies the last `releaseSeries` contract (v1beta2 for
  Cluster API v1.14). Change it if the release moves to a new Cluster API contract.

## Fallback: tagging by hand

Pushing a `v*` tag still runs the same release, for when the `VERSION` path is broken:

```bash
git fetch origin
git tag -a vX.Y.Z -m vX.Y.Z origin/main
git push origin vX.Y.Z
```

Then set `VERSION` to that version in a PR. Until then it is behind the newest tag, and every PR's
`VERSION is releasable` check fails (main keeps publishing `sha-` images).

**Never hand-tag a version that `VERSION` already names but has not released yet.** If both runs
are in flight, the `main` run's tag step fails before it pushes a `vX.Y.Z` image. But it can still
have read the tag as absent. The tag run then builds and pushes `vX.Y.Z` from the tagged commit.

## Recovering a release

The tag is the lock: once `vX.Y.Z` names a commit, only that commit can produce the release.

- **The run failed after the tag was created.** Re-run it, with either "Re-run failed jobs" or
  "Re-run all jobs". The version job sees the tag on this commit with no Release and finishes
  the release. If a Release already exists, its assets are re-uploaded.
- **The tag names a commit that can never build**, or a newer commit's run warns
  "`vX.Y.Z` has no GitHub Release and names another commit". Delete the tag and let the next
  `main` run release from its own commit, or bump `VERSION` again:

  ```bash
  git push origin :refs/tags/vX.Y.Z
  ```

- **The run failed before the tag was created.** Fix it forward. The next green `main` run whose
  `VERSION` is still untagged releases from that commit.


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
