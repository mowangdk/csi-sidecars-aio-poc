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
- **Independent upstream sources**: all four `kubernetes-csi/external-*`
  repositories retain their own source ownership and release streams. This
  project's assembly does not replace or deprecate them. `./tools/scripts/sync.sh`
  imports their sources and performs the import and AIO lifecycle adaptations.
- **Individual repo history preserved**: each sidecar is cloned with
  `git-filter-repo`, keeping the full commit history available for
  `git blame`/`git log` traceability.
- **Unified dependency workspace**: a generated `go.mod`/`go.work` at the
  repository root; synced sidecar components do not carry their own `go.mod`.
  Release reproducibility is **not established yet**. The four controllers and
  `csi-lib-utils` use exact original revisions. Builder, runtime, and selected
  legacy test images are locked. The replay-validated source/dependency bundle
  is activated locally for normal builds. Full offline assembly, native
  architecture coverage, and OCI image reproducibility remain unfinished.
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
| Linux amd64 | Presubmit is configured for locked binary builds and maintained-package tests; native acceptance is pending. |
| Linux arm64 | Not yet an automated CI matrix entry. |
| Hostpath e2e | Kubernetes 1.31.9, with attacher, provisioner, and resizer in AIO and snapshotter in a separate upstream container. |
| Four controllers together | Local fake Kubernetes/CSI startup, cancellation, connection/lease failure, and in-flight snapshot cancellation tests with election enabled/disabled; not in-cluster storage-operation coverage. |
| Standalone snapshot-controller and webhook | Build and image/CLI smoke checks, not complete functional coverage. |
| Race detection | Bounded tests for AIO supervision, election, startup helpers, checked adapters, and assembled-controller cancellation. |
| HA, upgrades and rollback | Not yet covered by a complete automated test suite. |
| Vulnerabilities | Daily and PR image scans are report-only; success does not mean the images have no vulnerabilities. |

### Current limitations

- Hostpath e2e currently enables only attacher, provisioner, and resizer in AIO;
  snapshotter runs in a separate upstream container. It does not yet validate
  the four-controller configuration below or our standalone snapshot images.
- The active source/dependency pair selects the replay-validated Kubernetes
  1.36.3 dependency baseline, with staging modules aligned at 0.36.3. It replaces
  the incompatible historical combination of 0.37 source requirements and 0.36.1
  replacements. This is a local assembly baseline, not a production Kubernetes
  support claim or security approval. The mandatory compatibility and checksum
  gates still reject stale, mismatched, or modified inputs.
- Common leader-election flags configure the individual controller elections;
  they do not establish one process-wide election. Shared informers and unified
  health/metrics serving are still pending.
- With multiple controllers enabled, `--http-endpoint` and `--metrics-address`
  are rejected before clients or sockets are opened. The two flags are also
  mutually exclusive in single-controller mode. Unified serving is not implemented.
- The launcher supervises named runners and requests cancellation on SIGTERM or
  SIGINT. `--shutdown-timeout` defaults to `25s` and must be positive; it starts
  when shutdown begins, not at process startup. A second signal forces a nonzero
  exit. Set the Pod termination grace period above this timeout plus log-flush
  and scheduling time (40 seconds for the default timeout).
- Checked AIO-only adaptations remove private entrypoint signals and process
  exits, own CSI connections and HTTP services, and track controller workers and
  provisioner helper loops. Unknown lifecycle shapes fail assembly. Standalone
  commands retain their original signal/election behavior.
- The AIO election adapter continues renewal during worker drain and only
  releases a lease afterward when the controller's release-on-exit policy permits
  it. Unexpected leadership loss or runner failure cancels peers and exits
  nonzero. A shutdown timeout is a failure, not successful drain, and does not
  trigger an early lease release. Real storage-operation/HA acceptance is still
  pending; the combined process remains experimental and not production-ready.
- Logging feature gates and configuration are applied once before runners start;
  `--logging-format=json` and verbosity settings apply to the AIO process.
  The historical `--attacher-max-grpc-log-length` option configures a shared
  gRPC response-log limit when attacher is enabled; it is initialized before any
  runner starts and does not provide per-controller logging isolation.
  `--version` returns before controller selection or network initialization.
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

For an isolated Linux assembly, including from macOS with a Linux container
engine, explicitly preload the locked image, then use the isolated helper:

```bash
podman pull "$(python3 -B tools/scripts/build_environment.py image)"
python3 -B tools/scripts/isolated_sync.py --engine podman
```

