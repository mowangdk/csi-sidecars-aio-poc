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

The active source/dependency pair uses the validated Kubernetes 1.36.3 baseline,
with staging modules aligned at 0.36.3. It replaces the historical incompatible
selection of 0.37 source requirements with 0.36.1 replacements. The gate still
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

Only `--update-dependencies 1.MINOR.PATCH` runs dependency resolution (`go mod
tidy`). It preserves source minimums, aligns Kubernetes patches within the
original core minor, and leaves original library manifests untouched. Normal
sync installs locked manifests, checks the readonly graph, vendors with Go
checksum verification, and rejects manifest or vendor drift before builds.
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
python3 -B tools/scripts/image_inputs.py verify --root .work/<assembly>/source
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

The pinned Linux arm64 tooling run `.work/assembly-iii7vol6/assembly.log` passed
all 94 tests without skips, gofmt, and license checks. The old builder-only
bundle was rejected before bootstrap/source generation in
`.work/assembly-fizulv7m/assembly.log`; no candidate was exported. A separate
network-disabled, read-only run of the bootstrapped tooling snapshot also passed
all 94 tests. This verifies offline tooling, not full offline source assembly.

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

### Active source and dependency baseline

The image-bound candidate below is now activated locally as
`tools/assembly/dependencies.lock.json`, byte-for-byte, together with its embedded
source selection in `sources.lock.json`. Provisioner selects
`5ba0af9d11649b47cf0ea6b2df8b1b0c5b6a33b4` and csi-lib-utils selects
`a83815b94f6ead7ac9946fd5cde3fd2025dd4e8e`; attacher, resizer, and snapshotter
retain their previous revisions. No update channels were resolved during
activation, and no candidate checksum or dependency content was rewritten.

A fresh normal assembly, with no source/dependency override or update flags,
passed in the pinned Go 1.26.5 Linux arm64 builder:

```bash
python3 -B tools/scripts/isolated_sync.py --workspace /build/active-lock
```

Retained evidence: `.work/assembly-6u7ukoqt/assembly.log` and its `source/` tree.
All 113 tooling tests passed without skips, alongside maintained Go tests, race
tests, vet, CLI/help checks, all four binary checkpoints, and controlled metadata
verification. Canonical preflight, manifest installation, module graph,
pristine vendor, and adapted vendor checks passed without `go mod tidy`.

Compared with `.work/assembly-qb_1v35d/source`, the generated `cmd/`, `pkg/`,
and `staging/` inventories (contents, executable modes, and link targets), four
binaries and build records, source/project provenance, root build inputs, and
complete bundle match. All three retained `image-lock-validation` images passed
network-disabled smoke checks against the active assembly's binaries. Read-only,
network-disabled gofmt, license, and build-provenance checks also passed.

The active bundle SHA-256 remains
`6e9841eaa6e4bc413b52fe3889d4fddc6e64ea6c0d7cf476ab5f7e579a5887e3`, with
maintained-input fingerprint
`bbef8c006d043494a9d90b6d4f33cd32922f5bcfa70b5b328375b07b664da9fb`.
These are still dirty working-tree builds. Activation and documentation changes
are unstaged; the preexisting staged index and `release-tools/` were preserved.
No commit, publication, cluster operation, or kubeconfig access was performed.
Native amd64 and full offline assembly remain acceptance gaps, as do the
production Kubernetes matrix, security/functional release gates, and OCI
image-byte reproducibility. The following sections retain earlier checkpoint
status; their unpromoted-lock statements describe those checkpoints only.

### Image-bound candidate validation

After locking the shared runtime and legacy test image inputs, a fresh Kubernetes
1.36.3 dependency update at `/build/images` and a locked replay at
`/build/images-replay/deeper` passed in the pinned Go 1.26.5 Linux arm64 builder.
Both passed all 113 tooling tests without skips, maintained Go tests and race
checks, vet, CLI/help checks, the standalone attacher checkpoint, and controlled
binary metadata/checksum verification. The replay ran no `go mod tidy`.

The complete candidate bundles, generated `cmd/`, `pkg/`, and `staging/`
inventories (contents, executable modes, and link targets), all four binaries
and build records, source/project provenance, and root build inputs matched.
Canonical manifest and pristine/adapted vendor checks passed. The maintained
fingerprint still matches the candidate after recording this documentation.
Compared with the root-only candidate below, source selections, canonical
manifests, module graph, Go version, and vendor hashes are unchanged; only the
maintained-input binding and enclosing payload checksum changed.

