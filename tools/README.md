# tools/ — hand-maintained tool code

This directory is the **source of truth** maintained by developers. Reviewed
source-lock updates are produced by the explicit candidate workflow. See the repository-root
[CODE_LAYOUT.md](../CODE_LAYOUT.md) for how this layer relates to the generated
assembly area.

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
| `scripts/sync.sh` | Clones upstream `external-*`, rewrites imports, assembles the merged module and binaries. |
| `scripts/cleanup.sh` | Removes all generated artifacts (leaves `tools/` untouched). |
| `scripts/retry-go-dependencies.sh` | Bounded retries for transient Go dependency transport failures; preserves checksum verification. |
| `scripts/retry_go_dependencies_test.py` | Regression tests for retry limits, exit status, and integrity failures. |
| `scripts/verify_artifacts.py` | Checks README CLI arguments and packaged image executables/entrypoints/help. |
| `scripts/verify_artifacts_test.py` | Regression tests for the artifact verifier; no assembly or container engine required. |
| `assembly/sources.lock.json` | Exact original revisions and separate repository identities for all five source inputs. |
| `assembly/dependencies.lock.json` | Active canonical manifests, module graph, source identities, and input/vendor checksums. |
| `assembly/build-environment.lock.json` | Public per-architecture builder digests, exact tool versions, and Python wheel URLs/hashes. |
| `assembly/images.lock.json` | Shared runtime index/platform digests and explicitly selected legacy Kubernetes test images. |
| `scripts/image_inputs.py` | Offline image-lock/Dockerfile checks, standalone Dockerfile generation, test-image selection, and explicit registry verification. |
| `scripts/image_inputs_test.py` | Image/schema drift, filesystem safety, registry checksums/platforms, and harmless Prow-wrapper fixtures. |
| `scripts/build_environment.py` | Locked image selection, tool preflight, and fresh hash-verified Python bootstrap. |
| `scripts/build_environment_test.py` | Builder/version, wheel tampering, partial-install, and generator-integrity fixtures. |
| `assembly/build-provenance.lock.json` | Original release-tools identity/inventory, explicit local changes, and root build file hashes. |
| `scripts/build_provenance.py` | Offline build-input verification and explicit candidate generation from fetched Git objects. |
| `scripts/build_provenance_test.py` | Provenance tampering, filesystem safety, Git-object/import, and pre-bootstrap rejection fixtures. |
| `scripts/build_binaries.py` | Controlled static builds, project identity capture, embedded metadata, and checked per-binary manifests. |
| `scripts/build_binaries_test.py` | Metadata drift, explicit targets, failed rebuilds, and real cross-path/architecture build fixtures. |
| `scripts/assembly_sources.py` | Strict source-lock validation, exact checkout, provenance, and bootstrap verification. |
| `scripts/assembly_sources_test.py` | Malformed-lock, stale-output, exact-history import, and update-channel fixtures. |
| `scripts/assembly_dependencies.py` | Mandatory original-source, effective Kubernetes graph, and vendor compatibility checks. |
| `scripts/assembly_dependencies_test.py` | Family drift, hidden downgrade, real Go parser/vendor, and bypass-rejection fixtures. |
| `scripts/assembly_lock.py` | Checksummed source/dependency bundles, explicit dependency resolution, and locked manifest/graph/vendor verification. |
| `scripts/assembly_lock_test.py` | Tamper, stale-input, parser, update isolation, and no-clobber bundle fixtures. |
| `scripts/isolated_sync.py` | Fresh Linux assembly and validated source/dependency-candidate export without changing active locks. |
| `scripts/sidecars.conf` | Update-channel metadata, one `<sidecar>,<branch>` per line; not consumed by normal sync. |
| `csi-release-tools-hashes.txt` | Historical upstream hash list; not the current subtree provenance lock. |
| `sync.log` | Reference log of a successful sync (tracked, generated). |

## Usage

```bash
podman pull "$(python3 -B tools/scripts/build_environment.py image)"
python3 -B tools/scripts/isolated_sync.py
```

The helper defaults to Podman; use `--engine docker` with a Docker preload instead.
It retains a fresh snapshot and log under `.work/`. `--tooling-only` runs tooling
tests, gofmt, and license checks without requiring an adopted dependency bundle.
Makefile shortcuts `make sync` and `make clean` still wrap the root scripts;
`make sync` is an in-builder operation, not a host environment bootstrap.

