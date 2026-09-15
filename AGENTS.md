# cluster-api-provider-hydra — agent guide

> **Read first:** PR titles are `[PET-xx] Title`. Never put AI attribution in commits or PR bodies,
> but **do** prefix every PR comment you write with `**<Agent> (<Model>)**`. Details in
> [Working agreements](#working-agreements) below.

## What this repo is

The Cluster API **infrastructure provider** for Hydra. It reconciles Cluster API `Machine` objects
into real machines on an on-prem hypervisor — libvirt/KVM first. This is the only Petatron repo
holding product code; everything else is docs or cluster configuration.

The provider owns **only the infrastructure half** of the cluster lifecycle:

| Concern | Owner |
|---|---|
| `Cluster` / `Machine` / `MachineDeployment` lifecycle | Cluster API core |
| Turning a `Machine` into a bootable VM | **this provider** |
| Generating bootstrap data (cloud-init) | Kubeadm Bootstrap Provider (CABPK) |
| Control-plane lifecycle | Kubeadm Control Plane Provider (KCP) |
| Deciding how many machines are needed | Cluster Autoscaler |

The deliberate consequence: this provider implements **no** machine lifecycle of its own, no scaling
logic, and no bootstrap mechanism. It implements the Cluster API infrastructure contract and nothing
more. A change that adds one of those responsibilities is almost certainly wrong — raise it before
writing it.

API group `infrastructure.cluster.x-k8s.io/v1alpha1`:

```
HydraCluster          — one cluster's infrastructure, backing one CAPI Cluster
HydraMachine          — one infrastructure machine, backing one CAPI Machine
HydraMachineTemplate  — the template a MachineDeployment stamps machines from
```

## Toolchain — pinned, and the pins matter

| Thing | Version | Note |
|---|---|---|
| Go | **1.26** | Per `go.mod`. The installed toolchain may be newer; **`go.mod` wins.** |
| Cluster API | **v1.14.0** | Contract **v1beta2** |
| controller-runtime | v0.24.1 | |
| kubebuilder | v4.15.0 | At `~/go/bin/kubebuilder` — **not on `PATH`, use the full path** |

**CAPI v1.14 moved its API types into a separate Go module.** Import from
`sigs.k8s.io/cluster-api/api`, core package `sigs.k8s.io/cluster-api/api/core/v1beta2`. Any guide or
answer citing `api/v1beta1` in the main module is stale and will not resolve — this catches people
constantly.

**libvirt is driven through `github.com/digitalocean/go-libvirt` (pure Go), on purpose.** The
official cgo binding would force abandoning `CGO_ENABLED=0` and the `distroless/static` runtime
image. Do not switch to it.

`controller-gen`, `kustomize`, and the envtest binaries are auto-downloaded into `bin/` by the
Makefile. Docker is required for image builds.

## Never edit these — they are generated and CI verifies them

- `config/crd/bases/*.yaml` — from `make manifests`
- `config/rbac/role.yaml` — from `make manifests`
- `config/webhook/manifests.yaml` — from `make manifests`
- `**/zz_generated.*.go` — from `make generate`
- `PROJECT` — kubebuilder metadata

Generated code, CRDs, and RBAC are **committed to the repo**, and a `verify` CI job fails on any
drift. `make test` deliberately does **not** run `go mod tidy`, so module drift is reported rather
than silently fixed. If `verify` fails, run the generator and commit the result — never hand-edit
the generated file to match.

Do **not** delete `// +kubebuilder:scaffold:*` markers; the CLI injects code at them. Do not move
files — the tooling expects fixed locations. Scaffold new types with
`~/go/bin/kubebuilder create api` rather than writing files by hand.

## After making changes

```bash
make manifests generate   # after editing *_types.go or any kubebuilder marker
make lint-fix             # after editing any *.go
make test                 # unit tests (envtest: real kube-apiserver + etcd)
```

Full local loop before opening a PR: `make manifests generate lint test`.

Other targets: `make run` (run against current kubeconfig), `make build`,
`make docker-build docker-push IMG=…`, `make deploy IMG=…`, `make release-manifests`,
`make test-e2e` (needs an **isolated Kind cluster** — never a real dev or prod cluster;
`make setup-test-e2e` / `make cleanup-test-e2e` manage it).

CI on every PR: `lint`, `test`, `verify`. `test-e2e` runs on `main` and manual dispatch. `release`
publishes to ghcr on `main` and cuts a GitHub Release on `v*` tags.

## Repo-specific traps

**Example manifests go in `docs/examples/`, NOT `config/samples/`.** The samples suite applies every
file under `config/samples/` against envtest, which loads only *this* provider's CRDs — Cluster API's
are not vendored. A `MachineDeployment` or `Cluster` placed there fails to admit for a reason that
has nothing to do with the change under test.

**"We don't watch it" does not mean "we don't cache it."** `mgr.GetClient()` is cache-backed, and
the **first cached `Get` for a kind starts a cluster-wide informer for that kind**. Declining to add
a `Watches()` for Secrets buys nothing while `r.Get` on a Secret still retains every Secret in the
cluster in the manager's memory. Read one-off sensitive objects through **`mgr.GetAPIReader()`**
(uncached) and narrow the RBAC verb to `get` so the informer is not even permitted.

**Never accept a field and ignore it.** A spec field the provider silently drops is worse than one
it rejects. This came out of a review and has recurred.

**Volume reclamation is where the real bugs live.** Two separate defects found in review would have
(a) orphaned every cidata ISO and (b) destroyed an operator's out-of-band volume. Any change to
teardown needs to be reasoned about against a *running* VM, both volumes, and the shared backing
base image, which must survive.

## Conventions for controller code

- **Idempotent reconciliation** — safe to run any number of times.
- **Re-fetch before update** — `r.Get(ctx, req.NamespacedName, obj)` before `r.Update`, to avoid
  conflicts.
- **`metav1.Condition` for status**, not custom string fields. Predefined types (`metav1.Time`) over
  strings.
- **Owner references** (`SetControllerReference`) for garbage collection; **finalizers** for external
  resources — VMs, volumes, DNS.
- **Watch secondary resources** with `.Owns()` / `.Watches()` rather than leaning on `RequeueAfter`.
- **RBAC lives in markers** on the controller, not in hand-edited YAML.
- **Structured logging**, Kubernetes message style: capital letter, no trailing period, active voice,
  past tense, name the object type, balanced key-value pairs.

  ```go
  log := log.FromContext(ctx)
  log.Info("Created Deployment", "name", deploy.Name)
  log.Error(err, "Failed to create Pod", "name", name)
  ```

Tests are **Ginkgo + Gomega**; see `suite_test.go` for setup.

## References

- [Kubebuilder Book](https://book.kubebuilder.io) · [Good practices](https://book.kubebuilder.io/reference/good-practices.html) · [Markers](https://book.kubebuilder.io/reference/markers.html)
- [controller-runtime FAQ](https://github.com/kubernetes-sigs/controller-runtime/blob/main/FAQ.md)
- [Kubernetes API conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md) · [logging conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-instrumentation/logging.md#message-style-guidelines)
- [Cluster API book](https://cluster-api.sigs.k8s.io/)

## Project context

**Hydra** is an open-source on-prem Kubernetes lifecycle platform built on
[Cluster API](https://cluster-api.sigs.k8s.io/), with libvirt/KVM as the first infrastructure
provider and workload-driven node autoscaling via Cluster Autoscaler. It lives on the
[`Petatron`](https://github.com/Petatron) GitHub org across five repositories:

| Repo | Role |
|---|---|
| [`hydra`](https://github.com/Petatron/hydra) | Umbrella / namesake repo. Effectively empty today. |
| [`cluster-api-provider-hydra`](https://github.com/Petatron/cluster-api-provider-hydra) | The Cluster API **infrastructure provider**. Go, kubebuilder. This is where product code lives. |
| [`hydra-bootstrap`](https://github.com/Petatron/hydra-bootstrap) | **Documentation only.** Runbooks for standing up hosts, the CAPI management cluster, and workload clusters. |
| [`hydra-gitops`](https://github.com/Petatron/hydra-gitops) | Argo CD configuration. **A merge to `main` changes live clusters.** |
| [`hydra-infra`](https://github.com/Petatron/hydra-infra) | Terraform for the pre-Hydra, hand-built worker VMs. Legacy path, being superseded by the provider. |

**Where truth lives — three systems, deliberately separated:**

- **Notion** (PETATRON teamspace → *Project Hydra*) — architecture, ADRs, design docs, runbooks,
  worklogs, validation evidence. This is the source of truth for **engineering knowledge**.
- **Linear** (workspace `petatron`, team key `PET`) — issues `PET-5`..`PET-46+`. Source of truth
  for **delivery status**. `PET-1`..`PET-4` are onboarding stubs, not real work.
- **GitHub** — code, PRs, CI. Not a place to record decisions.

Durable findings go in Notion, **not** Linear comments. Every Linear issue must have a matching
Notion page, linked both ways.

## Working agreements

These are instructions, not suggestions.

### 1. PR titles carry the ticket; commit messages do not

```
PR title:  [PET-27] Publish template capacity for scale-from-zero
Commit:    feat: publish template capacity for scale-from-zero
```

The ticket goes in **square brackets at the front** of the PR title. Commit messages stay in
conventional style (`feat:` / `fix:` / `docs:` / `chore:` / `ci:`) with **no** ticket prefix.
`feat: … (PET-27)` is the wrong shape for a PR title — do not copy it.

**Every PR needs a ticket. If there isn't one, stop and ask.** When the work has no Linear (or Jira)
issue, do not open the PR on your own judgment — ask whether to create a ticket first or to open
this one without a prefix, and let the user decide. **Never invent or guess a number:** a wrong
`[PET-xx]` silently attaches the PR to somebody else's work and corrupts the tracking both systems
exist to provide.

### 2. Never put AI attribution in git history

Commit messages, commit trailers, PR titles, and PR bodies must **never** mention Claude, Codex,
Cursor, Copilot, or any AI assistant. Git history records what changed and why, not which tool
typed it. Never emit:

```
Co-Authored-By: Claude <noreply@anthropic.com>
Co-authored-by: Cursor <cursoragent@cursor.com>
🤖 Generated with [Claude Code](https://claude.com/claude-code)
Made with [Cursor](https://cursor.com)
```

Nor phrases like "AI-assisted", "generated by AI", or a model name anywhere in a commit message or
PR description. Write them as the human author would.

**Some tools inject attribution after the fact — always verify.** Writing a clean message is not
enough:

```bash
ATTRIB='^Co-authored-by:|Generated with \[|Generated with Claude|Made with \[|AI-assisted|generated by AI|noreply@anthropic\.com|cursoragent@cursor\.com|🤖'
git cat-file -p HEAD | grep -iE "$ATTRIB" && echo DIRTY || echo clean
gh pr view <n> --json body --jq '.body' | grep -iE "$ATTRIB" && echo DIRTY || echo clean
```

Match the *attribution strings*, **not** product names. Bare `claude`/`cursor` hit the filename
`CLAUDE.md`; bare `Claude Code` hits any commit that legitimately mentions the tool. Both report
clean commits as dirty.

`git commit --amend` re-injects the trailer and cannot fix this; rebuild the commit object with
`git commit-tree` instead. If the bad commit was already pushed, **ask before force-pushing**, then
use `--force-with-lease=<branch>:<old-sha>`.

### 3. DO attribute yourself in PR comments

This is the exception to rule 2, and it is required. Multiple agents (and the human maintainer)
review the same PRs; the comment thread has to say who is speaking.

**Prefix every GitHub PR comment, review comment, review summary, and reply you write with your
agent name and model in bold:**

```
**Claude (Opus 5)** — the informer starts on the first cached Get, so declining
the Watch bought nothing here. Read the Secret through mgr.GetAPIReader().
```

```
**Codex (GPT-5)** — agreed, but the RBAC verb also needs narrowing to `get`.
```

Format is `**<Agent> (<Model>)**` followed by an em dash. Use the name a reader would recognise —
`Claude (Opus 5)`, `Codex (GPT-5)`, `Cursor (Composer)`. If you genuinely do not know your model
identity, use `**<Agent>**` alone rather than guessing.

This applies to **comments only** — the conversation surface. It does **not** apply to the PR body,
the PR title, or commit messages, which stay clean under rule 2.

GitHub still attributes the comment to whichever account is authenticated. The prefix says which
agent wrote the text; do **not** claim the displayed GitHub author has changed.

### 4. Keep the record current continuously, not at the end

Whenever a ticket, a test, or a meaningful piece of progress completes, update **both**:

- **Linear** — status, plus a comment stating what was proven and, just as importantly, what was
  **not**.
- **Notion** — the linked page's worklog section and its Doc Status property.

Write down what was *learned* — especially anything that turned out different from what was
assumed — not just what shipped. The goal is that a future session resumes with no lost context.

Every operation performed against real infrastructure gets recorded as it happens, in both.

### 5. Read PR review comments via GraphQL, never REST

The REST endpoint `/pulls/N/comments` **silently omits threads** — it missed all eight of the
maintainer's review comments on one PR. Always use `reviewThreads`:

```bash
gh api graphql -f query='{repository(owner:"Petatron",name:"<repo>"){pullRequest(number:N){
  reviewThreads(last:80){nodes{isResolved path line
    comments(first:1){nodes{author{login} createdAt body}}}}}}}'
```

Filter by `createdAt` to find new threads. Do not slice by index against the REST count.

### 6. Expect several review rounds, and verify every comment

Copilot reviews every PR and has found dozens of real defects — several that would only have
surfaced in production as orphaned VMs or wedged finalizers. **Expect 3–5 review rounds per PR.**

Verify each comment against the actual file before acting on it. Some have been stale test
assumptions rather than code bugs, and at least one was a linter firing on a conversion that was
genuinely required. A review comment is evidence, not a verdict.

### 7. Generic design beats lab convenience

The reference lab (an XPS 13 control plane at home, a workstation at the office, ~30 ms apart) is
one person's setup, **not a requirement Hydra should be shaped around**. Hydra must assume nothing
about that hardware, topology, or site count.

When lab convenience and generic design diverge, **generic wins** — unless the shortcut is provably
free, meaning a config value that can change later, not a code path that would have to be undone.

Architecture decisions carry a **Scope** property in Notion: `Product` binds Hydra for everyone,
`Reference lab` describes only that setup. Do not read a lab ADR as a product constraint.