Retained local evidence:
- Update: `.work/assembly-_5we07mf/assembly.log` and `.work/dependencies-36-images.json`.
- Successful locked replay: `.work/assembly-qb_1v35d/assembly.log` and
  `.work/dependencies-36-images-rechecked.json`.
- Complete bundle SHA-256:
  `6e9841eaa6e4bc413b52fe3889d4fddc6e64ea6c0d7cf476ab5f7e579a5887e3`.
- Maintained-input SHA-256:
  `bbef8c006d043494a9d90b6d4f33cd32922f5bcfa70b5b328375b07b664da9fb`.
- An earlier replay, `.work/assembly-ciy1403w/assembly.log`, failed during the
  upstream-history merge with `fatal: stash failed`, before dependency
  validation. Read-only checks found no persistent tracked changes or disk-space
  shortage, but did not establish the cause. That tree/log was retained; the
  successful retry used a new directory without modifying the failed tree.

All three Linux arm64 runtime images built locally from the pinned base with
`--pull=never --network=none` and tag `image-lock-validation`. Image smoke checks
passed against both successful assemblies' binaries: exact packaged executable
bytes, command-specific entrypoints/help, and zero process exit status, with
networking disabled. Inspection confirmed UID 0 and the locked base's layer chain
plus one executable layer. To recheck the retained local images against the replay:

```bash
python3 -B .work/assembly-qb_1v35d/source/tools/scripts/verify_artifacts.py \
  images --engine podman --tag image-lock-validation
```

Explicit registry verification checked both pinned indexes and their amd64/arm64
manifest/config checksums and platforms. A separate read-only, network-disabled
Linux tooling run passed all 113 tests without skips; it reused a bootstrapped
snapshot and is not full offline assembly. Gofmt, license headers, Bash syntax,
warning-level ShellCheck, and original-object build-provenance checks also passed.
The host tooling rerun passed 113 tests with six platform-dependent skips.

At this checkpoint, these were dirty working-tree candidate builds, not release
acceptance. No source/dependency candidate had yet been promoted, no image was
published, and no cluster, kubeconfig, `release-tools/` file, or preexisting staged
change was modified.
Native amd64 acceptance, full offline assembly, nonroot runtime validation, a
production Kubernetes matrix, and OCI image-byte reproducibility remain open.
The root-only and older candidates below are historical evidence; image-input
changes invalidate their fingerprints, so they must not be promoted.

### Historical root-only integration validation

After moving controlled build routing into the root Makefile, a fresh update at
`/build/root-only` and locked replay at `/build/root-replay/deeper` passed in the
pinned Go 1.26.5 Linux arm64 builder. Both passed all 103 tooling tests without
skips, maintained Go tests and race tests, vet, CLI/help checks, and controlled
artifact verification. The replay ran no `go mod tidy`. The complete bundles,
generated `cmd/`, `pkg/`, and `staging/` trees, source provenance, and all four
binaries and build records matched byte-for-byte. Root build files and
`release-tools/` remained unchanged. Read-only, network-disabled gofmt, license,
and provenance checks also passed on the update snapshot.

Retained local evidence:
- Update: `.work/assembly-82vt34q2/assembly.log` and `.work/dependencies-36-root-only.json`.
- Replay: `.work/assembly-tv96nuz6/assembly.log` and `.work/dependencies-36-root-only-rechecked.json`.
- Bundle SHA-256: `857f7fe6ee472fd9be8a1457969dce1d553fe4a8c28000888aa6af556ca738d8`.
- Maintained-input SHA-256: `8a8f98fff36928b0f0ce03adb62de498cd5115ed9447032c3507be0f42eb102f`.

The source selection, canonical manifests, module graph, and vendor hashes are
unchanged from the prior metadata candidate; only the build-input binding changed.
These were dirty working-tree builds at project HEAD
`0e07ce47b62e82cb25a59dab755a3274d4a8136e`, not release acceptance. At this
checkpoint, active locks remained unpromoted; runtime/test image locking, native
amd64 acceptance, and full offline assembly were still open.

### Historical provenance-bound candidate validation

A fresh Go 1.26.5 / Kubernetes 1.36.3 update and separate ordinary locked replay
passed in the pinned Linux arm64 builder. Both ran all 94 tooling tests without
skips, maintained Go tests and race tests, vet, all three release build/help
checks, standalone attacher build/help, and README CLI checks. The replay ran no
`go mod tidy`. The complete bundles, source provenance, generated `cmd/`, `pkg/`,
and `staging/`, root build files, inherited release-tools, and all four checkpoint
binaries matched byte-for-byte. The maintained Makefile and release-tools tree
also matched the original working inputs after assembly.