`sync.sh` symlinks the hand-maintained entrypoints from `tools/` into the
assembly area (repository root) and generates the rest from upstream. It requires
the locked Linux builder. Both `sync.sh` and `cleanup.sh` can be started from any
directory: they resolve the repository root from their own location and always
operate on it.

## Updating source revisions

All four independent upstream controllers remain present. To change their update
channels, edit `scripts/sidecars.conf` (the example is illustrative, not a tested
selection):

```
attacher,master
provisioner,master
resizer,master
snapshotter,master
```

Then resolve and validate a complete candidate without changing active inputs.
Select the explicit Kubernetes release to match the proposed sources; `1.36.3`
is an investigation target, not a supported production matrix:

```bash
python3 tools/scripts/isolated_sync.py \
  --update-sources --update-dependencies 1.36.3 \
  --candidate-output .work/dependencies.candidate.json
```

The candidate is exported only after source/dependency compatibility, clean
assembly, maintained race tests, vet, and CLI checks pass. Use
`--source-lock .work/proposed-sources.json` instead of `--update-sources` to test
an explicitly selected combination without resolving moving branches. Neither
mode modifies the active lock or its input file. Review the diff and logs before
adopting a candidate; existing output files are never replaced.

The source lock targets Kubernetes 1.36.3, with staging modules aligned at
0.36.3. This build-only branch keeps the maintained Go code from `0e07ce4`;
it does not include the lifecycle adaptations from `444a2b4`. The gate still
checks effective replacement versions, all Kubernetes staging modules, and
original component requirements, including the snapshot client. It rejects
downgrade/fork/local replacements and cannot be skipped with `SKIP_SANITY_CHECK`.

Normal sync consumes both active locks under `tools/assembly/`. The dependency
bundle contains all five source identities, canonical root/workspace and original
library manifests with SHA-256 checksums, the resolved module graph,
pristine/adapted vendor hashes, maintained code/test fingerprints, and the exact
Go patch version. Local activation is not a release or production-support
approval; the bundle is integrity metadata, not a signature or complete
build-input lock.

For dependency-only updates, omit `--update-sources` to retain all five exact
revisions. The helper saves any old bundle outside the disposable update
snapshot as `previous-dependencies.lock.json`; the active checkout is unchanged.

Only `--update-dependencies 1.MINOR.PATCH` runs dependency resolution (`go mod
tidy`). It preserves source minimums, aligns Kubernetes patches within the
original core minor, and leaves original library manifests untouched. Normal
sync installs locked manifests, checks the readonly graph, vendors with Go
checksum verification, and rejects manifest or vendor drift before builds.
No lifecycle transformations modify vendored code on this branch, so the
pristine and final (`adapted_vendor_sha256`) hashes must agree.
All build/test commands use vendor mode. Code/test, builder/wheel-lock, image-lock, or
build-provenance-lock changes invalidate the bundle. Root build and inherited
release-tools drift must first be recorded and reviewed in the provenance lock.
Missing, malformed, stale, or mismatched locks fail before source downloads.

To exercise a candidate through the locked path in another fresh directory:

```bash
python3 tools/scripts/isolated_sync.py \
  --dependency-lock .work/dependencies.candidate.json \
  --candidate-output .work/dependencies.rechecked.json
```

The complete bundle and its embedded source selection must be reviewed together;
there is no automatic promotion. Omitting `--update-dependencies` never refreshes
dependencies, even with `--update-sources` or `--source-lock`; their selection must
match the existing bundle. Prefetched offline assembly, native architecture acceptance,
and broader compatibility/release gates remain pending.

### Runtime and legacy test image inputs

`images.lock.json` pins one `gcr.io/distroless/static` index for the root AIO image
and both standalone snapshot images, plus the Linux amd64/arm64 child manifests.
The selected index was resolved from the existing `static:latest` channel; this
freezes that runtime family without migrating to nonroot or claiming security
acceptance. Root and generated Dockerfiles use the same checked template and
preserve command-specific entrypoints and the `binary` build argument.

