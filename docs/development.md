# Development with Podman

The sync script requires Linux. On macOS, use a running Podman machine; on
Linux, use a working local Podman engine. This guide builds the actual project
in a disposable Linux workspace and then packages its binaries with Podman.
It does not run Kubernetes e2e or establish production readiness.

## Prerequisites and scope

- Host tools: Bash, Git, Podman, and Python 3 for image verification.
- Start the Podman machine first when using macOS (`podman machine start`).
- Run all command blocks in the same Bash session, initially at the repository
  root. The variables below are reused by subsequent blocks.
- The source copy is a clone of your **committed HEAD**, including commits that
  have not been pushed. Uncommitted changes and untracked files are not copied.
  Commit changes you want to test before starting this workflow.
- Network access is needed for the builder image, Debian packages, Python
  tooling, upstream Git repositories, and Go dependencies. Allow sufficient
  disk space for source trees, Go caches, binaries, and images.

The builder matches the Go version currently exercised by CI. This workflow
still consumes mutable upstream refs, package versions, and image tags, just
like the current PoC; it is **not a reproducible release build**. It builds for
the Linux architecture of the Podman engine, not an automatic multiarch matrix.

## 1. Prepare an isolated workspace

Use a native Podman volume for the Linux source tree, rather than running the
sync against your host checkout. Cleanup and absolute generated symlinks then
stay inside the container, and file-heavy Git/vendor operations avoid a macOS
bind mount. Go module and compiler caches are retained separately for reuse.

```bash
set -euo pipefail
build_root=$(mktemp -d "${TMPDIR:-/tmp}/csi-sidecars-podman.XXXXXX")
build_name="csi-sidecars-build-$(date +%s)-$$"
source_volume="${build_name}-source"
image_tag="$build_name"

git clone --no-hardlinks . "$build_root/repo"
podman volume create "$source_volume"
podman volume create csi-sidecars-go-mod-cache
podman volume create csi-sidecars-go-build-cache
```

## 2. Build and test the real binaries

The builder installs Python venv support, activates a Linux virtual environment,
then runs the normal sync. The sync installs `git-filter-repo` if needed and
builds the binaries. Checksum verification is left enabled.

```bash
podman create --name "$build_name" \
  --volume "$source_volume:/workspace" \
  --volume csi-sidecars-go-mod-cache:/go/pkg/mod \
  --volume csi-sidecars-go-build-cache:/root/.cache/go-build \
  --workdir /workspace/repo \
  --env GOTOOLCHAIN=local \
  docker.io/library/golang:1.26.5 \
  bash -c '
    set -euo pipefail
    apt-get update
    apt-get install -y --no-install-recommends python3-venv
    export GOPATH=/go
    python3 -m venv .venv
    source .venv/bin/activate
    go version
    go env GOOS GOARCH GOPROXY GOSUMDB
    python3 -B -m unittest discover -s tools/scripts -p "*_test.py"
    ./tools/scripts/sync.sh
    go test ./cmd/csi-sidecars/... ./pkg/attacher/cmd/csi-attacher/config/...
    go vet ./cmd/csi-sidecars/... ./pkg/attacher/cmd/csi-attacher/config/...
    python3 tools/scripts/verify_artifacts.py cli
    go mod verify
  '

podman cp "$build_root/repo" "$build_name:/workspace/"
podman start --attach "$build_name" 2>&1 | tee "$build_root/build.log"
# Do not rely only on the exit status of the attach command.
build_status=$(podman inspect --format '{{.State.ExitCode}}' "$build_name")
test "$build_status" -eq 0
```

If the build fails, retain the container and log for diagnosis. Do not continue
to image packaging with partial outputs. Retry transient dependency operations
through the normal script; a failed full sync may require a fresh source
workspace instead of restarting its repository transformations in place.

## 3. Package the outputs in Podman

Copy only the built binaries, Dockerfiles, and verifier back to the host. Do not
copy the generated source tree for host execution: its symlinks refer to Linux
workspace paths, and the binaries are Linux executables.

The following commands use the same Dockerfile selection as `make container`,
but explicitly invoke Podman. `make container` itself still invokes Docker;
`--engine podman` is an option of the verifier only.

```bash
image_context="$build_root/images"
mkdir -p "$image_context/tools/scripts" \
  "$image_context/cmd/snapshot-controller" \
  "$image_context/cmd/snapshot-conversion-webhook"

podman cp "$build_name:/workspace/repo/bin" "$image_context/bin"
podman cp "$build_name:/workspace/repo/Dockerfile" "$image_context/Dockerfile"
podman cp "$build_name:/workspace/repo/tools/scripts/verify_artifacts.py" \
  "$image_context/tools/scripts/verify_artifacts.py"

for component in snapshot-controller snapshot-conversion-webhook; do
  podman cp "$build_name:/workspace/repo/cmd/$component/Dockerfile" \
    "$image_context/cmd/$component/Dockerfile"
done

for component in csi-sidecars snapshot-controller snapshot-conversion-webhook; do
  dockerfile="$image_context/Dockerfile"
  if [[ "$component" != csi-sidecars ]]; then
    dockerfile="$image_context/cmd/$component/Dockerfile"
  fi
  podman build --file "$dockerfile" \
    --tag "$component:$image_tag" "$image_context"
done

python3 "$image_context/tools/scripts/verify_artifacts.py" images \
  --engine podman --tag "$image_tag"
printf 'Build log and artifacts: %s\n' "$build_root"
```

The verifier checks the entrypoint, compares the packaged executable byte for
byte with the built binary, and runs component-specific `--help` checks with
container networking disabled. These are packaging/CLI checks, not a substitute
for controller, webhook, or cluster integration tests. The images remain in
Podman's local store; this workflow does not publish them to a registry.

## 4. Cleanup

After inspecting the results, remove this run's builder and source volume:

```bash
podman rm "$build_name"
podman volume rm "$source_volume"
```

The Go cache volumes, local images, and `$build_root` logs/artifacts remain for
reuse or investigation. To remove this run's images when no longer needed:

```bash
for component in csi-sidecars snapshot-controller snapshot-conversion-webhook; do
  podman rmi "$component:$image_tag"
done
```

Do not prune unrelated containers or volumes. The host repository and any
uncommitted work in it are not changed by these steps.

For the scope of current automated checks and the destructive GOPATH behavior
of the separate e2e tooling, return to the [README](../README.md).
