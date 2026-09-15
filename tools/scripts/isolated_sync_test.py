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

import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import isolated_sync
import build_environment
from build_provenance_test import fixture as provenance_fixture

BUILD_LOCK = (build_environment.ROOT / build_environment.LOCK).read_bytes()
BUILD_IMAGE = build_environment.select_image(build_environment.load())


class IsolatedSnapshotTests(unittest.TestCase):
    def test_snapshot_preserves_working_edits_and_excludes_user_files(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / "root"
            root.mkdir()
            (root / "tools").mkdir()
            (root / "tools/new.py").write_text("new maintained code")
            (root / "tracked name").write_text("working tree edit")
            (root / ".claude").mkdir()
            (root / ".claude/private").write_text("not an assembly input")
            (root / "alias").symlink_to("tracked name")
            (root / "release-tools").mkdir()
            (root / "release-tools/new.make").write_text("untracked inherited input")
            destination = Path(directory) / "copy"
            with patch.object(isolated_sync.subprocess, "run") as clone, patch.object(
                isolated_sync.subprocess, "check_output", side_effect=[
                    b"tracked name\0alias\0deleted\0", b"tools/new.py\0"]
            ):
                isolated_sync.snapshot(root, destination)
            command = clone.call_args.args[0]
            self.assertIn("--no-hardlinks", command)
            self.assertIn("--no-checkout", command)
            self.assertEqual((destination / "tracked name").read_text(), "working tree edit")
            self.assertEqual((destination / "tools/new.py").read_text(), "new maintained code")
            self.assertEqual((destination / "release-tools/new.make").read_text(), "untracked inherited input")
            self.assertEqual(os.readlink(destination / "alias"), "tracked name")
            self.assertFalse((destination / ".claude").exists())
            self.assertFalse((destination / "deleted").exists())
            self.assertEqual((root / ".claude/private").read_text(), "not an assembly input")

    def test_snapshot_rejects_external_symlinks(self):
        for target in ("../outside", "/tmp/outside"):
            with self.subTest(target=target), tempfile.TemporaryDirectory() as directory:
                root = Path(directory) / "root"
                root.mkdir()
                (root / "alias").symlink_to(target)
                with patch.object(isolated_sync.subprocess, "run"), patch.object(
                    isolated_sync.subprocess, "check_output", side_effect=[b"alias\0", b""]
                ):
                    with self.assertRaisesRegex(ValueError, "escapes snapshot"):
                        isolated_sync.snapshot(root, Path(directory) / "copy")

    def test_runtime_snapshot_freezes_inputs_and_links_current_tools(self):
        with tempfile.TemporaryDirectory() as directory:
            baseline = Path(directory) / "baseline"
            checkout = Path(directory) / "checkout"
            for tree in ("cmd", "pkg", "staging", "vendor"):
                (baseline / tree).mkdir(parents=True)
                (baseline / tree / "input.go").write_text("baseline")
            (baseline / "go.mod").write_text("module fixture")
            tool = checkout / "tools/pkg/runtime/supervisor.go"
            tool.parent.mkdir(parents=True)
            tool.write_text("maintained")
            commands = isolated_sync.runtime_snapshot(baseline, checkout)
            (baseline / "pkg/input.go").write_text("later baseline change")
            self.assertEqual((checkout / "pkg/input.go").read_text(), "baseline")
            self.assertEqual((checkout / "go.mod").read_text(), "module fixture")
            link = checkout / "pkg/runtime/supervisor.go"
            self.assertEqual(link.read_text(), "maintained")
            self.assertFalse(Path(os.readlink(link)).is_absolute())
            self.assertEqual(link.resolve(), tool.resolve())
            self.assertEqual(commands.count("/tmp/lifecycle-adapter "), 7)
            self.assertIn("--workers .", commands)
            self.assertIn("--dependencies .", commands)
            self.assertIn("go test -race", commands)
            self.assertNotIn("sync.sh", commands)

    def test_container_replay_has_no_external_network_or_host_credentials(self):
        for engine in ("podman", "docker"):
            command = isolated_sync.container_command(
                engine, "preloaded", Path("/isolated/source"), "tests", offline=True)
            self.assertEqual(command[0], engine)
            self.assertIn("--network=none", command)
            self.assertIn("--pull=never", command)
            self.assertIn("GOTOOLCHAIN=local", command)
            self.assertIn("CSI_AIO_BUILDER_IMAGE=preloaded", command)
            self.assertEqual(command.count("--volume"), 1)
            self.assertIn("/isolated/source:/workspace", command)
            self.assertEqual(command[-4:], ["preloaded", "bash", "-c", "tests"])
        online = isolated_sync.container_command("podman", "preloaded", Path("/copy"), "sync")
        self.assertNotIn("--network=none", online)
        with patch.object(isolated_sync.os, "uname") as uname:
            uname.return_value.sysname = "Linux"
            command = isolated_sync.container_command("docker", "preloaded", Path("/copy"), "sync")
        self.assertIn(f"{os.getuid()}:{os.getgid()}", command)
        self.assertIn("HOME=/tmp", command)
        self.assertIn("GOCACHE=/tmp/go-build", command)

    def test_unlocked_builder_rejected_before_snapshot_or_source_resolution(self):
        with patch.object(isolated_sync, "snapshot") as snapshot, patch.object(
            isolated_sync.assembly_sources, "resolve_candidate"
        ) as resolve, patch("sys.argv", ["isolated_sync", "--image", "mutable:latest"]):
            with self.assertRaisesRegex(ValueError, "unlocked builder"):
                isolated_sync.main()
        snapshot.assert_not_called()
        resolve.assert_not_called()

    def test_tooling_mode_defaults_to_locked_image_without_assembly(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            provenance_fixture(root)
            path = root / build_environment.LOCK
            path.write_bytes(BUILD_LOCK)
            with patch.object(isolated_sync, "ROOT", root), patch.object(
                isolated_sync, "snapshot", side_effect=lambda _, destination: provenance_fixture(destination)
            ), patch.object(isolated_sync.subprocess, "run", return_value=subprocess.CompletedProcess([], 0)) as run, patch(
                "sys.argv", ["isolated_sync", "--tooling-only"]
            ):
                self.assertEqual(isolated_sync.main(), 0)
            command = run.call_args.args[0]
            self.assertIn(BUILD_IMAGE, command)
            self.assertIn("build_environment.py bootstrap", command[-1])
            self.assertLess(command[-1].index("build_provenance.py verify"), command[-1].index("build_environment.py bootstrap"))
            self.assertIn("unittest discover", command[-1])
            self.assertIn("verify-boilerplate.sh", command[-1])
            self.assertNotIn("sync.sh", command[-1])

    def test_external_runtime_baseline_is_rejected_before_clone(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(
            isolated_sync, "ROOT", Path(directory)
        ), patch("sys.argv", ["isolated_sync", "--image", "preloaded",
                              "--runtime-baseline", "/outside"]), patch.object(
            isolated_sync, "snapshot"
        ) as snapshot, patch.object(isolated_sync.subprocess, "run") as run:
            with self.assertRaises(SystemExit) as raised:
                isolated_sync.main()
            self.assertEqual(raised.exception.code, 2)
            snapshot.assert_not_called()
            run.assert_not_called()


class SourceCandidateTests(unittest.TestCase):
    def test_export_is_atomic_and_never_overwrites(self):
        data = isolated_sync.assembly_sources.DEFAULT_LOCK.read_bytes()
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "candidate.json"
            isolated_sync.export_candidate(data, output)
            self.assertEqual(output.read_bytes(), data)
            with self.assertRaises(FileExistsError):
                isolated_sync.export_candidate(data, output)
            self.assertEqual(list(Path(directory).iterdir()), [output])
            output.unlink()
            output.symlink_to("missing")
            with self.assertRaises(FileExistsError):
                isolated_sync.export_candidate(data, output)
            self.assertTrue(output.is_symlink())
            self.assertFalse((output.parent / "missing").exists())

    def test_update_exports_only_after_full_validation_and_keeps_active_lock(self):
        active = isolated_sync.assembly_sources.DEFAULT_LOCK.read_bytes()
        resolved = isolated_sync.assembly_sources.load(isolated_sync.assembly_sources.DEFAULT_LOCK)
        resolved["sources"]["attacher"]["commit"] = "b" * 40
        for status, explicit in ((0, False), (23, False), (0, True), (23, True)):
            with self.subTest(status=status, explicit=explicit), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                provenance_fixture(root)
                active_path = root / "tools/assembly/sources.lock.json"
                active_path.write_bytes(active)
                (root / build_environment.LOCK).write_bytes(BUILD_LOCK)
                output = root / ".work/candidate.json"
                selected = root / "selected.json"
                selected.write_bytes(isolated_sync.assembly_sources.encoded(resolved))
                selection_args = ["--source-lock", str(selected)] if explicit else ["--update-sources"]

                def snapshot(_, destination):
                    provenance_fixture(destination)
                    path = destination / "tools/assembly/sources.lock.json"
                    path.write_bytes(active)

                with patch.object(isolated_sync, "ROOT", root), patch.object(
                    isolated_sync, "snapshot", side_effect=snapshot
                ), patch.object(isolated_sync.assembly_sources, "resolve_candidate", return_value=resolved) as resolve, patch.object(
                    isolated_sync.subprocess, "run", return_value=subprocess.CompletedProcess([], status)
                ) as run, patch("sys.argv", ["isolated_sync", "--image", BUILD_IMAGE, *selection_args,
                                            "--candidate-output", str(output)]):
                    self.assertEqual(isolated_sync.main(), status)
                self.assertEqual(resolve.call_count, 0 if explicit else 1)
                self.assertEqual(selected.read_bytes(), isolated_sync.assembly_sources.encoded(resolved))
                self.assertEqual(active_path.read_bytes(), active)
                self.assertEqual(output.exists(), status == 0)
                script = run.call_args.args[0][-1]
                for required in ("./tools/scripts/sync.sh &&", "assembly_dependencies.py vendor &&",
                                 "go test -race -count=1 -timeout=10m", "go vet", "verify_artifacts.py cli"):
                    self.assertIn(required, script)
                if status == 0:
                    self.assertEqual(output.read_bytes(), isolated_sync.assembly_sources.encoded(resolved))

    def test_invalid_candidate_destination_fails_before_snapshot(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for extra in (["--update-sources"], ["--source-lock", "missing.json"],
                          ["--update-sources", "--source-lock", "missing.json"],
                          ["--candidate-output", str(root / ".work/new")],
                          ["--update-sources", "--candidate-output", str(root / "tools/lock")]):
                with patch.object(isolated_sync, "ROOT", root), patch.object(
                    isolated_sync, "snapshot"
                ) as snapshot, patch("sys.argv", ["isolated_sync", "--image", "preloaded", *extra]):
                    with self.assertRaises(SystemExit) as raised:
                        isolated_sync.main()
                    self.assertEqual(raised.exception.code, 2)
                    snapshot.assert_not_called()

    def test_malformed_explicit_selection_fails_before_snapshot(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            selected = root / "bad.json"
            selected.write_text('{}')
            with patch.object(isolated_sync, "snapshot") as snapshot, patch(
                "sys.argv", ["isolated_sync", "--image", "preloaded", "--source-lock", str(selected),
                             "--candidate-output", str(root / ".work/output.json")]
            ), self.assertRaises(ValueError):
                isolated_sync.main()
            snapshot.assert_not_called()


if __name__ == "__main__":
    unittest.main()
