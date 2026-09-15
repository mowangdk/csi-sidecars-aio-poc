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

import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import build_binaries as build
import isolated_sync
from build_provenance_test import fixture


def repository(root):
    fixture(root)
    (root / build.build_environment.LOCK).write_bytes((build.ROOT / build.build_environment.LOCK).read_bytes())
    (root / "tools/assembly/sources.lock.json").write_bytes(build.assembly_sources.DEFAULT_LOCK.read_bytes())
    (root / "tools/input.go").write_text("package fixture\n")
    subprocess.run(["git", "init", "-q", str(root)], check=True)
    subprocess.run(["git", "-C", str(root), "add", "."], check=True)
    env = {**os.environ, "GIT_AUTHOR_NAME": "Fixture", "GIT_COMMITTER_NAME": "Fixture",
           "GIT_AUTHOR_EMAIL": "fixture@localhost", "GIT_COMMITTER_EMAIL": "fixture@localhost",
           "GIT_AUTHOR_DATE": "1700000000 +0000", "GIT_COMMITTER_DATE": "1700000000 +0000"}
    subprocess.run(["git", "-C", str(root), "-c", "commit.gpgsign=false", "commit", "-qm", "fixture"], check=True, env=env)


class MetadataTests(unittest.TestCase):
    def test_project_identity_ignores_index_tags_docs_and_generation_time(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / "original"
            repository(root)
            original = build.project_metadata(root)
            self.assertFalse(original["dirty"])
            self.assertEqual(original["version"], original["revision"])
            self.assertEqual(original["source_date_epoch"], 1700000000)
            subprocess.run(["git", "-C", str(root), "tag", "v999.0.0"], check=True)
            (root / "README.md").write_text("documentation is not a binary input")
            (root / "tmp").mkdir()
            (root / "tmp/generated").write_text("unrelated timestamped history")
            self.assertEqual(original, build.project_metadata(root))
            destination = Path(directory) / "other/deeper/path"
            isolated_sync.snapshot(root, destination)
            # A no-checkout clone has an empty index. Identity uses objects/bytes.
            self.assertEqual(original, build.project_metadata(destination))
            (root / "tools/input.go").write_text("package changed\n")
            changed = build.project_metadata(root)
            self.assertTrue(changed["dirty"])
            self.assertTrue(changed["version"].startswith(original["revision"] + "-dirty."))
            (root / "tools/input.go").chmod(0o755)
            self.assertNotEqual(changed["version"], build.project_metadata(root)["version"])
            (root / "tools/new.py").write_text("# new build input\n")
            self.assertNotEqual(changed["inputs_sha256"], build.project_metadata(root)["inputs_sha256"])

    def test_capture_is_exclusive_and_rejects_metadata_input_and_source_drift(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            repository(root)
            (root / "tmp").mkdir()
            value = build.capture(root)
            self.assertEqual(value, build.project(root))
            with self.assertRaises(FileExistsError):
                build.capture(root)
            path = root / build.PROJECT
            for key, altered in (("revision", "f" * 40), ("source_date_epoch", True), ("dirty", 0), ("version", "invented")):
                changed = {**value, key: altered}
                path.write_bytes(build.assembly_lock.encoded(changed))
                with self.assertRaisesRegex(ValueError, "metadata changed"):
                    build.project(root)
            path.write_bytes(build.assembly_lock.encoded(value))
            selected = root / "tools/assembly/sources.lock.json"
            sources = json.loads(selected.read_text())
            sources["sources"]["attacher"]["commit"] = "f" * 40
            selected.write_text(json.dumps(sources))
            self.assertNotEqual(value["version"], build.project_metadata(root)["version"])
            with self.assertRaisesRegex(ValueError, "metadata changed"):
                build.project(root)

    def test_image_lock_participates_in_project_identity(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            repository(root)
            original = build.project_metadata(root)
            path = root / build.image_inputs.LOCK
            lock = json.loads(path.read_text())
            lock["runtime"]["platforms"]["linux/arm64"] = "sha256:" + "a" * 64
            path.write_text(json.dumps(lock))
            changed = build.project_metadata(root)
            self.assertTrue(changed["dirty"])
            self.assertNotEqual(original["inputs_sha256"], changed["inputs_sha256"])

    def test_generated_dockerfile_drift_fails_before_binary_inputs(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture(root)
            build.image_inputs.dockerfiles(root, generate=True)
            (root / "cmd/snapshot-controller/Dockerfile").write_text("FROM mutable:latest\n")
            with patch.object(build, "project") as project:
                with self.assertRaisesRegex(ValueError, "locked runtime template"):
                    build.inputs(root)
            project.assert_not_called()

    def test_build_environment_drops_caller_flags_and_uses_fixed_arch_levels(self):
        contaminated = {"GOFLAGS": "-race", "GOEXPERIMENT": "boringcrypto", "CGO_ENABLED": "1",
                        "GOOS": "darwin", "GOTOOLCHAIN": "auto", "GOAMD64": "v4", "GOARM64": "v9.0",
                        "GODEBUG": "cpu.all=off", "SOURCE_DATE_EPOCH": "999", "CC": "unexpected"}
        with patch.dict(os.environ, contaminated):
            for arch, (key, level) in build.ARCHITECTURES.items():
                env = build.environment(Path("/build/fixture"), {"source_date_epoch": 1700000000}, arch)
                for name, value in {"GOOS": "linux", "GOARCH": arch, key: level, "GOFLAGS": "", "GOEXPERIMENT": "",
                                    "CGO_ENABLED": "0", "GOTOOLCHAIN": "local", "GOPROXY": "off",
                                    "SOURCE_DATE_EPOCH": "1700000000", "GOWORK": "/build/fixture/go.work"}.items():
                    self.assertEqual(env[name], value)
                self.assertNotIn("GODEBUG", env)
                self.assertNotIn("CC", env)
        with self.assertRaises(ValueError):
            build.environment(Path("/build/fixture"), {}, "ppc64le")

    def test_workspace_paths_are_explicit_safe_and_validation_is_relative(self):
        for path in ("/workspace", "/build/one", "/build/another/deep/path"):
            command = isolated_sync.container_command("podman", "locked", Path("/copy"), "test", workspace=path)
            self.assertIn("/copy:" + path, command)
            self.assertEqual(command[command.index("--workdir") + 1], path)
        for path in ("/", "/usr/local/go", "relative", "/build/a/../b", "/build/a:ro", "/build/a b", "/build/$(id)"):
            with self.assertRaises(argparse.ArgumentTypeError):
                isolated_sync.workspace_path(path)
        self.assertIn('GOWORK="$PWD/go.work"', isolated_sync.FRESH_VALIDATION)
        self.assertNotIn("/workspace/", isolated_sync.FRESH_VALIDATION)

    def test_make_routes_all_commands_to_controlled_policy_and_rejects_legacy_matrix(self):
        commands = tuple(build.COMMANDS)[:3]
        goals = [[], ["all"], ["build"], ["container"], ["push"], ["push-multiarch"]]
        goals += [[prefix + name] for prefix in ("build-", "container-", "push-", "push-multiarch-")
                  for name in commands]
        for arch in ("amd64", "arm64"):
            for goal in goals:
                with self.subTest(arch=arch, goal=goal):
                    # Dry runs only: never execute inherited container/publication recipes.
                    result = subprocess.run(
                        ["make", "-n", *goal, "BUILD_ARCH=" + arch, "REV=fixture", "IMAGE_TAGS=fixture",
                         "CSI_AIO_BUILD_COMMAND=echo obsolete-hook"], cwd=build.ROOT,
                        capture_output=True, text=True, check=True, timeout=30)
                    self.assertNotIn("go build", result.stdout)
                    self.assertNotIn("obsolete-hook", result.stdout)
                    selected = [name for name in commands if goal and goal[0].endswith("-" + name)] or commands
                    for name in selected:
                        self.assertEqual(result.stdout.count(
                            "build_binaries.py build --command " + name + " --arch '" + arch + "'"), 1)
        # The exact environment gate belongs to the builder. Isolate routing here.
        result = subprocess.run(["make", "-o", "check-go-version-go", "build", "BUILD_PLATFORMS=linux amd64"],
                                cwd=build.ROOT, capture_output=True, text=True, timeout=30)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Use BUILD_ARCH", result.stdout)
        result = subprocess.run(["make", "-o", "check-go-version-go", "container-csi-sidecars", "BUILD_ARCH=arm64", "CMDS_DIR=other"],
                                cwd=build.ROOT, capture_output=True, text=True, timeout=30)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("CMDS_DIR must remain cmd", result.stdout)
        self.assertNotIn("docker build", result.stdout)
        script = Path(__file__).with_name("sync.sh").read_text()
        self.assertLess(script.index("build_binaries.py capture"), script.index("git merge"))
        self.assertIn('GIT_COMMITTER_DATE="${SOURCE_DATE_EPOCH} +0000"', script)
        self.assertIn('build --command csi-attacher --arch "${BUILD_ARCH}"', script)
        self.assertNotIn("main.version=foo", script)

    def test_root_recipes_execute_builder_and_propagate_failure_without_subtree_hook(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "release-tools").mkdir()
            for name in ("Makefile", "release-tools/build.make"):
                shutil.copy2(build.ROOT / name, root / name)
            inherited = (root / "release-tools/build.make").read_bytes()
            self.assertNotIn(b"CSI_AIO_BUILD_COMMAND", inherited)
            scripts = root / "tools/scripts"
            scripts.mkdir(parents=True)
            (scripts / "build_binaries.py").write_text(
                'import json, os, sys\n'
                'print("fixture build " + json.dumps(sys.argv[1:]))\n'
                'sys.exit(int(os.environ.get("FIXTURE_BUILD_EXIT", "0")))\n')
            # Even a same-named file must not suppress the controlled build.
            (root / "build-csi-sidecars").touch()
            command = ["make", "-o", "check-go-version-go"]
            docker = scripts / "docker"
            docker.write_text('#!/bin/sh\necho "unexpected fixture Docker invocation" >&2\nexit 97\n')
            docker.chmod(0o755)
            env = {**os.environ, "FIXTURE_BUILD_EXIT": "0", "PATH": str(scripts) + os.pathsep + os.environ["PATH"]}
            result = subprocess.run(command + ["-j3", "build", "BUILD_ARCH=arm64"], cwd=root,
                                    env=env, capture_output=True, text=True, check=True, timeout=30)
            calls = [json.loads(line.removeprefix("fixture build ")) for line in result.stdout.splitlines()
                     if line.startswith("fixture build ")]
            self.assertCountEqual(calls, [["build", "--command", name, "--arch", "arm64"]
                                          for name in tuple(build.COMMANDS)[:3]])
            # The inherited container dependency must stop before Docker after failure.
            result = subprocess.run(command + ["container-csi-sidecars", "BUILD_ARCH=arm64", "REV=fixture"],
                                    cwd=root, env={**env, "FIXTURE_BUILD_EXIT": "23"},
                                    capture_output=True, text=True, timeout=30)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("Error 23", result.stderr)
            self.assertNotIn("docker build", result.stdout)
            self.assertNotIn("unexpected fixture Docker invocation", result.stderr)
            self.assertEqual((root / "release-tools/build.make").read_bytes(), inherited)

    def test_verification_requires_every_exact_artifact_record(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "bin").mkdir()
            record = {"binary_sha256": "a" * 64, "project": {"dirty": True}}
            for command in build.COMMANDS:
                (root / "bin" / (command + ".build.json")).write_bytes(build.assembly_lock.encoded(record))
            with patch.object(build.build_environment, "load"), patch.object(build.build_environment, "verify"), patch.object(
                build, "inputs", return_value=({}, {}, "b" * 64)
            ), patch.object(build, "artifact_record", return_value=record) as actual:
                build.verify(root, "arm64")
                self.assertEqual(actual.call_count, 4)
                path = root / "bin/snapshot-conversion-webhook.build.json"
                path.write_text('{}')
                with self.assertRaisesRegex(ValueError, "record mismatch"):
                    build.verify(root, "arm64")
                path.unlink()
                with self.assertRaises(FileNotFoundError):
                    build.verify(root, "arm64")

    def test_failed_rebuild_invalidates_record_and_preserves_unrecognized_metadata(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "cmd/csi-sidecars").mkdir(parents=True)
            metadata = {"version": "fixture", "source_date_epoch": 1700000000}
            (root / "bin").mkdir()
            record = root / "bin/csi-sidecars.build.json"
            record.write_text("old success")
            with patch.object(build.build_environment, "load"), patch.object(build.build_environment, "verify"), patch.object(
                build, "inputs", return_value=(metadata, {}, "a" * 64)
            ), patch.object(build.subprocess, "run", side_effect=subprocess.CalledProcessError(1, "go")):
                with self.assertRaises(subprocess.CalledProcessError):
                    build.build(root, "csi-sidecars", "arm64")
            self.assertFalse((root / "bin/csi-sidecars.build.json").exists())
            (root / "bin/csi-sidecars").write_bytes(b"retained")
            (root / "cmd/csi-sidecars/zz_build_info.go").write_text("unrecognized input")
            with patch.object(build.build_environment, "load"), patch.object(build.build_environment, "verify"), patch.object(
                build, "inputs", return_value=(metadata, {}, "a" * 64)
            ), patch.object(build.subprocess, "run") as run:
                with self.assertRaisesRegex(ValueError, "metadata drift"):
                    build.build(root, "csi-sidecars", "arm64")
            run.assert_not_called()
            self.assertEqual((root / "bin/csi-sidecars").read_bytes(), b"retained")


@unittest.skipUnless(shutil.which("go") and os.uname().sysname == "Linux", "Linux Go required")
class RealBuildTests(unittest.TestCase):
    def test_identical_binaries_across_paths_with_actual_elf_and_embedded_identity(self):
        metadata = {"schema_version": 1, "revision": "a" * 40, "version": "a" * 40 + "-dirty.fixture",
                    "source_date_epoch": 1700000000, "dirty": True,
                    "inputs_sha256": "b" * 64, "source_lock_sha256": "c" * 64}
        with tempfile.TemporaryDirectory() as directory:
            roots = [Path(directory) / "first", Path(directory) / "different/deep/second"]
            for root in roots:
                (root / "cmd/csi-sidecars").mkdir(parents=True)
                (root / "go.mod").write_text("module " + build.assembly_lock.ROOT_MODULE + "\ngo 1.26.0\n")
                (root / "go.work").write_text("go 1.26.0\nuse .\n")
                (root / "cmd/csi-sidecars/main.go").write_text(
                    'package main\nimport ("fmt"; "runtime")\nvar version = "unknown"\n'
                    'func main() { _, path, _, _ := runtime.Caller(0); fmt.Println(version, path) }\n')
                source = build.metadata_source(metadata)
                self.assertEqual(subprocess.check_output(["gofmt"], input=source, text=True), source)
                (root / "cmd/csi-sidecars/zz_build_info.go").write_text(source)
                (root / build.build_environment.LOCK).parent.mkdir(parents=True)
                (root / build.build_environment.LOCK).write_bytes((build.ROOT / build.build_environment.LOCK).read_bytes())
            for arch in build.ARCHITECTURES:
                binaries = []
                for root in roots:
                    binary = root / arch
                    subprocess.run(build.command_line("csi-sidecars", metadata, binary), cwd=root,
                                   env=build.environment(root, metadata, arch), check=True, timeout=180)
                    info = build.build_info(root, binary, metadata, arch)
                    self.assertEqual(info["package"], build.assembly_lock.ROOT_MODULE + "/cmd/csi-sidecars")
                    binaries.append(binary.read_bytes())
                    if arch == build.build_environment.architecture():
                        identity = subprocess.check_output([str(binary), "--build-info"], text=True, timeout=30)
                        self.assertEqual(json.loads(identity), metadata)
                        output = subprocess.check_output([str(binary)], text=True, timeout=30)
                        self.assertIn(metadata["version"], output)
                        self.assertNotIn(str(root), output)
                self.assertEqual(binaries[0], binaries[1])
                with self.assertRaisesRegex(ValueError, "Go binary metadata"):
                    build.build_info(roots[0], roots[0] / arch, metadata, "arm64" if arch == "amd64" else "amd64")
                (roots[0] / arch).write_bytes(b"not ELF")
                with self.assertRaises(subprocess.CalledProcessError):
                    build.build_info(roots[0], roots[0] / arch, metadata, arch)


if __name__ == "__main__":
    unittest.main()
