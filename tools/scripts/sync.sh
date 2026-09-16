#!/bin/bash

# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -euxo pipefail
export LC_ALL=C
export GOTOOLCHAIN=local
export GOENV=off GOFLAGS="" GOWORK=off
export GOSUMDB=sum.golang.org GOPRIVATE="" GONOSUMDB=""

if [[ $(uname) != "Linux" ]]; then
  echo "This script only works in Linux arm64/amd64, yours is $(uname)"
  exit 1
fi

# The script assembles the generated tree at the repository root; resolve it from
# the script location so the sync can be started from any directory.
SCRIPT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

# shellcheck source=tools/scripts/retry-go-dependencies.sh
source "${REPO_ROOT}/tools/scripts/retry-go-dependencies.sh"

# ==============================================================================
# DEVELOPER WORKSPACE PATH NORMALIZATION
# ==============================================================================
# When developers run this synchronization script locally, their terminal output
# and the resulting 'tools/sync.log' file capture absolute file paths unique to
# their specific machine/box (e.g., '/home/mauriciopoppe.linux' or '/root').
#
# Since 'tools/sync.log' is a tracked file in version control (linked by the
# README.md as a reference log of a successful synchronization), these absolute
# paths cause persistent git diff noise and merge conflicts whenever different
# developers run the tooling.
#
# To solve this cleanly without manual post-processing, the block below intercepts
# the script execution. If 'NORMALIZED_LOGGING' is not active, it re-executes the
# script and filters all stdout and stderr in real-time through GNU 'sed'.
# Any absolute path matching the current working directory ($PWD) or the user's
# home directory ($HOME) is replaced with generic placeholders ('$WORKSPACE'
# and '$HOME' respectively).
#
# Because 'set -o pipefail' is active (line 2), the exit status of the underlying
# execution is correctly preserved and bubbled up to the caller (or CI runner).
# ==============================================================================
if [[ "${NORMALIZED_LOGGING:-}" != "true" ]]; then
  export NORMALIZED_LOGGING=true
  escaped_pwd=$(echo "$PWD" | sed 's/[.[\*^$]/\\&/g')
  escaped_home=$(echo "$HOME" | sed 's/[.[\*^$]/\\&/g')
  "${SCRIPT}" "$@" 2>&1 | sed -u -e "s|$escaped_pwd|\$WORKSPACE|g" -e "s|$escaped_home|\$HOME|g"
  exit $?
fi


# Source locks never authorize reusing a previously transformed tree. Preflight
# before installing tools, fetching sources, or changing generated inputs.
SOURCE_LOCK="${REPO_ROOT}/tools/assembly/sources.lock.json"
python3 tools/scripts/assembly_sources.py --lock "${SOURCE_LOCK}" preflight --root "${REPO_ROOT}"

# A checkout of this repository contains the committed generated tree. Preflight
# verified everything present is tracked and clean, so regeneration removes it
# before any assembly input is created (tmp/ is reserved atomically below).
"${REPO_ROOT}/tools/scripts/cleanup.sh"