```bash
# Offline maintained-input check; does not create generated files:
python3 -B tools/scripts/image_inputs.py preflight
# After sync, verify all three Dockerfiles:
python3 -B tools/scripts/image_inputs.py verify --root .work/ASSEMBLY_DIRECTORY/source
# Explicit online verification of index, child manifest, and config checksums/platforms:
python3 -B tools/scripts/image_inputs.py registry
```

Normal sync does not resolve tags or contact image registries. It checks the root
Dockerfile before tool bootstrap, then generates only fresh standalone Dockerfiles.
Controlled builds and image smoke checks reject missing/drifted Dockerfiles and
per-command AIO overrides. `CMDS_DIR` must stay `cmd` in the inherited container
path. The image lock participates in dependency fingerprints, project identity,
and binary build records. Changing it requires a fresh dependency candidate;
older bundles must not be repaired by replacing hashes.

The existing Kubernetes **1.31.9 legacy regression** selection is pinned to the
index published in the [kind v0.28.0 release](https://github.com/kubernetes-sigs/kind/releases/tag/v0.28.0).
The maintained `.prow.sh` selects that exact patch image and matching kind version
before sourcing inherited defaults. Previously, minor-only selection could choose
1.31.2 despite requesting 1.31.9. An unknown version or malformed lock fails before
Prow is sourced. `release-tools/` remains unchanged. Tests execute the wrapper only
against a harmless stub; no real cluster is created or changed.

This does not establish a production Kubernetes matrix, lock every hostpath/workload
image, validate the inherited deployment/cleanup harness, or pin all of its tool
downloads. Those acceptance boundaries remain open. Registry checksum verification
is not signature verification, and binary reproducibility does not establish OCI
layer/configuration timestamp reproducibility. No image publication is authorized.

### Root-build and release-tools provenance

`build-provenance.lock.json` records upstream release-tools commit
`379a1bb9b001c0d62a091a21f1a4efaf42987248` and tree
`74aa8aff92eea2c4889162eeee65341d34e7688d`. Git objects fetched from the official
repository exactly match the project's original import at
`dd1a3e583140399704654e02514122718da2a19f:release-tools`. The historical hash list
is not used to infer this identity. The current subtree retains two modified files:
- `build.make`: legacy `hack` package exclusions and disabled aggregate ShellCheck.
- `prow.sh`: platform-aware downloads, kind versions/images, URL-based snapshot
  installation, and build-platform forwarding.

The lock contains every upstream file's SHA-256 and Git mode, the original
`OWNERS_ALIASES` link target, and explicit local change entries with reasons.
It also binds `Makefile`, `Dockerfile`, `.prow.sh`, and `.cloudbuild.sh`. Recording
these legacy inputs does not approve their deployment/publication behavior or
fix their remaining security and reproducibility gaps. No inherited file was
replaced. Sync now validates the maintained Makefile instead of rewriting it.

```bash
python3 -B tools/scripts/build_provenance.py verify
# Optional original-object/import verification using an already fetched repository:
python3 -B tools/scripts/build_provenance.py verify \
  --upstream-repository .work/release-tools-upstream.git
```

Verification runs before the isolated snapshot, again on the copy, before Python
bootstrap in update/locked/tooling modes, and during final bundle validation.
Content/mode drift, missing or additional files (including ignored subtree files),
unsafe links/special files, and competing Makefiles fail closed. The snapshot
retains the whole inherited inventory; no new subtree file is silently omitted.
The provenance lock is included in the dependency fingerprint only after the
actual root files and subtree match it. This is integrity metadata, not a
signature, full release provenance, or authorization to execute Prow/Cloud Build.

For an intentional provenance update, use `build_provenance.py candidate` with
`--upstream-repository`, full `--commit`, the original `--import-commit`, and one
`--local-change PATH=REASON` for each actual local difference. The command reads
Git objects and emits JSON to stdout; it does not fetch, check out, execute
upstream code, or replace any active file. Review that output and the actual
content diff before adopting a changed provenance lock, then validate a new
complete dependency candidate. Never repair an old bundle by editing its hashes.

### Controlled binary builds

`build_binaries.py` captures the full project HEAD, its committer timestamp as
`SOURCE_DATE_EPOCH`, and the maintained/source-input hashes before generation.
Build-input bytes/modes are compared with Git objects, independent of the local
index or tags. Modified build inputs receive a `-dirty.<input-id>` version suffix;
they are never labeled as clean releases. Generated upstream merge commits use
the captured epoch; their timestamps and revision IDs do not select binary versions.

The root Makefile overrides the three build recipes after including the unchanged
release-tools file; GNU Make reports these intentional recipe overrides. Inherited
prerequisites remain, including container/push dependencies and the legacy Go-version
advisory. The controlled builder independently enforces the exact locked environment.
No project hook is installed in `release-tools/`. These root recipes and the
standalone attacher checkpoint use the same Python builder.
Targets are explicit Linux `amd64` (`GOAMD64=v1`)
or `arm64` (`GOARM64=v8.0`), with CGO disabled, `-trimpath`, `-buildvcs=false`,
and `main.version` set from the captured project identity. Caller Go flags,
experiments, debug settings, and implicit toolchain selection are not inherited.
Compilation uses checked vendor inputs with dependency networking disabled.
The original standalone webhook has no `--version`; all four commands now accept
exactly `--build-info` as a single-argument, pre-main JSON identity query.
The generated metadata source remains reproducible from `tools/`.

Inside the verified builder and assembled snapshot, an explicit rebuild is:

```bash
source .assembly-env/bin/activate
make build BUILD_ARCH=arm64  # or amd64; use the intended target explicitly
python3 -B tools/scripts/build_binaries.py build --command csi-attacher --arch arm64
python3 -B tools/scripts/build_binaries.py verify --arch arm64
./bin/csi-sidecars --build-info
```

`bin/<command>.build.json` binds each binary's checksum and observed Go/ELF
metadata to the project, all five original source identities, dependency bundle,
builder digest, build-provenance lock, and build flags. Native binaries also
have their embedded identity executed and checked. Cross-compilation is not
native execution acceptance. A failed rebuild removes its old success record;
modified generated identity files and changed captured inputs are rejected.
These records are unsigned local build evidence, not release attestations.

`isolated_sync.py --workspace /build/first` selects a safe alternative Linux
mount path; use another path such as `/build/second/deeper` for a fresh locked
replay. Each run bootstraps its own environment. Reusing a virtual environment
at a different path is not supported. Runtime/OCI image timestamps are not
controlled by these binary checks.

## Image smoke checks

Build local images from the retained snapshot using its root AIO Dockerfile and
generated standalone Dockerfiles, with a unique tag. Then verify them:

```bash
python3 -B .work/ASSEMBLY_DIRECTORY/source/tools/scripts/verify_artifacts.py images \
  --engine podman --tag YOUR_UNIQUE_TAG
```

## Standalone branch validation

Linux arm64 candidate run: `.work/assembly-0spxpuji/assembly.log`. It passed
113 tooling tests, maintained-package tests/race tests, vet, CLI validation,
three release binary builds, and the standalone attacher checkpoint.
Go formatting, license headers, shell syntax, and ShellCheck also passed.

Active bundle SHA-256: `f069b1aa95d06e98786a3c15eb655eaeed4eff29b9dffa573dcc5bf087de2a63`.
Pristine and final vendor SHA-256 agree: `2185655df0a948759feafeb2a6ad3fa60f4e43e4d1830efce8ff6002aa935de8`.
The exact candidate bytes were activated without changing source revisions.

All three local images use tag `standalone-precommit-f069b1aa`; entrypoints,
packaged binary bytes, and network-disabled help checks passed. Existing
`image-lock-validation` tags were preserved. No images were published.

Normal locked run `.work/assembly-2qwr3000/assembly.log` passed without update
flags or `go mod tidy`. Generated source inventories, manifests, all four
binaries/build records, and provenance matched the candidate at another path.

These are pre-commit working-tree results. Final-revision cross-path replay
awaits the two human-authored commits; it is not claimed complete.

An earlier candidate run `.work/assembly-0r10mq0t/assembly.log` failed because
both the imported lock and candidate were present. The update helper now
retains the previous lock outside its snapshot; regression tests cover both
success and failure without modifying the active checkout.

Native amd64, full offline assembly, cluster E2E, and production acceptance
remain deferred.
