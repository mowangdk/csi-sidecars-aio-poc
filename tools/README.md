# tools/ — hand-maintained tool code

This directory is the **source of truth** maintained by developers. See the
repository-root [CODE_LAYOUT.md](../CODE_LAYOUT.md) for how this layer relates
to the generated assembly area.

The assembly area (`cmd/`, `pkg/`, `staging/`, `go.mod`/`go.work`, `vendor/`)
is produced by `scripts/sync.sh` from the locked upstream revisions and is
**not committed** — `.gitignore` excludes it as build output. A fresh checkout
runs sync once inside the locked builder, then builds with a plain `go build`;
sync is not a step of every build.

## Contents

| Path | Purpose |
|------|---------|
| `cmd/csi-sidecars/main.go` | AIO unified entry point; dispatches to each sidecar's `<sidecar>_main`. |
| `cmd/csi-sidecars/main_test.go` | Tests for `parseControllers` and the config→global-var mapping. |
| `cmd/csi-sidecars/config/flags.go` | AIO + snapshotter flag registration. |
| `cmd/csi-sidecars/config/flags_test.go` | Flag-registration regression tests. |
| `pkg/attacher/cmd/csi-attacher/main.go` | Forked attacher entrypoint testing the flag-init strategy. |
| `pkg/attacher/cmd/csi-attacher/config/flags.go` | Attacher flag registration (prefixed + unprefixed). |
| `pkg/attacher/cmd/csi-attacher/config/flags_test.go` | Attacher flag-registration tests. |
| `scripts/sync.sh` | Clones upstream `external-*`, rewrites imports, assembles the merged module, generates `go.mod`/`go.work`, and vendors. |
| `scripts/cleanup.sh` | Removes all generated artifacts (leaves `tools/` untouched). |
| `scripts/isolated_sync.py` | Runs `sync.sh` (or tooling checks) inside the locked Linux builder with the checkout mounted, regenerating the assembly area in place. |
| `scripts/retry-go-dependencies.sh` | Bounded retries for transient Go dependency transport failures; preserves checksum verification. |
| `scripts/retry_go_dependencies_test.py` | Regression tests for retry limits, exit status, and integrity failures. |
| `scripts/verify_artifacts.py` | Checks README CLI arguments and packaged image executables/entrypoints/help. |
| `scripts/verify_artifacts_test.py` | Regression tests for the artifact verifier; no assembly or container engine required. |
| `scripts/assembly_sources.py` | Source-lock validation and exact single-commit checkout of each upstream input. |
| `scripts/assembly_sources_test.py` | Malformed-lock, stale-output, exact-history import, and update-channel fixtures. |
| `scripts/assembly_dependencies.py` | Generates `go.mod`/`go.work` from the original sources, aligning the Kubernetes family on the selected release. |
| `scripts/assembly_dependencies_test.py` | go.mod/go.work generation, family alignment, and real Go parser fixtures. |
| `scripts/build_environment.py` | Locked builder-image selection, tool preflight, and fresh hash-verified Python bootstrap. |
| `scripts/build_environment_test.py` | Builder/version, wheel tampering, partial-install, and generator-integrity fixtures. |
| `scripts/image_inputs.py` | Image-lock/Dockerfile checks, standalone Dockerfile generation, test-image selection, and explicit registry verification. |
| `scripts/image_inputs_test.py` | Image/schema drift, filesystem safety, registry checksums/platforms, and harmless Prow-wrapper fixtures. |
| `assembly/sources.lock.json` | Exact original revisions and repository identities for all five source inputs. |
| `assembly/build-environment.lock.json` | Per-architecture builder digests, exact tool versions, and Python wheel URLs/hashes. |
| `assembly/images.lock.json` | Shared runtime index/platform digests and explicitly selected legacy Kubernetes test images. |
| `scripts/sidecars.conf` | Update-channel metadata, one `<sidecar>,<branch>` per line; not consumed by normal sync. |
| `csi-release-tools-hashes.txt` | Historical upstream hash list. |
| `sync.log` | Reference log of a successful sync (tracked, generated). |