Retained local evidence:
- Update: `.work/assembly-b6u3qvyo/assembly.log` and
  `.work/dependencies-36-provenance.json`.
- Locked replay: `.work/assembly-_rdjf9o6/assembly.log` and
  `.work/dependencies-36-provenance-rechecked.json`.
- Complete bundle SHA-256:
  `2678355807548a56d294876336927057b5adf79a10889cce77d5f68e6ad1f5b2`.
- Then-current maintained-input SHA-256:
  `7668a386011efc0c092c5c1d7b9fd06a0b8d9f649bf17dff92a312abd4c828ec`.
- Build-provenance lock SHA-256:
  `ebf09bd954eb1e7f894ff64d82319c3838c39548733363df8672d53939d3d3ba`.

| Linux arm64 checkpoint | SHA-256 (both runs) |
| --- | --- |
| `csi-sidecars` | `e1bef03337e216b777d6f51fd9c382fa55effc3ad6ba29bffdc1e9f7a651184a` |
| `snapshot-controller` | `2b1c453186706e23c629a2d267cb53aa3290cdf7e31997cf2314b5d7834f5ed3` |
| `snapshot-conversion-webhook` | `4ac15ae338a3d819580bf6fbe984fb20b9d216f4857c7c43ee81f43dbfbe89ca` |
| standalone `csi-attacher` | `cfc08c218655c0ecec381e324a8a8fa51ca6c5394a43464dfe546507fac9fc7c` |

These use the same source selection and dependency payload as the earlier
builder-only candidate; only the input and enclosing payload hashes changed.
This bundle matched its then-current fingerprint; controlled-build tooling has
since invalidated it. It must not be promoted. Both builds
used working-tree snapshots at project HEAD
`0e07ce47b62e82cb25a59dab755a3274d4a8136e`, not clean committed release inputs.
Their binaries match the earlier Go 1.26.5 bytes (including `vcs.modified=true`).
Both directories were mounted at `/workspace`; this is not path-independent or
native amd64 acceptance. This run predates controlled binary metadata.
Runtime/test image locks, full offline assembly, and production acceptance remain open. Active source and
dependency selections remain unpromoted.

### Historical Go 1.26.5 builder validation

The selected Kubernetes 1.36.3 sources also passed a fresh explicit update and
separate fresh locked replay in the public digest-pinned Linux arm64 builder.
Both used snapshots of the maintained working tree at project HEAD
`0e07ce47b62e82cb25a59dab755a3274d4a8136e`; these were not clean committed release
inputs. The provisioner and csi-lib-utils selections are the same as the
historical candidate below; the other three controller revisions are unchanged.

Both runs passed all 83 tooling tests without skips, maintained Go tests and
race tests, vet, the three release build/help checks, standalone attacher
build/help, and README CLI checks. The locked replay executed no `go mod tidy`.
The complete bundles, original-source provenance, generated `cmd/`, `pkg/`, and
`staging/` trees, and all four checkpoint binaries matched byte-for-byte.
Canonical manifest and pristine/adapted vendor checks passed. This bundle matched
its then-current inputs; the subsequent build-provenance changes invalidate it.
It is retained as historical evidence and must not be promoted.

Retained local evidence:
- Update: `.work/assembly-sk9sliw8/assembly.log` and
  `.work/dependencies-36-tools-locked.json`.
- Locked replay: `.work/assembly-0lwtd0jf/assembly.log` and
  `.work/dependencies-36-tools-rechecked.json`.
- Complete bundle SHA-256:
  `1020eee4acf46bbed847b3a6936e20bf7429736baaf164f263cc55acf7eae80f`.
- Maintained-input SHA-256:
  `895f8a18aa1e5f3054ea2ba0b3cd38817c8cdab9e94df8b9f98f53809afda308`.

| Linux arm64 release binary | SHA-256 (both runs) |
| --- | --- |
| `csi-sidecars` | `e1bef03337e216b777d6f51fd9c382fa55effc3ad6ba29bffdc1e9f7a651184a` |
| `snapshot-controller` | `2b1c453186706e23c629a2d267cb53aa3290cdf7e31997cf2314b5d7834f5ed3` |
| `snapshot-conversion-webhook` | `4ac15ae338a3d819580bf6fbe984fb20b9d216f4857c7c43ee81f43dbfbe89ca` |