Use `docker pull` and `--engine docker` for Docker. Arbitrary image overrides
are rejected for fresh assembly. `--tooling-only` exercises tooling tests,
gofmt, and license checks without needing an adopted dependency bundle.

This copies tracked working files and new maintained files under `tools/` to a
fresh `.work/assembly-*/source` directory, then runs sync only on that copy. It
preserves edits, leaves developer caches and unrelated untracked files out, and
retains the assembly log and source for diagnosis. It does not mount the original
checkout, kubeconfig, or engine socket into the container. A fresh run also
executes bounded maintained-package race tests, vet, and the README CLI smoke
check. Normal sync consumes the locally activated source and canonical dependency
locks under `tools/assembly/`, without update or candidate-selection flags.
Builder tools, shared runtime images, and the selected legacy Kubernetes node
image are locked; full build/release acceptance remains open. This helper is not
a release guarantee.

For fast regression testing, `--image YOUR_HISTORICAL_BUILD_IMAGE` together with
`--runtime-baseline .work/assembly-.../source` replays a retained **pre-adaptation** assembly with the current maintained tools
and runs race tests against local fake services with container networking disabled.
The replay freezes a separate copy and rejects unknown/already-adapted entrypoints.
It is supplemental runtime evidence, not a clean assembly or a substitute for
functional storage tests.

Each isolated invocation starts from a fresh snapshot and retains its own
`assembly.log`; no workspace cleanup is needed. Direct `sync.sh`/`make sync`
invocations are in-builder operations and reject existing generated trees or
partial `.assembly-env` installations. The tracked
[tools/sync.log](./tools/sync.log) is historical reference, not current evidence.

The sync script clones each sidecar repo preserving their commit history
(using `git-filter-repo`). The merged history is available at `tmp/csi-sidecars/`;
`pkg/<sidecar>/` contains the processed source files, not a Git checkout.

Normal sync consumes [`tools/assembly/sources.lock.json`](./tools/assembly/sources.lock.json),
not branch heads. It records the selected original revisions and source-lock hash
in `tmp/source-provenance.json` before rewriting source history. The active source
selection matches the canonical dependency bundle; provisioner and csi-lib-utils
use the compatible revisions validated by fresh assembly and locked replay.
The other three controller revisions remain unchanged.

To propose updates, edit the branch-channel metadata in
[`tools/scripts/sidecars.conf`](./tools/scripts/sidecars.conf), then run:

```bash
python3 tools/scripts/isolated_sync.py --engine podman \
  --update-sources --update-dependencies 1.36.3 \
  --candidate-output .work/dependencies.candidate.json
```

This resolves all five sources and dependencies in a fresh copy. The explicit
Kubernetes release must match the sources' core minor and satisfy their patch
minimums; the example is an investigation target, not a support claim. Only
successful dependency checks, assembly, maintained race tests, vet, and CLI
checks export the complete bundle; an existing candidate is never overwritten.
The active lock is never changed automatically. To test selected revisions without
moving every component to its update-channel head, replace `--update-sources` with
`--source-lock .work/proposed-sources.json`. The input file is also left unchanged.
Review the complete bundle, source diff, and retained logs before adopting it.