# go.mod/go.work are generated from the original source requirements, so a fresh
# assembly always needs the explicit Kubernetes release to align the family on.
if [[ $# != 2 || $1 != "--update-dependencies" || ! $2 =~ ^1\.[0-9]+\.[0-9]+$ ]]; then
  echo "Usage: sync.sh --update-dependencies 1.MINOR.PATCH"
  exit 1
fi
UPDATE_KUBERNETES="$2"

python3 -B tools/scripts/image_inputs.py preflight
python3 -B tools/scripts/build_environment.py bootstrap
# shellcheck disable=SC1091 # created exclusively by the hash-verified bootstrap
source .assembly-env/bin/activate
python3 -B tools/scripts/build_environment.py verify
export CC=/usr/bin/gcc CGO_ENABLED=1

# Run the real exact-revision/filter/import fixture with the installed tool.
python3 -B -m unittest discover -s tools/scripts -p '*_test.py'

TRASH="trash"
if ! command -v trash >/dev/null 2>&1; then
  TRASH="rm -rf"
fi

# Atomic reservation prevents concurrent syncs from sharing an output tree.
# The directory remains after failure so a partial run cannot be reused.
mkdir tmp
# Deterministic commit timestamps for the merged upstream history.
SOURCE_DATE_EPOCH=$(git show -s --format=%ct HEAD)
export SOURCE_DATE_EPOCH
mkdir -p pkg cmd/csi-sidecars/ staging/src/github.com/kubernetes-csi/
# Freeze the selected lock before fetching upstream repositories.
cp "${SOURCE_LOCK}" tmp/sources.lock.json
SOURCE_LOCK="${REPO_ROOT}/tmp/sources.lock.json"
python3 tools/scripts/assembly_sources.py --lock "${SOURCE_LOCK}" manifest >tmp/source-provenance.json

# Marker: pkg/ (and the assembly cmd/ + staging/) are auto-generated by this
# script from upstream kubernetes-csi repositories. The hand-maintained source
# of truth lives under tools/. See CODE_LAYOUT.md.
cat <<'EOF' >pkg/.GENERATED
This directory is auto-generated by tools/scripts/sync.sh from the upstream
kubernetes-csi/external-* repositories. Do not edit its contents by hand; edit
the source of truth under tools/ and re-run the sync. See CODE_LAYOUT.md.
EOF

# Initialize the target repo for merged commit history
if [[ ! -d tmp/csi-sidecars ]]; then
  mkdir -p tmp/csi-sidecars
  (cd tmp/csi-sidecars && git init)
fi

# symlink_from_root_to_tools creates a symlink in the assembly area (project root)
# pointing at the hand-maintained source of truth under tools/.
#
# Usage:
# symlink_from_root_to_tools <tools/ prefixed path>
#
# Example:
# symlink_from_root_to_tools tools/cmd/csi-sidecars/main.go
# (creates the symlink cmd/csi-sidecars/main.go -> tools/cmd/csi-sidecars/main.go)
symlink_from_root_to_tools() {
  file="$1"
  # strip tools/ prefix to compute the assembly-area destination
  file_without_tools="${file#tools/}"
  mkdir -p "$(dirname "${file_without_tools}")"
  ln -sf "$(realpath --relative-to="$(dirname "${file_without_tools}")" "${file}")" "${file_without_tools}"
}

# add_generation_marker inserts the Go generated-code marker
# (https://go.dev/s/generatedcode) immediately before the package clause, so the
# upstream Apache license header stays at the top of the file.
#
# Usage:
# add_generation_marker <file> <upstream origin>
#
# Example:
# add_generation_marker cmd/csi-sidecars/attacher_main.go external-attacher
add_generation_marker() {
  file="$1"
  origin="$2"
  if grep -q '^// Code generated ' "${file}"; then
    return 0
  fi
  awk -v marker="// Code generated by tools/scripts/sync.sh from ${origin}; DO NOT EDIT." '
    !marked && /^package / { print marker; print ""; marked = 1 }
    { print }
  ' "${file}" >"${file}.marked" && mv "${file}.marked" "${file}"
}

# Branches in sidecars.conf are update metadata only. Ordinary assembly consumes
# exact original commits and preserves a stable ref across history filtering.
SIDECAR_LIST=$(python3 tools/scripts/assembly_sources.py --lock "${SOURCE_LOCK}" controllers)
for SIDECAR in ${SIDECAR_LIST}; do
    python3 tools/scripts/assembly_sources.py --lock "${SOURCE_LOCK}" checkout "${SIDECAR}" "tmp/external-${SIDECAR}"
    (
      cd tmp/external-${SIDECAR}

      # The checkout is a new, private repository containing the selected history.
      git filter-repo \
        --commit-callback "
original_hash = commit.original_id.decode()
original_message = commit.message.decode()
new_message = f\"{original_message}\n\nImported-from: external-${SIDECAR}\n\nOriginal-commit-hash: {original_hash}\"
commit.message = new_message.encode()
        " \
        --to-subdirectory-filter pkg/${SIDECAR} --force
    )

    # Merge the rewritten sidecar history into the target repo
    (
      cd tmp/csi-sidecars
      export GIT_AUTHOR_NAME="CSI AIO Sync" GIT_COMMITTER_NAME="CSI AIO Sync"
      export GIT_AUTHOR_EMAIL="csi-aio-sync@localhost" GIT_COMMITTER_EMAIL="csi-aio-sync@localhost"
      export GIT_AUTHOR_DATE="${SOURCE_DATE_EPOCH} +0000" GIT_COMMITTER_DATE="${SOURCE_DATE_EPOCH} +0000"
      git remote add external-${SIDECAR} ../external-${SIDECAR}
      git fetch external-${SIDECAR} refs/heads/csi-aio-import:refs/remotes/external-${SIDECAR}/csi-aio-import
      git merge external-${SIDECAR}/csi-aio-import --allow-unrelated-histories --no-edit --quiet >/dev/null
    )

    # Copy the sidecar files (without .git) for processing
    cp -a tmp/external-${SIDECAR}/pkg/${SIDECAR} pkg/${SIDECAR}

    ${TRASH} pkg/${SIDECAR}/.github
    ${TRASH} pkg/${SIDECAR}/vendor
    ${TRASH} pkg/${SIDECAR}/release-tools
    ${TRASH} pkg/${SIDECAR}/go.mod
    ${TRASH} pkg/${SIDECAR}/go.sum
    ${TRASH} pkg/${SIDECAR}/Dockerfile
    ${TRASH} pkg/${SIDECAR}/.cloudbuild.sh
    ${TRASH} pkg/${SIDECAR}/cloudbuild.yaml
    ${TRASH} pkg/${SIDECAR}/.prow.sh
    ${TRASH} pkg/${SIDECAR}/OWNER_ALIASES
    ${TRASH} pkg/${SIDECAR}/Makefile

    if [ "${SIDECAR}" = "snapshotter" ]; then
      ${TRASH} pkg/${SIDECAR}/client/.git
      ${TRASH} pkg/${SIDECAR}/client/go.mod
      ${TRASH} pkg/${SIDECAR}/client/go.sum
      ${TRASH} pkg/${SIDECAR}/client/hack
      ${TRASH} pkg/${SIDECAR}/CHANGELOG
      ${TRASH} pkg/${SIDECAR}/examples
      ${TRASH} pkg/${SIDECAR}/deploy
      ${TRASH} pkg/${SIDECAR}/hack
      # snapshot-conversion-webhook is kept as an independent binary in cmd/snapshot-conversion-webhook
      # ${TRASH} pkg/${SIDECAR}/cmd/snapshot-conversion-webhook
      ${TRASH} pkg/${SIDECAR}/SECURITY_CONTACTS
      ${TRASH} pkg/${SIDECAR}/code-of-conduct.md
      ${TRASH} pkg/${SIDECAR}/CONTRIBUTING.md
      ${TRASH} pkg/${SIDECAR}/OWNERS_ALIASES
    fi

    (
      cd pkg/${SIDECAR}
      find . -type f -exec grep -q "github.com/kubernetes-csi/external-${SIDECAR}/" --files-with-matches {} \; -print
    )

    (
      cd pkg/${SIDECAR}
      if [ "${SIDECAR}" = "snapshotter" ]; then
        # shellcheck disable=SC2038 # upstream Go sources have no whitespace/special chars in their paths
        find . -type f -exec grep -q "github.com/kubernetes-csi/external-${SIDECAR}/" --files-with-matches {} \; -print |
          xargs -r sed -E -i".bak" -e "s%github.com/kubernetes-csi/external-snapshotter/v8/%github.com/kubernetes-csi/csi-sidecars/pkg/snapshotter/%g" \
                                -e "s%github.com/kubernetes-csi/external-snapshotter/client/v8/%github.com/kubernetes-csi/csi-sidecars/pkg/snapshotter/client/%g"
      else
        # shellcheck disable=SC2038 # upstream Go sources have no whitespace/special chars in their paths
        find . -type f -exec grep -q "github.com/kubernetes-csi/external-${SIDECAR}/" --files-with-matches {} \; -print |
          xargs -r sed -E -i".bak" "s%github.com/kubernetes-csi/external-${SIDECAR}/(v[0-9]+/)?%github.com/kubernetes-csi/csi-sidecars/pkg/${SIDECAR}/%g"
      fi
    )

  # After cloning a CSI repository its entrypoints have additional code that now belong
  # to this codebase, for example:
  #
  # - A main() function - CSI repositories no longer need them.
  # - Flags, logging code
  # may have code that
  # shellcheck disable=SC2044 # entrypoint filenames are plain *.go, no whitespace/globbing risk
  for FILE in $(find pkg/${SIDECAR}/cmd/csi-${SIDECAR}/ -maxdepth 1 -name '*.go' ! -name '*_test.go' | sort); do
    NEW_FILE="cmd/csi-sidecars/${SIDECAR}_$(basename ${FILE})"
    cp -v -- "${FILE}" "${NEW_FILE}"
    # Rename main()
    sed -i".bak" "s/func main()/func ${SIDECAR}_main(ctx context.Context)/g" "${NEW_FILE}"
    # Remove variables (mostly flags)
    sed -i".bak" '/^var (/,/^)/d' "${NEW_FILE}"
    # Pass context from main.go
    sed -i".bak" '/ctx :=/d' "${NEW_FILE}"
    sed -i".bak" 's/context.TODO()/ctx/g' "${NEW_FILE}"

    # Flags/logging code that must be removed
    sed -i".bak" '/flag.Var/d' "${NEW_FILE}"
    sed -i".bak" '/featuregate.NewFeatureGate/d' "${NEW_FILE}"
    sed -i".bak" '/logsapi.AddFeatureGates/d' "${NEW_FILE}"
    sed -i".bak" '/Options are:/d' "${NEW_FILE}"
    sed -i".bak" '/logsapi.NewLoggingConfiguration/d' "${NEW_FILE}"
    sed -i".bak" '/logsapi.AddGoFlags/d' "${NEW_FILE}"
    sed -i".bak" '/logsapi.AddFlags/d' "${NEW_FILE}"
    sed -i".bak" '/logs.InitLogs/d' "${NEW_FILE}"
    sed -i".bak" '/flag.Parse/d' "${NEW_FILE}"
    sed -i".bak" '/logsapi.ValidateAndApply/,/}/d' "${NEW_FILE}"
    sed -i".bak" '/klog.InitFlags/d' "${NEW_FILE}"
    sed -i".bak" '/logtostderr/d' "${NEW_FILE}"
    # sed -i".bak" '/utilfeature.DefaultMutableFeatureGate/,/}/d' "${NEW_FILE}"
    sed -i".bak" '/^\tif !utilfeature\.DefaultMutableFeatureGate/,/^\t}/d' "${NEW_FILE}"
    sed -i".bak" '/flag.CommandLine.AddGoFlagSet/d' "${NEW_FILE}"

    # TODO: handle setting the automaxproc flag from each sidecar>
    # In the meantime remove setting the flag and handle it in the AIO sidecar.
    # https://github.com/mauriciopoppe/csi-sidecars-aio-poc/issues/14
    sed -i".bak" '/standardflags.AddAutomaxprocs/d' "${NEW_FILE}"
    sed -i".bak" '/standardflags.RegisterCommonFlags/d' "${NEW_FILE}"

    # Standalone var version (outside var() blocks) conflicts with main.go
    sed -i".bak" '/^var version/d' "${NEW_FILE}"

    # Dead imports
    sed -i".bak" '/goflag/d' "${NEW_FILE}"
    sed -i".bak" '/flag"/d' "${NEW_FILE}"
    sed -i".bak" '/featuregate"/d' "${NEW_FILE}"
    sed -i".bak" '/logs/d' "${NEW_FILE}"

    if [ "${SIDECAR}" = "resizer" ]; then
      sed -i".bak" '/strings/d' "${NEW_FILE}"
      # Upstream external-resizer added dedicated --resize-timeout/--modify-timeout
      # flags plus a resolveOperationTimeouts() helper that inspects
      # flag.CommandLine (stdlib flag) to decide whether an explicitly-set
      # --timeout overrides them. The AIO owns flag parsing (pflag) and exposes
      # the two timeouts as resizer-prefixed flags wired into global vars
      # (resizeTimeout/modifyTimeout) by copyFlagsFromConfigToGlobalVars, so the
      # helper and its flag.CommandLine dependency don't apply here. Drop the
      # helper definition and consume the globals directly at the call site.
      #
      # Remove the resolveOperationTimeouts helper (its doc comment through the
      # closing brace of the function).
      sed -i".bak" '/^\/\/ resolveOperationTimeouts /,/^}/d' "${NEW_FILE}"
      # Replace the call to the (now removed) helper with a direct read of the
      # resizer timeout globals. The upstream call spans two lines.
      sed -i".bak" '/effectiveResizeTimeout, effectiveModifyTimeout := resolveOperationTimeouts(/{N;s/.*/\teffectiveResizeTimeout, effectiveModifyTimeout := *resizeTimeout, *modifyTimeout/;}' "${NEW_FILE}"
    fi
    if [ "${SIDECAR}" = "attacher" ]; then
      sed -i".bak" '/strings/d' "${NEW_FILE}"
      # Remove flag registration that uses flag.CommandLine (handled by main.go via RegisterAttacherFlagsWithPrefix)
      sed -i".bak" '/RegisterAttacherFlags.*flag.CommandLine/d' "${NEW_FILE}"
      sed -i".bak" '/^var attacherConfiguration/d' "${NEW_FILE}"
      # Remove local var re-declarations that shadow globals set by copyFlagsFromConfigToGlobalVars
      sed -i".bak" '/attacherConfiguration\./d' "${NEW_FILE}"
      sed -i".bak" '/attacherconfiguration "/d' "${NEW_FILE}"
      # Replace standardflags.Configuration field accesses with global vars (order matters: longest match first)
      sed -i".bak" 's/standardflags\.Configuration\.ShowVersion/*showVersion/g' "${NEW_FILE}"
      sed -i".bak" 's/standardflags\.Configuration\.MetricsAddress/*metricsAddress/g' "${NEW_FILE}"
      sed -i".bak" 's/standardflags\.Configuration\.HttpEndpoint/*httpEndpoint/g' "${NEW_FILE}"
      sed -i".bak" 's/standardflags\.Configuration\.KubeConfig/*kubeconfig/g' "${NEW_FILE}"
      sed -i".bak" 's/standardflags\.Configuration\.CSIAddress/*csiAddress/g' "${NEW_FILE}"
      sed -i".bak" 's/standardflags\.Configuration\.MetricsPath/*metricsPath/g' "${NEW_FILE}"
      # Remove only the standardflags.RegisterCommonFlags import alias line if present,
      # but keep the standardflags package import and bare standardflags.Configuration
      # usages (passed to libconfig.BuildConfig and leaderelection.RunWithLeaderElection).
      # The field accesses (e.g. standardflags.Configuration.ShowVersion) were already
      # replaced with global vars above.
    fi
    if [ "${SIDECAR}" = "provisioner" ]; then
      # Remove pre-Go 1.21 max() helper that shadows the builtin
      sed -i".bak" '/^\/\/ max returns/,/^}/d' "${NEW_FILE}"
    fi
    if [ "${SIDECAR}" = "snapshotter" ]; then
      # NOTE: unlike other sidecars, do NOT remove strings import for snapshotter
      # because it's used in the leaderelection.RunWithLeaderElection call
      # Restore the prefix var that was inside var(...) block (stripped by sed)
      sed -i".bak" '/^func snapshotter_main/i\var snapshotterPrefix = "external-snapshotter-leader"' "${NEW_FILE}"
      sed -i".bak" 's/\bprefix\b/snapshotterPrefix/g' "${NEW_FILE}"
      # Rename colliding variables to avoid conflicts with other sidecar globals
      sed -i".bak" 's/\bthreads\b/snapshotterThreads/g' "${NEW_FILE}"
      sed -i".bak" 's/\bextraCreateMetadata\b/snapshotterExtraCreateMetadata/g' "${NEW_FILE}"
      sed -i".bak" 's/\benableNodeDeployment\b/snapshotterEnableNodeDeployment/g' "${NEW_FILE}"
      sed -i".bak" 's/\bcsiTimeout\b/snapshotterCSITimeout/g' "${NEW_FILE}"
      sed -i".bak" 's/\bbuildConfig\b/snapshotterBuildConfig/g' "${NEW_FILE}"
    fi

    add_generation_marker "${NEW_FILE}" "external-${SIDECAR}"
  done
  if [[ ${SIDECAR} == attacher ]]; then
    symlink_from_root_to_tools tools/pkg/attacher/cmd/csi-attacher/main.go
  fi
done

# Copy snapshot-controller entrypoint into its own independent cmd/ directory.
# Unlike the sidecar entrypoints which merge into cmd/csi-sidecars/main.go,
# snapshot-controller keeps its own main() as a separate binary.
# NOTE: Import path replacement for both the main module and the client module
# is already handled by the sed block inside pkg/snapshotter/ above, which
# recursively covers cmd/snapshot-controller/*.go.
mkdir -p cmd/snapshot-controller
cp -v pkg/snapshotter/cmd/snapshot-controller/*.go cmd/snapshot-controller/
for FILE in cmd/snapshot-controller/*.go; do
  add_generation_marker "${FILE}" "external-snapshotter"
done

# Copy snapshot-conversion-webhook entrypoint into its own independent cmd/ directory.
# Like snapshot-controller, the webhook keeps its own main() as a separate binary.
# NOTE: Import path replacement is already handled by the sed block inside
# pkg/snapshotter/ above.
mkdir -p cmd/snapshot-conversion-webhook
cp -v pkg/snapshotter/cmd/snapshot-conversion-webhook/*.go cmd/snapshot-conversion-webhook/
for FILE in cmd/snapshot-conversion-webhook/*.go; do
  add_generation_marker "${FILE}" "external-snapshotter"
done

# Per-cmd Dockerfiles: build.make's container-% target uses ./cmd/<name>/Dockerfile
# when present and otherwise falls back to the root Dockerfile, which hardcodes
# the csi-sidecars binary and entrypoint. Without these, snapshot-controller and
# snapshot-conversion-webhook images would ship the wrong binary.
# The `binary` ARG is required by release-tools/cloudbuild.yaml: push-multiarch-%
# passes --build-arg binary=./bin/<cmd><arch-suffix>, so the COPY must consume it
# or non-amd64 images would silently embed the host-arch binary.
# One checked template binds all three Dockerfiles to the maintained image lock.
python3 -B tools/scripts/image_inputs.py generate

# The sed -i".bak" rewrites above leave backup files behind in the generated
# area; drop them so the assembly tree contains only the transformed sources.
find cmd pkg staging -name '*.bak' -delete

# Fetch the selected utility source before resolving any dependency graph.
# Original controller manifests remain in tmp/external-*/ for attribution.
csi_lib_utils=staging/src/github.com/kubernetes-csi/csi-lib-utils
python3 tools/scripts/assembly_sources.py --lock "${SOURCE_LOCK}" checkout csi-lib-utils "${csi_lib_utils}"
# Its original SHA is retained in tmp/source-provenance.json before .git removal.
${TRASH} ${csi_lib_utils}/.git
${TRASH} ${csi_lib_utils}/.github
${TRASH} ${csi_lib_utils}/vendor
${TRASH} ${csi_lib_utils}/release-tools

# The new entrypoint for all the sidecars
symlink_from_root_to_tools tools/cmd/csi-sidecars/main.go
# Tooling tests for the AIO entrypoint (parseControllers, config->global mapping).
symlink_from_root_to_tools tools/cmd/csi-sidecars/main_test.go
# The utility global function to register common and per-sidecar flags.
symlink_from_root_to_tools tools/cmd/csi-sidecars/config/flags.go
# Tooling tests for the AIO flag registration.
symlink_from_root_to_tools tools/cmd/csi-sidecars/config/flags_test.go
# The utility glofal functions to register attacher flags.
symlink_from_root_to_tools tools/pkg/attacher/cmd/csi-attacher/config/flags.go
# Tooling tests for the attacher flag registration.
symlink_from_root_to_tools tools/pkg/attacher/cmd/csi-attacher/config/flags_test.go

# Generate go.mod/go.work from the original source requirements, aligning the
# Kubernetes family on the explicitly selected release, then resolve and vendor.
# GOWORK is still off here so `go mod tidy` operates on the root module alone.
python3 -B tools/scripts/assembly_dependencies.py seed --kubernetes "${UPDATE_KUBERNETES}"
retry_go_dependencies go mod tidy
export GOWORK="${REPO_ROOT}/go.work"

retry_go_dependencies go work vendor
export GOFLAGS="-mod=vendor"

# Echo each checkpoint command before running it so every step is visible in
# the log.
set -x

# checkpoint: run the tooling unit tests (flag registration + AIO entrypoint
# helpers). These only exist after the symlinks above are in place and the
# merged module resolves, so they run here rather than from the repo root.
go test -timeout=5m ./cmd/csi-sidecars/... ./pkg/attacher/cmd/csi-attacher/config/...

# checkpoint: build all release binaries with the inherited plain go build.
make build
./bin/csi-sidecars --help
./bin/snapshot-controller --help
./bin/snapshot-conversion-webhook --help

set +x

echo "Complete!"
echo "Merged commit history available at tmp/csi-sidecars/"
