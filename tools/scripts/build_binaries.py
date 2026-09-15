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

"""Build static Linux artifacts with checked, path-independent project metadata."""

import argparse
import json
import os
from pathlib import Path
import subprocess

import assembly_lock
import assembly_sources
import build_environment
import build_provenance
import image_inputs

ROOT = Path(__file__).resolve().parents[2]
PROJECT = "tmp/project-build.json"
COMMANDS = {name: f"./cmd/{name}" for name in (
    "csi-sidecars", "snapshot-controller", "snapshot-conversion-webhook")}
COMMANDS["csi-attacher"] = "./pkg/attacher/cmd/csi-attacher"
ARCHITECTURES = {"amd64": ("GOAMD64", "v1"), "arm64": ("GOARM64", "v8.0")}
FLAGS = ["-mod=vendor", "-trimpath", "-buildvcs=false"]


def relevant(name):
    return (name in (*build_provenance.ROOT_FILES, build_environment.LOCK,
                     build_provenance.LOCK, image_inputs.LOCK, "tools/assembly/sources.lock.json") or
            name.startswith("release-tools/") or
            (name.startswith("tools/") and Path(name).suffix in (".go", ".py", ".sh")))


def project_metadata(root):
    """Compare build inputs to HEAD objects, independent of the snapshot's index."""
    revision = build_provenance.git(root, "rev-parse", "HEAD^{commit}").decode().strip()
    if not build_provenance.digest(revision, 40):
        raise ValueError("project revision must be a full commit ID")
    epoch = int(build_provenance.git(root, "show", "-s", "--format=%ct", revision))
    if epoch < 0:
        raise ValueError("project commit has an invalid source epoch")
    fingerprint = assembly_lock.inputs_hash(root)
    current = {name: build_provenance.file_entry(root / name) for name in (
        *build_provenance.ROOT_FILES, build_environment.LOCK, build_provenance.LOCK,
        image_inputs.LOCK, "tools/assembly/sources.lock.json")}
    current.update({"release-tools/" + name: item for name, item in build_provenance.local_files(root).items()})
    for path in (root / "tools").rglob("*"):
        name = path.relative_to(root).as_posix()
        if relevant(name) and path.is_file():
            current[name] = build_provenance.file_entry(path)
    original = {}
    for record in build_provenance.git(root, "ls-tree", "-rz", revision).split(b"\0"):
        if not record:
            continue
        metadata, raw_name = record.split(b"\t", 1)
        name = raw_name.decode()
        if not relevant(name):
            continue
        mode, kind, blob = metadata.decode().split()
        if kind != "blob":
            raise ValueError(f"unsupported project input: {name}")
        original[name] = build_provenance.entry(mode, build_provenance.git(root, "cat-file", "blob", blob))
    dirty = original != current
    # The source selection is separately locked, but also distinguishes dirty versions.
    source_hash = assembly_lock.sha((root / "tools/assembly/sources.lock.json").read_bytes())
    identity = assembly_lock.sha(assembly_lock.encoded([fingerprint, source_hash]))
    return {"schema_version": 1, "revision": revision, "source_date_epoch": epoch,
            "dirty": dirty, "inputs_sha256": fingerprint, "source_lock_sha256": source_hash,
            "version": revision + ("-dirty." + identity[:12] if dirty else "")}


def capture(root):
    value = project_metadata(root)
    path = assembly_lock.safe_path(root, PROJECT)
    with path.open("xb") as output:
        output.write(assembly_lock.encoded(value))
    return value


def project(root):
    recorded = json.loads(assembly_lock.safe_path(root, PROJECT).read_text(),
                          object_pairs_hook=assembly_sources.unique_object)
    if assembly_lock.encoded(recorded) != assembly_lock.encoded(project_metadata(root)):
        raise ValueError("project build metadata changed; start a fresh assembly")
    return recorded


