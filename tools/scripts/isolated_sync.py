#!/usr/bin/env python3
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

"""Run Linux assembly on a fresh copy; retain source and logs under .work/."""

import argparse
import difflib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile

import assembly_sources
import assembly_lock
import build_environment
import build_provenance

ROOT = Path(__file__).resolve().parents[2]
MAINTAINED_PACKAGES = (
    "./cmd/csi-sidecars/... ./pkg/attacher/cmd/csi-attacher/config/... "
    "./pkg/runtime/... ./pkg/leaderelection/... ./pkg/csistartup/... "
    "./pkg/resizer/pkg/csi/... ./pkg/provisioner/pkg/owner/... ./tools/cmd/lifecycle-adapter/..."
)
FRESH_VALIDATION = (
    'export GOENV=off GOFLAGS=-mod=vendor GOWORK="$PWD/go.work" CGO_ENABLED=1 CC=/usr/bin/gcc && '
    "python3 -B tools/scripts/build_environment.py verify && "
    "python3 -B tools/scripts/build_provenance.py verify && "
    "python3 -B tools/scripts/assembly_dependencies.py vendor && "
    f"go test -race -count=1 -timeout=10m {MAINTAINED_PACKAGES} && "
    f"go vet {MAINTAINED_PACKAGES} && python3 -B tools/scripts/verify_artifacts.py cli && "
    'python3 -B tools/scripts/build_binaries.py verify --arch "$(python3 -B tools/scripts/build_binaries.py arch)"'
)


def snapshot(root, destination):
    """Copy working inputs, including the complete verified release-tools tree."""
    root = root.resolve()
    subprocess.run(["git", "clone", "--local", "--no-hardlinks", "--no-checkout",
                    str(root), str(destination)], check=True)
    tracked = subprocess.check_output(
        ["git", "-C", str(root), "ls-files", "-z", "--cached"])
    added = subprocess.check_output(
        ["git", "-C", str(root), "ls-files", "-z", "--others",
         "--exclude-standard", "--", "tools"])
    inherited = set()
    if (root / "release-tools").exists() or (root / "release-tools").is_symlink():
        inherited = {os.fsencode("release-tools/" + name) for name in build_provenance.local_files(root)}
    for name in sorted((set((tracked + added).split(b"\0")) | inherited) - {b""}):
        relative = Path(os.fsdecode(name))
        source = root / relative
        target = destination / relative
        if source.is_symlink():
            link = os.readlink(source)
            if Path(link).is_absolute() or not source.resolve().is_relative_to(root):
                raise ValueError(f"source symlink escapes snapshot: {relative}")
            target.parent.mkdir(parents=True, exist_ok=True)
            target.symlink_to(link)
        elif source.exists():
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(source, target)
        # Absent tracked files preserve deletions in the working tree.


def runtime_snapshot(baseline, checkout):
    """Replay pre-adaptation inputs for fast runtime tests, not clean assembly."""
    for name in ("cmd", "pkg", "staging", "vendor"):
        shutil.copytree(baseline / name, checkout / name, symlinks=True)
    for name in ("go.mod", "go.sum", "go.work", "go.work.sum"):
        if (baseline / name).exists():
            shutil.copy2(baseline / name, checkout / name)
    for tree in ("cmd/csi-sidecars", "pkg"):
        for source in (checkout / "tools" / tree).rglob("*.go"):
            target = checkout / source.relative_to(checkout / "tools")
            target.parent.mkdir(parents=True, exist_ok=True)
            if target.exists() or target.is_symlink():
                target.unlink()
            target.symlink_to(os.path.relpath(source, target.parent))
    entries = {"attacher": "attacher_main.go", "provisioner": "provisioner_csi-provisioner.go",
               "resizer": "resizer_main.go", "snapshotter": "snapshotter_main.go"}
    commands = ["GO111MODULE=off go build -o /tmp/lifecycle-adapter ./tools/cmd/lifecycle-adapter"]
    commands += [f"/tmp/lifecycle-adapter {controller} cmd/csi-sidecars/{name}"
                 for controller, name in entries.items()]
    commands += ["/tmp/lifecycle-adapter --workers .", "/tmp/lifecycle-adapter --dependencies .",
                 "go test -race -count=1 -timeout=5m ./cmd/csi-sidecars/... ./pkg/runtime/... "
                 "./pkg/leaderelection/... ./pkg/csistartup/... ./pkg/resizer/pkg/csi/... ./pkg/provisioner/pkg/owner/..."]
    return " && ".join(commands)


