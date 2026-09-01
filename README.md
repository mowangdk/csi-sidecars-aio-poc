# CSI Sidecars Monorepo

The [Container Storage Interface (CSI)](https://kubernetes-csi.github.io/docs/)
is the standard for exposing storage systems to containerized workloads on
Kubernetes. Storage vendors implement the CSI specification in a *CSI driver*;
the Kubernetes-specific glue around that driver is provided by a set of common
components maintained by the SIG Storage community in the
[kubernetes-csi](https://github.com/kubernetes-csi) organization:

- **Sidecars**, deployed as containers next to the CSI driver:
  - [external-provisioner](https://github.com/kubernetes-csi/external-provisioner) — watches PVCs and calls `CreateVolume`/`DeleteVolume`.
  - [external-attacher](https://github.com/kubernetes-csi/external-attacher) — watches VolumeAttachments and calls `ControllerPublishVolume`/`ControllerUnpublishVolume`.
  - [external-resizer](https://github.com/kubernetes-csi/external-resizer) — watches PVCs and calls `ControllerExpandVolume`.
  - [external-snapshotter](https://github.com/kubernetes-csi/external-snapshotter) — watches VolumeSnapshots and calls `CreateSnapshot`/`DeleteSnapshot`.
  - [node-driver-registrar](https://github.com/kubernetes-csi/node-driver-registrar), [livenessprobe](https://github.com/kubernetes-csi/livenessprobe), and others.
- **Controllers and webhooks** that are deployed cluster-wide rather than as
  sidecars, e.g. `snapshot-controller` and the CSI snapshot validation webhook.
- **Shared libraries and tooling**: [csi-lib-utils](https://github.com/kubernetes-csi/csi-lib-utils)
  (metrics, RPC helpers, ...) and [csi-release-tools](https://github.com/kubernetes-csi/csi-release-tools)
  (build/release/CI plumbing).

Because every CSI driver ships most of these components alongside its own
driver image, the common components multiply the release, update and resource
cost across the ecosystem.
[KEP-4958: CSI Sidecars All in one](https://github.com/kubernetes/enhancements/pull/5153)
addresses this by combining the source code of the CSI sidecars in a monorepo.
Instead of just putting the code repositories together, the program entries of
all sidecars are consolidated into a single artifact (binary and container
image), similar to how `kube-controller-manager` operates. Among the benefits
are:

- Improve the CSI sidecar release process by reducing the number of components released.
- Decrease the maintenance tasks the SIG Storage community maintainers do to maintain the sidecars.
- Propagate changes in common libraries used by CSI Sidecars immediately instead of through additional PRs.
- Reduce the number of components CSI Driver authors and cluster administrators need to keep up to date in k8s clusters.

As a side effect we also:

- Reduce the memory usage/API server calls done by the CSI Sidecars through the usage of a shared informer.
- Reduce the cluster resource requirements needed to run the CSI Sidecars.

See the [KEP](https://github.com/kubernetes/enhancements/pull/5153) for the full
motivation, quantified benefits and risk analysis.

## Design overview

The key design points from the KEP as implemented (or targeted) by this repo:

- **Single artifact**: one `csi-sidecars` binary/container image that enables
  sidecars selectively, kube-controller-manager style, through a
  `--controllers` flag, e.g. `--controllers=attacher,provisioner,resizer,snapshotter`.
- **Command line split in two types**: global flags configured once for all
  controllers (e.g. `--csi-address`, `--leader-election`, `--timeout`), and
  per-controller flags prefixed with the controller name
  (e.g. `--attacher-timeout`, `--attacher-worker-threads`).
- **Standalone binaries stay standalone**: `snapshot-controller` and
  `snapshot-conversion-webhook` are *not* true sidecars and are not deployed
  with the CSI driver, so they are built as separate binaries from the same
  monorepo instead of being merged into `csi-sidecars`.
- **Code synchronization**: during the transition phase (before the individual
  repositories are deprecated), changes are synced from the individual
  `kubernetes-csi/external-*` repositories by `./tools/scripts/sync.sh`,
  which also performs the import path rewrites required by the monorepo.
- **Individual repo history preserved**: each sidecar is cloned with
  `git-filter-repo`, keeping the full commit history available for
  `git blame`/`git log` traceability.
- **Reproducible builds**: a single generated `go.mod`/`go.work` at the
  repository root; synced components do not carry their own `go.mod`.
- **RBAC**: mirrors the individual repositories, each controller keeps its own
  policy; driver maintainers apply the RBAC of the controllers they enable.

## Scope and status

The KEP targets these components:

- kubernetes-csi/external-attacher
- kubernetes-csi/external-provisioner
- kubernetes-csi/external-resizer
- kubernetes-csi/external-snapshotter
- kubernetes-csi/livenessprobe
- kubernetes-csi/node-driver-registrar
- kubernetes-csi/external-health-monitor (volume-health-monitor)
- kubernetes-csi/volume-data-source-validator

This proof-of-concept currently syncs four of them (see
[`tools/scripts/sidecars.conf`](./tools/scripts/sidecars.conf)):

- kubernetes-csi/external-attacher
- kubernetes-csi/external-provisioner
- kubernetes-csi/external-resizer
- kubernetes-csi/external-snapshotter

The snapshotter integration additionally produces two standalone binaries,
`snapshot-controller` and `snapshot-conversion-webhook`, alongside the merged
`csi-sidecars` binary.

## Usage

A CSI driver deployment replaces the individual sidecar containers with a
single `csi-sidecars` container. Control plane example (the same style used by
the hostpath e2e deployment in [`deploy/`](./deploy/)):

```yaml
kind: Deployment
apiVersion: apps/v1
metadata:
  name: csi-driver-deployment
spec:
  replicas: 1
  template:
    spec:
      containers:
        - name: csi-driver
          args:
            - "--v=5"
            - "--endpoint=unix:/csi/csi.sock"
        - name: csi-sidecars
          command:
            - csi-sidecars
            - "--csi-address=unix:/csi/csi.sock"
            # similar style as kube-controller-manager
            - "--controllers=attacher,provisioner,resizer,snapshotter"
            - "--feature-gates=Topology=true"
            # leader election flags for all the components as one
            - "--leader-election"
            - "--leader-election-namespace=kube-system"
            # global timeouts
            - "--timeout=30s"
            # per controller specific flags are prefixed with the controller name
            - "--attacher-timeout=30s"
            - "--attacher-worker-threads=100"
            - "--provisioner-volume-name-prefix=pvc"
          volumeMounts:
            - mountPath: /csi
              name: socket-dir
```

Once the node-side components (e.g. `node-driver-registrar`, `livenessprobe`)
are integrated, the same image will serve the node pools with a different
`--controllers` value, e.g. `--controllers=node-driver-registrar`.

## Development

Requirements:

- go 1.26
- python 3 (for `git-filter-repo`)

### Building the project locally

After cloning the repo, run the following commands to start from scratch:

```bash
# cleanup first
./tools/scripts/cleanup.sh
# setup venv, clone repos with history, setup go workspaces and build
python3 -m venv .venv && source .venv/bin/activate
./tools/scripts/sync.sh 2>&1 | tee tools/sync.log
```

Logs: [./tools/sync.log](./tools/sync.log)

The sync script clones each sidecar repo preserving their commit history
(using `git-filter-repo`). The full commit history is available at `pkg/<sidecar>/.git`.

To change which sidecars are synced or from which branch, edit
[`tools/scripts/sidecars.conf`](./tools/scripts/sidecars.conf). Makefile
shortcuts wrap the same scripts: `make sync` and `make clean`.

See [CODE_LAYOUT.md](./CODE_LAYOUT.md) for the dual-layer layout that separates
the hand-maintained `tools/` source of truth from the generated assembly area.

### Building the project using CI

There's a presubmit job that runs on every PR using Github Actions,
to run the action locally install https://github.com/nektos/act and run:

```bash
act push
```

The presubmit workflow (`.github/workflows/presubmit.yaml`) has these jobs:

- `verify` — code-quality gates on the hand-maintained source of truth under
  `tools/`: `gofmt`, Apache-2.0 boilerplate license headers, and
  `shellcheck` (severity `warning`) on the scripts we own. The generated
  assembly area (`cmd/`, `pkg/`, `staging/`) mirrors upstream code and is
  covered by each upstream project's own CI, so it is intentionally not
  re-verified here.
- `build` — runs the full sync and builds the merged binary.
- `unit` — runs `go test` and `go vet` over the hand-maintained packages.
- `e2e-hostpath` — runs the Hostpath CSI driver e2e suite via `.prow.sh`.

Supplementary workflows: `trivy.yaml` (report-only image vulnerability scan) and
`codespell.yml` (spelling).

### E2E tests through the Hostpath CSI Driver

- Go over the slides above.
- Make sure that the project was built locally. The `sync.sh` command should exit with status code 0.

WARNING: The following nukes your $GOPATH/src/k8s.io/ directory. Please read .prow.sh.log
and find the `git clean -fdx` command (which removes untracked files).

```bash
./.prow.sh 2>&1 | tee ./.prow.sh.log
```

Common errors:

- `ERROR: failed to clean $GOPATH/src/k8s.io/kubernetes`, csi-release-tools doesn't work fine
  if there's a local copy of the kubernetes codebase already. Remove it and try again.
- `403 on pulling CSI manifests from github`. Due to throttling, try again.

## Resources

- [KEP-4958: CSI Sidecars All in one](https://github.com/kubernetes/enhancements/pull/5153)
- [Enhancement issue kubernetes/enhancements#4958](https://github.com/kubernetes/enhancements/issues/4958)
- [Presentation](https://www.youtube.com/watch?v=hZpgLqys_lQ&t=1742s)
- [Slides](https://docs.google.com/presentation/d/1lldJYYf2WVxgv4O3Wgdefrq3ZAN7s3Mwx-GL_CRmakE/)
- [Design doc](https://docs.google.com/document/d/1z7OU79YBnvlaDgcvmtYVnUAYFX1w9lyrgiPTV7RXjHM/)