`assembly_lock.py` records canonical root/workspace and original library manifests,
per-file and bundle SHA-256 checksums, the complete resolved module graph, pristine
and adapted vendor hashes, maintained code/test fingerprints, and the exact Go
patch version. Normal sync installs these manifests without `go mod tidy`, checks
the readonly graph, and rejects checksum or manifest drift before building from
vendor. Only `--update-dependencies 1.MINOR.PATCH` can resolve new dependencies,
including test dependencies. Source-only selection never implicitly updates them.
Original library manifests are preserved; patch alignment is explicit and cannot
hide a source requirement downgrade. Changed code/tests, builder/wheel locks,
image locks, or build-provenance locks require a new candidate. Root build files and the
complete inherited release-tools inventory now have
[checked provenance](./tools/README.md#root-build-and-release-tools-provenance),
including the verified original import and preserved local modifications. Sync
validates the maintained Makefile instead of rewriting it. The earlier candidates
are historical evidence, invalidated by subsequent input changes.
[Controlled binary builds](./tools/README.md#controlled-binary-builds) now use
explicit target architectures, trimpath, and captured project revision/epoch
metadata; all four checkpoints expose `--build-info` and have checked build
records. The image-bound source/dependency bundle is now activated locally; see
[activation evidence](./tools/README.md#active-source-and-dependency-baseline).
Native amd64, full offline assembly, image reproducibility, and production
acceptance remain open.

Validate a bundle through the normal locked path without modifying active inputs:

```bash
python3 tools/scripts/isolated_sync.py \
  --dependency-lock .work/dependencies.candidate.json \
  --candidate-output .work/dependencies.rechecked.json
```

[`tools/assembly/dependencies.lock.json`](./tools/assembly/dependencies.lock.json)
is the active canonical bundle. Its checksums detect drift relative to validated
inputs; they are not signatures or release certification. Cross-directory binary
comparisons are separate evidence, documented with their architecture and scope.

Dependency checks use Go's module parser on all four original controller manifests,
the snapshot client, and `csi-lib-utils`. Core API/client requirements must agree on
a minor. The resolved and vendored graphs must use one exact Kubernetes release
across staging modules and `k8s.io/kubernetes`, without replacement downgrades or
unversioned/forked Kubernetes replacements. Independently versioned repositories
such as `klog`, `utils`, and `kube-openapi` are not staging modules. Diagnostics
identify the source component and original revision; `SKIP_SANITY_CHECK=true` is
rejected. These checks are necessary constraints, not a Kubernetes support matrix,
complete dependency lock, security assessment, or release certification.

Existing generated trees (including interrupted runs) are rejected before sync
mutates inputs. Use the isolated helper for repeated runs; atomic replacement of
an existing assembled tree remains unfinished. Makefile shortcuts still wrap the
root scripts: `make sync` and `make clean`.

The sync retries `go mod tidy` (update mode only) and `go work vendor` up to three times for
recognized transient proxy/checksum-server transport errors, waiting 5 and 10
seconds between attempts. Checksum verification remains enabled; integrity
failures and other deterministic errors fail immediately. The entire sync is
not retried because its repository transformations are not safe to restart.

See [CODE_LAYOUT.md](./CODE_LAYOUT.md) for the dual-layer layout that separates
the hand-maintained `tools/` source of truth from the generated assembly area.

### Building the project using CI

GitHub Actions runs presubmit checks for pull requests and pushes to `main`.
The verify/build jobs use the locked isolated helper above. The legacy hostpath
E2E job is not yet migrated to this path and still needs ownership-safe test
orchestration. Use the local commands below to exercise build and tooling checks.
[act](https://github.com/nektos/act) is an optional workflow debugging tool, not
a guaranteed reproduction of the full CI environment: workflow event/branch
filters, runner images, and Docker/privileged e2e setup must also be accounted
for.

The presubmit workflow (`.github/workflows/presubmit.yaml`) has these jobs:

- `verify` — code-quality gates on the hand-maintained source of truth under
  `tools/`: `gofmt`, Apache-2.0 boilerplate license headers, and
  `shellcheck` (severity `warning`) on the scripts we own, plus regression
  tests for artifact verification and dependency retries. This lint job excludes
  the generated assembly area (`cmd/`, `pkg/`, `staging/`). Upstream CI does not
  validate our transformations or unified dependencies; restoring the full
  upstream unit suites against the assembled tree remains necessary.
- `build` — runs the full sync, builds all three binaries, runs `go test` and
  `go vet` over the hand-maintained packages, bounded lifecycle race tests
  (including local fake Kubernetes/CSI subprocesses), and validates the marked
  README arguments against the assembled AIO CLI.
- `e2e-hostpath` — runs the Hostpath CSI driver e2e suite via `.prow.sh`.

Supplementary workflows: `trivy.yaml` builds all three images and checks each
entrypoint, packaged executable, and component-specific help before performing
a report-only vulnerability scan; `codespell.yml` checks spelling.

After a successful sync, run CLI and metadata checks inside that snapshot's
verified builder. Image packaging additionally requires a Docker-capable
environment and remains outside the validated isolated workflow:

```bash
set -euo pipefail
python3 tools/scripts/verify_artifacts.py cli
python3 -B tools/scripts/build_binaries.py verify --arch arm64 # or amd64
# Requires the verified builder plus Docker; not yet validated end-to-end:
make container BUILD_ARCH=arm64
python3 tools/scripts/verify_artifacts.py images
```

The smoke checks require no Kubernetes cluster or CSI socket. The image checks
run with container networking disabled and verify that `--help` exits cleanly;
they do not replace controller integration tests. Run the tooling regression
tests without assembly or a container engine using:

```bash
python3 -B -m unittest discover -s tools/scripts -p '*_test.py'
```

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