def environment(root, metadata, arch):
    if arch not in ARCHITECTURES:
        raise ValueError("explicit Linux amd64 or arm64 architecture required")
    # Do not inherit GOFLAGS, GOEXPERIMENT, GODEBUG, toolchain overrides, or CGO settings.
    env = {name: os.environ[name] for name in ("PATH", "HOME", "TMPDIR", "GOCACHE") if name in os.environ}
    env.update(GOENV="off", GOTOOLCHAIN="local", GOFLAGS="", GO111MODULE="on",
               GOWORK=str(root / "go.work"), CGO_ENABLED="0", GOOS="linux", GOARCH=arch,
               GOPROXY="off", GOSUMDB="off", GOTELEMETRY="off", GOEXPERIMENT="",
               SOURCE_DATE_EPOCH=str(metadata["source_date_epoch"]), LC_ALL="C", TZ="UTC")
    name, level = ARCHITECTURES[arch]
    env[name] = level
    return env


def metadata_source(metadata):
    # Every command, including the upstream webhook without --version, exposes
    # the same early, single-argument identity query without starting clients.
    data = assembly_lock.encoded(metadata).decode().strip()
    return ('// Code generated by tools/scripts/build_binaries.py; DO NOT EDIT.\n'
            'package main\n\nimport (\n\taioBuildFmt "fmt"\n\taioBuildOS "os"\n)\n\n'
            'func init() {\n\tif len(aioBuildOS.Args) == 2 && aioBuildOS.Args[1] == "--build-info" {\n'
            f'\t\taioBuildFmt.Println(`{data}`)\n\t\taioBuildOS.Exit(0)\n\t}}\n}}\n')


def command_line(command, metadata, output):
    return ["go", "build", *FLAGS, "-ldflags", f"-X main.version={metadata['version']}",
            "-o", str(output), COMMANDS[command]]


def build_info(root, binary, metadata, arch):
    env = environment(root, metadata, arch)
    text = subprocess.check_output(["go", "version", "-m", str(binary)], cwd=root, env=env, text=True, timeout=30)
    lines = text.splitlines()
    settings = {}
    path = None
    for line in lines[1:]:
        fields = line.strip().split("\t")
        if fields[0] == "build" and len(fields) == 2:
            key, _, value = fields[1].partition("=")
            settings[key] = value
        elif fields[0] == "path" and len(fields) == 2:
            path = fields[1]
    version = build_environment.load(root)["versions"]["go"]
    arch_key, arch_level = ARCHITECTURES[arch]
    expected = {"-trimpath": "true", "CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": arch,
                arch_key: arch_level, "-buildmode": "exe", "-compiler": "gc"}
    if (not lines or not lines[0].endswith(": " + version) or
            any(settings.get(k) != v for k, v in expected.items()) or
            any(k.startswith("vcs") for k in settings)):
        raise ValueError(f"unexpected Go binary metadata: {binary.name}")
    # Check the actual ELF machine, not only Go's metadata label.
    with binary.open("rb") as stream:
        header = stream.read(20)
    machine = 62 if arch == "amd64" else 183
    if header[:6] != b"\x7fELF\x02\x01" or int.from_bytes(header[18:20], "little") != machine:
        raise ValueError(f"unexpected ELF architecture: {binary.name}")
    return {"go_version": version, "package": path, "settings": settings}


def inputs(root):
    image_inputs.dockerfiles(root)
    metadata = project(root)
    paths = [assembly_lock.safe_path(root, name) for name in (assembly_lock.LOCK, assembly_lock.CANDIDATE)]
    available = [path for path in paths if path.exists()]
    if len(available) != 1:
        raise ValueError("expected exactly one canonical dependency bundle")
    bundle = assembly_lock.load(available[0])
    assembly_lock.verify(root, bundle, "adapted")
    return metadata, bundle, assembly_lock.sha(available[0].read_bytes())