## Usage

Build directly from a populated tree (no sync required):

```bash
make build            # plain go build of the three release commands
```

Populate or regenerate the assembly area inside the locked Linux builder:

```bash
podman pull "$(python3 -B tools/scripts/build_environment.py image)"
python3 -B tools/scripts/isolated_sync.py --update-dependencies 1.MINOR.PATCH
```

The helper defaults to Podman; use `--engine docker` with a Docker preload
instead. The container mounts the checkout, so the tree is regenerated in
place: preflight verifies any existing outputs are disposable and `sync.sh`
removes them before assembling. `--tooling-only`
runs the tooling tests, gofmt, and license checks without assembling.
`make sync KUBERNETES=1.MINOR.PATCH` and `make clean` wrap the root scripts.
Both `sync.sh` and `cleanup.sh` can be started from any directory: they resolve
the repository root from their own location and always operate on it.

`sync.sh` symlinks the hand-maintained entrypoints from `tools/` into the
assembly area and generates the rest from upstream. It generates
`go.mod`/`go.work` from the original source requirements (via
`assembly_dependencies.py seed`), aligning the Kubernetes staging modules on the
release passed via `--update-dependencies`, then runs `go mod tidy` and
`go work vendor`.

## Updating source revisions

All four independent upstream controllers remain present. To change their update
channels, edit `scripts/sidecars.conf` and update `assembly/sources.lock.json`
to the exact commits to sync. Normal sync consumes that lock directly; the branch
names in `sidecars.conf` are update metadata only.

The source lock targets Kubernetes 1.36.3, with staging modules aligned at
0.36.3. `assembly_dependencies.py seed` reads the original component
requirements (including the snapshot client) and aligns every Kubernetes staging
module on the selected release while generating `go.mod`/`go.work`. The resolved
tree is then pinned by the source lock and `go.sum`; there is no separate consistency
re-check, so a build is a plain `go build` against the generated vendor tree.

## Runtime and legacy test image inputs

`images.lock.json` pins one `gcr.io/distroless/static` index for the root AIO
image and both standalone snapshot images, plus the Linux amd64/arm64 child
manifests. Root and generated Dockerfiles use the same checked template and
preserve command-specific entrypoints and the `binary` build argument.

```bash
# Offline maintained-input check; does not create generated files:
python3 -B tools/scripts/image_inputs.py preflight
# After sync, verify all three Dockerfiles:
python3 -B tools/scripts/image_inputs.py verify --root <ASSEMBLY_SOURCE_DIR>
# Explicit online verification of index, child manifest, and config checksums/platforms:
python3 -B tools/scripts/image_inputs.py registry
```

Normal sync does not resolve tags or contact image registries. It checks the root
Dockerfile before tool bootstrap, then generates only fresh standalone
Dockerfiles. `CMDS_DIR` must stay `cmd` in the inherited container path.

The existing Kubernetes **1.31.9 legacy regression** selection is pinned to the
index published in the [kind v0.28.0 release](https://github.com/kubernetes-sigs/kind/releases/tag/v0.28.0).
The maintained `.prow.sh` selects that exact patch image and matching kind
version before sourcing inherited defaults. An unknown version or malformed lock
fails before Prow is sourced. Tests execute the wrapper only against a harmless
stub; no real cluster is created or changed.

## Build

The root `Makefile` inherits the plain `go build` target from
`release-tools/build.make`; it stamps `main.version` from the git revision and
uses the `vendor/` directory automatically when present. No separate controlled
build step is required.

## Image smoke checks

Build local images from the assembled tree using its root AIO Dockerfile and
generated standalone Dockerfiles, with a unique tag. Then verify them:

```bash
python3 -B tools/scripts/verify_artifacts.py images --engine podman --tag YOUR_UNIQUE_TAG
```