def workspace_path(value):
    # Keep mount destinations out of system/tool directories; no shell metacharacters.
    if value != "/workspace" and not re.fullmatch(r"/build/[a-zA-Z0-9_-]+(?:/[a-zA-Z0-9_-]+)*", value):
        raise argparse.ArgumentTypeError("workspace must be /workspace or a safe path under /build/")
    return value


def container_command(engine, image, checkout, script, offline=False, workspace="/workspace"):
    workspace_path(workspace)
    command = [engine, "run", "--rm", "--pull=never", "--cap-drop=ALL",
               "--security-opt=no-new-privileges", "--env", "GOTOOLCHAIN=local",
               "--env", f"CSI_AIO_BUILDER_IMAGE={image}",
               "--volume", f"{checkout}:{workspace}", "--workdir", workspace]
    if engine == "docker" and os.uname().sysname == "Linux":
        # Keep CI snapshots owned by the checkout user without weakening Git trust.
        command += ["--user", f"{os.getuid()}:{os.getgid()}", "--env", "HOME=/tmp",
                    "--env", "GOPATH=/tmp/go", "--env", "GOCACHE=/tmp/go-build"]
    if offline:
        command.append("--network=none")
    return command + [image, "bash", "-c", script]


def export_candidate(data, output, validator=assembly_sources.validate):
    """Atomically expose a validated candidate without replacing any existing file."""
    validator(json.loads(data, object_pairs_hook=assembly_sources.unique_object))
    output.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(dir=output.parent, prefix=".source-candidate-") as temporary:
        temporary.write(data)
        temporary.flush()
        os.fsync(temporary.fileno())
        os.link(temporary.name, output)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--engine", default="podman", choices=("podman", "docker"))
    parser.add_argument("--workspace", default="/workspace", type=workspace_path,
                        help="Linux mount path for independent path-variation checks")
    parser.add_argument("--image", help="Optional locked digest; arbitrary images allowed only for runtime replay")
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--tooling-only", action="store_true",
                      help="Validate tools/ in the locked builder without assembling or selecting sources")
    mode.add_argument("--runtime-baseline", type=Path,
                      help="Replay retained pre-adaptation .work source; not clean assembly evidence")
    mode.add_argument("--update-sources", action="store_true",
                      help="Resolve update channels and validate in isolation; never replace the active lock")
    mode.add_argument("--source-lock", type=Path,
                      help="Validate an explicitly selected source lock without promoting it")
    mode.add_argument("--dependency-lock", type=Path,
                      help="Validate a complete source/dependency bundle without promoting it")
    parser.add_argument("--update-dependencies", metavar="1.MINOR.PATCH",
                        help="Explicitly resolve and validate a complete dependency candidate")
    parser.add_argument("--candidate-output", type=Path,
                        help="New candidate file under .work; required for selection/update modes")
    args = parser.parse_args()
    work = ROOT / ".work"
    if args.update_dependencies and (args.runtime_baseline or args.dependency_lock or args.tooling_only or
                                    not re.fullmatch(r"1\.[0-9]+\.[0-9]+", args.update_dependencies)):
        parser.error("--update-dependencies requires 1.MINOR.PATCH and cannot replay a baseline/bundle")
    if bool(args.update_sources or args.source_lock or args.dependency_lock or args.update_dependencies) != bool(args.candidate_output):
        parser.error("selection/update modes require --candidate-output (and vice versa)")
    bundle = assembly_lock.load(args.dependency_lock) if args.dependency_lock else None
    selected = assembly_sources.encoded(assembly_sources.load(args.source_lock)) if args.source_lock else None
    if bundle is not None:
        selected = assembly_sources.encoded(bundle["sources"])
    if args.candidate_output:
        output = args.candidate_output.absolute()
        if not output.resolve().is_relative_to(work.resolve()):
            parser.error("candidate output must be under this checkout's .work")
        if output.exists() or output.is_symlink():
            parser.error("candidate output already exists; it will not be replaced")
        args.candidate_output = output
    if args.runtime_baseline:
        args.runtime_baseline = args.runtime_baseline.resolve()
        if not args.runtime_baseline.is_relative_to(work.resolve()):
            parser.error("runtime baseline must be retained under this checkout's .work")
    if not args.runtime_baseline:
        args.image = build_environment.select_image(build_environment.load(ROOT), args.image)
        build_provenance.verify(ROOT)
    elif args.image is None:
        parser.error("historical runtime replay requires an explicit --image")
    work.mkdir(exist_ok=True)
    run = Path(tempfile.mkdtemp(prefix="assembly-", dir=work))
    checkout = run / "source"
    snapshot(ROOT, checkout)
    if not args.runtime_baseline:
        build_provenance.verify(checkout)
    candidate = None
    original = b""
    if args.update_sources or selected is not None or args.update_dependencies:
        path = checkout / "tools/assembly/sources.lock.json"
        original = path.read_bytes()
        candidate = selected if selected is not None else assembly_sources.encoded(
            assembly_sources.resolve_candidate(assembly_sources.load(path), checkout / "tools/scripts/sidecars.conf")
            if args.update_sources else assembly_sources.load(path))
        path.write_bytes(candidate)
    if bundle is not None:
        (checkout / assembly_lock.LOCK).write_bytes(assembly_lock.encoded(bundle))
    sync_args = f" --update-dependencies {args.update_dependencies}" if args.update_dependencies else ""
    lock_path = assembly_lock.CANDIDATE if args.update_dependencies else assembly_lock.LOCK
    script = (f"./tools/scripts/sync.sh{sync_args} && source .assembly-env/bin/activate && "
              + FRESH_VALIDATION + f" && python3 -B tools/scripts/assembly_lock.py --lock {lock_path} adapted")
    if args.tooling_only:
        script = (
            "python3 -B tools/scripts/build_provenance.py verify && "
            "python3 -B tools/scripts/build_environment.py bootstrap && "
            "source .assembly-env/bin/activate && python3 -B tools/scripts/build_environment.py verify && "
            "python3 -B -m unittest discover -s tools/scripts -p '*_test.py' && "
            'test -z "$(gofmt -l tools/)" && ./release-tools/verify-boilerplate.sh "$PWD/tools" && '
            "python3 -B tools/scripts/build_provenance.py verify"
        )
    if args.runtime_baseline:
        script = runtime_snapshot(args.runtime_baseline, checkout)
        print("Runtime replay only: this is not a clean assembly.", flush=True)
    log = run / "assembly.log"
    print(f"Isolated source: {checkout}\nAssembly log: {log}", flush=True)
    command = container_command(args.engine, args.image, checkout, script,
                                offline=args.runtime_baseline is not None, workspace=args.workspace)
    with log.open("w") as output:
        result = subprocess.run(command, stdout=output, stderr=subprocess.STDOUT)
    print(f"Assembly exit status: {result.returncode}; retained at {run}")
    if result.returncode:
        print("\n".join(log.read_text(errors="replace").splitlines()[-60:]))
    elif candidate is not None:
        if args.update_dependencies or bundle is not None:
            result_bundle = assembly_lock.load(checkout / lock_path)
            if (assembly_sources.encoded(result_bundle["sources"]) != candidate or
                    result_bundle["inputs_sha256"] != assembly_lock.inputs_hash(checkout)):
                raise ValueError("validated dependency candidate does not match the selected inputs")
            export_candidate(assembly_lock.encoded(result_bundle), args.candidate_output, assembly_lock.validate)
            print(f"Validated source/dependency candidate: {args.candidate_output}; active locks unchanged")
        else:
            export_candidate(candidate, args.candidate_output)
            print(f"Validated source candidate: {args.candidate_output}; active lock unchanged")
        print("".join(difflib.unified_diff(
            original.decode().splitlines(keepends=True), candidate.decode().splitlines(keepends=True),
            fromfile="active source lock", tofile="validated source candidate")), end="")
        print("Source/runtime, Kubernetes-family, and canonical dependency checks passed; "
              "complete build provenance and release acceptance remain open")
    return result.returncode


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        raise SystemExit(f"Isolated assembly failed: {error}") from error