Binary build metadata confirms Go 1.26.5, Linux arm64, and CGO disabled. It also
records `vcs.modified=true`; this run predates the root-build/release-tools lock,
and predates the controlled binary metadata path. A separate read-only,
network-disabled run of the update assembly passed builder verification, vendor
compatibility, maintained race tests, vet, and CLI checks. It reused assembled
inputs and the bootstrapped environment, not prefetched full-assembly inputs.
Both fresh directories were mounted at `/workspace`; matching bytes do not prove
path independence. Native amd64, full offline assembly, and production acceptance
remain open. Active source/dependency locks have not been promoted.

### Historical Go 1.26.3 candidate validation

A Kubernetes 1.36.3 candidate has passed a fresh explicit update and a separate
fresh locked replay on Linux arm64 with Go 1.26.3. It retains the active attacher,
resizer, and snapshotter revisions, selecting provisioner
`5ba0af9d11649b47cf0ea6b2df8b1b0c5b6a33b4` and csi-lib-utils
`a83815b94f6ead7ac9946fd5cde3fd2025dd4e8e`. This is local candidate evidence, not
an adopted source selection or a production Kubernetes support claim.

Both runs passed all 72 tooling tests, maintained Go tests, race tests, vet,
three release build/help checks, the standalone attacher checkpoint, and README
CLI checks. The locked run executed no `go mod tidy`; its complete bundle,
generated `cmd/`, `pkg/`, and `staging/` trees, and all three release binaries
matched the update run. The bundle SHA-256 is
`bd92f51a86af1d4f3111a8104aa6c71f2d0bc56999816ba34bef352d832cc5d6` and its
maintained-input fingerprint is
`98035f3b9e50ef9581d60dc8e58c17b2cc1cf798e956aca3685060dc1ceba109`.
An older tooling fingerprint and Go 1.26.5 were independently rejected by the
real bundle checks. A separate network-disabled, read-only mounted assembly
passed maintained race tests, vet, vendor compatibility, and CLI checks.

The locked replay used local builder digest
`sha256:a7d6744891f7dde6b89799b36ec7cbeaae549c99b1cc16aafd63ee7d697ec107`
(Go 1.26.3, Python 3.13.5, GCC 14.2.0, Git 2.47.3). This local image is not a
published or approved CI builder. Both assembly directories were mounted as
`/workspace`, so matching binaries do not establish path independence or
cross-architecture reproducibility. The offline check reused assembled inputs;
it was not an offline source fetch or full assembly. This is historical evidence
for the pre-tool-lock inputs. The new builder path selects Go 1.26.5, so this
candidate is stale and must not be promoted. Active source/dependency locks
remain unchanged.

### Locked builder path

`build-environment.lock.json` selects public `docker.io/library/golang` platform
digests for Linux amd64 and arm64 and requires Go 1.26.5, Python 3.13.5, GCC 14.2.0,
and Git 2.47.3. The local helper and CI verify/build jobs consume this same lock.
Fresh assembly rejects arbitrary image overrides. Historical `--runtime-baseline`
replay still requires an explicit image and cannot export an assembly candidate.

After source/dependency preflight, bootstrap creates a new `.assembly-env` without
system pip, verifies the fixed pip 26.2 and git-filter-repo 2.47.0 wheel hashes,
and installs only those wheels with index/dependency resolution disabled.
Existing or interrupted environments are rejected; caller virtual environments
are never reused. Generator implementation bytes are checked against its verified
wheel. Race tests explicitly enable CGO and `/usr/bin/gcc`; release binaries and
the standalone attacher checkpoint disable CGO. A builder identity environment
value is a consistency check, not an attestation: the isolated launcher enforces
the digest used by the container engine.

Before the provenance changes, the pinned arm64 builder passed a fresh
`--tooling-only` run: verified wheel bootstrap, all 83 then-existing tooling tests
without skips, gofmt, and license headers. The pinned amd64 image also passed
actual tool-version checks and its own fresh
wheel bootstrap, followed by all 83 tooling tests without skips on a read-only
snapshot with networking disabled. That amd64 run used emulation on arm64;
it is not native amd64 or Docker CI acceptance. The offline suite reused the
bootstrapped environment and does not establish full offline assembly.

The legacy hostpath E2E job is not migrated by this builder change and remains
outside this validation path; its ownership-safe replacement is still pending.
At that builder checkpoint, runtime/test image locks were also pending; see the
image-bound validation and active baseline above for subsequent evidence. Full
offline assembly, native amd64 acceptance, and broader release gates remain open.
A pinned tool version is not a security certification.

Sync rejects existing generated inputs, including partial runs. Use the isolated
helper for repeated assembly without altering an earlier tree. Hand-maintained
symlinks in a fresh generated tree are relative, and selected source identities
are recorded in `tmp/source-provenance.json`.