def artifact_record(root, command, arch, metadata, bundle, bundle_hash):
    binary = assembly_lock.safe_path(root, "bin/" + command)
    info = build_info(root, binary, metadata, arch)
    expected = assembly_lock.ROOT_MODULE + COMMANDS[command][1:]
    if info["package"] != expected:
        raise ValueError(f"unexpected binary package: {command}")
    if arch == build_environment.architecture():
        actual = subprocess.check_output([str(binary), "--build-info"], cwd=root, text=True, timeout=30)
        if assembly_lock.encoded(json.loads(actual)) != assembly_lock.encoded(metadata):
            raise ValueError(f"embedded project metadata mismatch: {command}")
    return {"schema_version": 1, "command": command, "project": metadata,
            "target": "linux/" + arch, "sources": bundle["sources"],
            "dependency_bundle_sha256": bundle_hash,
            "builder_image": build_environment.select_image(build_environment.load(root)),
            "build_provenance_sha256": assembly_lock.sha((root / build_provenance.LOCK).read_bytes()),
            "build_environment_sha256": assembly_lock.sha((root / build_environment.LOCK).read_bytes()),
            "images_lock_sha256": assembly_lock.sha((root / image_inputs.LOCK).read_bytes()),
            "flags": FLAGS + ["-ldflags", f"-X main.version={metadata['version']}"],
            "binary_sha256": assembly_lock.sha(binary.read_bytes()), "build_info": info}


def build(root, command, arch):
    build_environment.verify(root, build_environment.load(root))
    metadata, bundle, bundle_hash = inputs(root)
    binary = assembly_lock.safe_path(root, "bin/" + command)
    record = assembly_lock.safe_path(root, "bin/" + command + ".build.json")
    binary.parent.mkdir(exist_ok=True)
    generated = assembly_lock.safe_path(root, COMMANDS[command][2:] + "/zz_build_info.go")
    source = metadata_source(metadata)
    if generated.exists():
        if generated.read_text() != source:
            raise ValueError("generated build metadata drift; use a fresh assembly")
    else:
        with generated.open("x") as output:
            output.write(source)
    # Rebuilds (including make container prerequisites) are allowed, but failed
    # attempts must not retain an earlier success record beside an old binary.
    record.unlink(missing_ok=True)
    subprocess.run(command_line(command, metadata, binary), cwd=root,
                   env=environment(root, metadata, arch), check=True, timeout=1800)
    value = artifact_record(root, command, arch, metadata, bundle, bundle_hash)
    with record.open("xb") as output:
        output.write(assembly_lock.encoded(value))
    print(f"Controlled build: {command} linux/{arch} {metadata['version']}")


def verify(root, arch):
    build_environment.verify(root, build_environment.load(root))
    metadata, bundle, bundle_hash = inputs(root)
    for command in COMMANDS:
        path = assembly_lock.safe_path(root, "bin/" + command + ".build.json")
        record = json.loads(path.read_text(), object_pairs_hook=assembly_sources.unique_object)
        actual = artifact_record(root, command, arch, metadata, bundle, bundle_hash)
        if assembly_lock.encoded(record) != assembly_lock.encoded(actual):
            raise ValueError(f"artifact record mismatch: {command}")
    print(f"Controlled artifact metadata and checksums verified: linux/{arch}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("phase", choices=("capture", "epoch", "arch", "build", "verify"))
    parser.add_argument("--arch", choices=tuple(ARCHITECTURES))
    parser.add_argument("--command", choices=tuple(COMMANDS))
    args = parser.parse_args()
    if (args.phase in ("build", "verify")) != (args.arch is not None):
        parser.error("build and verify require --arch; other phases do not accept it")
    if (args.phase == "build") != (args.command is not None):
        parser.error("only build requires --command")
    if args.phase == "capture":
        capture(ROOT)
    elif args.phase == "epoch":
        print(project(ROOT)["source_date_epoch"])
    elif args.phase == "arch":
        print(build_environment.architecture())
    elif args.phase == "build":
        build(ROOT, args.command, args.arch)
    else:
        verify(ROOT, args.arch)


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        raise SystemExit(f"Controlled build failed: {error}") from error
