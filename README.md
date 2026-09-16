# CSI Sidecars Monorepo

This repository is an experimental proof-of-concept implementation of
[KEP-4958: CSI Sidecars All in one](https://github.com/kubernetes/enhancements/pull/5153).
It is **not production-ready**. The Kubernetes community resources referenced
below describe the upstream ecosystem, not a claim that this PoC has completed
formal SIG project onboarding or has an official release/support commitment.

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
  sidecars, e.g. `snapshot-controller` and `snapshot-conversion-webhook`.
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

The design also targets lower memory usage, fewer API server calls through
shared informers, and lower cluster resource requirements. Shared informers
across controllers are not implemented in this PoC yet; these benefits still
need to be measured against equivalent standalone sidecars.

See the [KEP](https://github.com/kubernetes/enhancements/pull/5153) for the full
motivation, quantified benefits and risk analysis.

## Design overview

The key design points from the KEP as implemented (or targeted) by this repo:

- **Single artifact**: one `csi-sidecars` binary/container image that enables
  sidecars selectively, kube-controller-manager style, through a
  `--controllers` flag, e.g. `--controllers=attacher,provisioner,resizer,snapshotter`.
- **Command line split in two types**: global flags configured once for all
  controllers (e.g. `--csi-address`, `--leader-election`, `--kube-api-qps`), and
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
- **Unified dependency workspace**: a generated `go.mod`/`go.work` at the
  repository root; synced sidecar components do not carry their own `go.mod`.
  The assembly area (including `go.mod`/`go.work` and `vendor/`) is committed,
  so a checkout builds directly with a plain `go build`; `sync.sh` regenerates
  it from the locked source revisions, builder tools, and runtime images. See
  the [build workflow](./tools/README.md) for details.
- **RBAC**: the design reuses each enabled controller's upstream policy. The
  current hostpath test deployment references fixed, older RBAC versions; these
  are not generated from the synced source revisions. Driver maintainers must
  verify permissions against the actual controller versions and features they
  enable rather than treating the test deployment as a production installer.

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

### Validation scope

The following describes the current CI configuration, not a production
compatibility or support matrix. A passing build or CLI smoke test does not
establish that a controller works correctly in a cluster.

| Area | Current validation |
|------|--------------------|
| Linux amd64 | Locked presubmit build configured; native execution acceptance remains pending. |
| Linux arm64 | Local pre-commit assembly passed; see [branch evidence](./tools/README.md#standalone-branch-validation). Not an automated CI matrix entry. |
| Hostpath e2e | Kubernetes 1.31.9, with attacher, provisioner, and resizer in AIO and snapshotter in a separate upstream container. |
| Four controllers together | Build and CLI coverage, not complete in-cluster functional coverage. |
| Standalone snapshot-controller and webhook | Build and image/CLI smoke checks, not complete functional coverage. |
| Race detection | Maintained entrypoint/config packages only; no lifecycle integration coverage. |
| HA, upgrades and rollback | Not covered by a complete automated test suite. |
| Vulnerabilities | Daily and PR image scans are report-only; success does not mean the images have no vulnerabilities. |

### Current limitations

This branch preserves the entrypoint and flag code from `0e07ce4`; it excludes
the runtime lifecycle work in `444a2b4`. Build verification does not establish
coordinated shutdown, lease draining, or production readiness.

- Hostpath e2e currently enables only attacher, provisioner, and resizer in AIO;
  snapshotter runs in a separate upstream container. It does not yet validate
  the four-controller configuration below or our standalone snapshot images.
- Common leader-election flags configure the individual controller elections;
  they do not establish one process-wide election. Shared informers, unified
  health/metrics serving, and coordinated graceful shutdown are still pending.
- With multiple controllers enabled, do not set `--http-endpoint` or
  `--metrics-address` yet: individual controllers attempt to bind the same
  address. The combined process is not production-ready.
- The official release pipeline is not wired up. The presence of vendored
  `release-tools` and `.cloudbuild.sh` does not establish a working release
  process: there is no root `cloudbuild.yaml`, and the cloud-build entrypoint
  does not assemble this repository's generated source tree.

## Images and build environment

A successful sync builds the following binaries under `bin/`. Running
the inherited `make container BUILD_ARCH=arm64` (or `amd64`) target names
local images as follows. It requires the verified builder for compilation and
a Docker-capable packaging environment; this combined image path is not yet
validated with the locked builder:

| Local image | Purpose | Dockerfile |
|-------------|---------|------------|
| `csi-sidecars:latest` | Runs the selected driver-side controllers. | `Dockerfile` |
| `snapshot-controller:latest` | Runs the cluster-wide snapshot controller separately. | Generated `cmd/snapshot-controller/Dockerfile` |
| `snapshot-conversion-webhook:latest` | Runs the snapshot conversion webhook separately. | Generated `cmd/snapshot-conversion-webhook/Dockerfile` |

All three use the same digest-pinned `gcr.io/distroless/static` runtime index
from [`images.lock.json`](./tools/assembly/images.lock.json), with checked Linux
amd64/arm64 manifests. One maintained template verifies the root Dockerfile and
generates both standalone Dockerfiles. Go compilation happens outside these
Dockerfiles; they copy the already-built binaries into a minimal image without
a shell or package manager.
The verify/build CI jobs compile in the digest-pinned Linux builder; the runtime
image is not the build environment.

The Dockerfiles still inherit the base image's root user. Non-root execution
remains future work, with socket permissions, certificate access, and listening
ports to be validated before changing the runtime user. The image lock also pins
the existing Kubernetes 1.31.9 regression node image and matching kind release;
this is not a production support matrix. See the
[image-input workflow](./tools/README.md#runtime-and-legacy-test-image-inputs).

These local image names are development artifacts, not official registry pull
locations or stable releases. Image build success does not imply that release
publishing, signing, or promotion has been configured.

## Usage

A CSI driver deployment replaces the individual sidecar containers with a
single `csi-sidecars` container. The following is an illustrative control-plane
manifest fragment, not an installable Deployment: supply driver/image details,
selectors and labels, a service account with matching RBAC, and socket volumes.
See [`deploy/`](./deploy/) for the narrower hostpath test deployment.

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
          args:
            # BEGIN AIO CLI ARGS
            - "--csi-address=/csi/csi.sock"
            - "--controllers=attacher,provisioner,resizer,snapshotter"
            # Common settings for the individual controller elections
            - "--leader-election"
            - "--leader-election-namespace=kube-system"
            # No global --timeout flag is currently registered
            - "--attacher-timeout=30s"
            - "--resizer-resize-timeout=30s"
            - "--resizer-modify-timeout=30s"
            - "--snapshotter-timeout=30s"
            - "--attacher-worker-threads=100"
            - "--provisioner-volume-name-prefix=pvc"
            # END AIO CLI ARGS
          volumeMounts:
            - mountPath: /csi
              name: socket-dir
```

Timeout configuration is still transitional: `--attacher-timeout` also supplies
the legacy `timeout`/`operationTimeout` globals consumed by the merged code.
Resizer operation timeouts and snapshotter RPC timeouts have their own flags as
shown above; a global timeout/override precedence contract is not implemented.
CI checks that these arguments parse, not that this fragment is deployable.

Once the node-side components (e.g. `node-driver-registrar`, `livenessprobe`)
are integrated, the same image will serve the node pools with a different
`--controllers` value, e.g. `--controllers=node-driver-registrar`.

## Development

### Requirements

- **Source sync/build:** Python 3.9+ and Git on the host, plus a Linux container
  engine. Local assembly and CI verify/build jobs select the same per-architecture
  builder digest from [`build-environment.lock.json`](./tools/assembly/build-environment.lock.json).
  It requires Go **1.26.5**, Python **3.13.5**, GCC **14.2.0**, and Git **2.47.3**.
  These are selected build inputs, not a production compatibility certification.
- **Python generation tools:** a fresh private environment installs only hashed
  pip **26.2** and git-filter-repo **2.47.0** wheels. System pip and caller venvs
  are not reused. Network access to the pinned image/wheels, upstream source
  repositories, and Go dependency services is required.
- **Image building:** `make container` currently invokes the Docker CLI and
  requires a running engine for both image building and verification.
- **Cluster e2e:** additionally requires a Docker-capable Linux environment and
  permissions for the kind/driver test setup. Use an isolated GOPATH because the
  test tooling checks out and cleans repositories there.

Run the shell examples below in **Bash**, from the repository root in a Linux
environment.

### Building the project locally

The assembly area is committed, so from a populated tree you can build directly
with a plain compile (no sync step):

```bash
make build
```

To regenerate the assembly area (for example after an upstream revision bump),
including from macOS with a Linux container engine, preload the locked builder
image and run the isolated helper:

```bash
podman pull "$(python3 -B tools/scripts/build_environment.py image)"
python3 -B tools/scripts/isolated_sync.py --engine podman --update-dependencies 1.MINOR.PATCH
```

Use `docker pull` and `--engine docker` for Docker. Arbitrary image overrides
are rejected. `--tooling-only` exercises tooling tests, gofmt, and license
checks without assembling.

The helper copies tracked working files and new maintained files under `tools/`
to a fresh `.work/assembly-*/source` directory, then runs sync only on that copy.
It preserves edits, leaves developer caches and unrelated untracked files out,
and retains the assembly log and source for diagnosis. It does not mount the
original checkout, kubeconfig, or engine socket into the container. Sync
generates `go.mod`/`go.work` from the locked source revisions and vendors the
dependencies. This helper is not a release guarantee.

See [tools/README.md](./tools/README.md) for update, packaging, and image
details.

### Building the project using CI

Presubmit verify/build jobs use the locked helper for tooling checks and to
generate and compile the assembly. The legacy hostpath E2E job is not migrated
to that helper and has not been validated for this branch.

### E2E tests through the Hostpath CSI Driver

- Read the [hostpath deployment notes](deploy/README.md) and the
  [presentation slides](https://docs.google.com/presentation/d/1lldJYYf2WVxgv4O3Wgdefrq3ZAN7s3Mwx-GL_CRmakE/).
- Make sure the local build completed successfully before starting e2e.
- Review the limited controller/version coverage in [Validation scope](#validation-scope).

**Warning:** `.prow.sh` checks out and cleans repositories under GOPATH,
including the Kubernetes source tree. Its `git clean -fdx` operations can remove
untracked work. Run it in a disposable Linux environment with a fresh GOPATH,
not one containing development checkouts. The tracked [.prow.sh.log](./.prow.sh.log)
is a historical reference, not the result of this invocation.

```bash
set -euo pipefail
GOPATH=$(mktemp -d "${TMPDIR:-/tmp}/csi-sidecars-e2e-gopath.XXXXXX")
export GOPATH
e2e_log=$(mktemp "${TMPDIR:-/tmp}/csi-sidecars-e2e.XXXXXX")
./.prow.sh 2>&1 | tee "$e2e_log"
printf 'E2E log: %s\nDisposable GOPATH: %s\n' "$e2e_log" "$GOPATH"
```

Common errors:

- `ERROR: failed to clean $GOPATH/src/k8s.io/kubernetes`: retry with a fresh,
  isolated GOPATH rather than deleting an existing development checkout.
- `403 on pulling CSI manifests from github`. Due to throttling, try again.

## Resources

- [KEP-4958: CSI Sidecars All in one](https://github.com/kubernetes/enhancements/pull/5153)
- [Enhancement issue kubernetes/enhancements#4958](https://github.com/kubernetes/enhancements/issues/4958)
- [Presentation](https://www.youtube.com/watch?v=hZpgLqys_lQ&t=1742s)
- [Slides](https://docs.google.com/presentation/d/1lldJYYf2WVxgv4O3Wgdefrq3ZAN7s3Mwx-GL_CRmakE/)
- [Design doc](https://docs.google.com/document/d/1z7OU79YBnvlaDgcvmtYVnUAYFX1w9lyrgiPTV7RXjHM/)
