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

    def test_snapshot_excludes_committed_generated_tree(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / "root"
            root.mkdir()
            (root / "pkg").mkdir()
            (root / "pkg/x.go").write_text("generated upstream copy")
            (root / "go.mod").write_text("module example.org/assembly\n")
            (root / "kept.go").write_text("maintained")
            destination = Path(directory) / "copy"
            with patch.object(isolated_sync.subprocess, "run"), patch.object(
                isolated_sync.subprocess, "check_output",
                side_effect=[b"pkg/x.go\0go.mod\0go.work\0kept.go\0", b""]
            ):
                isolated_sync.snapshot(root, destination)
            self.assertEqual((destination / "kept.go").read_text(), "maintained")
            for name in ("pkg", "go.mod", "go.work"):
                self.assertFalse((destination / name).exists(), name)

    def test_container_command_is_locked_and_isolated(self):
        for engine in ("podman", "docker"):
            command = isolated_sync.container_command(
                engine, "preloaded", Path("/isolated/source"), "tests")
            self.assertEqual(command[0], engine)
            self.assertIn("--rm", command)
            self.assertIn("--pull=never", command)
            self.assertIn("--cap-drop=ALL", command)
            self.assertIn("GOTOOLCHAIN=local", command)
            self.assertIn("CSI_AIO_BUILDER_IMAGE=preloaded", command)
            self.assertEqual(command.count("--volume"), 1)
            self.assertIn("/isolated/source:/workspace", command)
            self.assertEqual(command[-4:], ["preloaded", "bash", "-c", "tests"])
        with patch.object(isolated_sync.os, "uname") as uname:
            uname.return_value.sysname = "Linux"
            command = isolated_sync.container_command("docker", "preloaded", Path("/copy"), "sync")
        self.assertIn(f"{os.getuid()}:{os.getgid()}", command)
        self.assertIn("HOME=/tmp", command)
        self.assertIn("GOCACHE=/tmp/go-build", command)


class MainModeTests(unittest.TestCase):
    def _root_with_lock(self, directory):
        root = Path(directory)
        (root / "tools/assembly").mkdir(parents=True)
        (root / build_environment.LOCK).write_bytes(BUILD_LOCK)
        return root

    def test_requires_exactly_one_mode(self):
        for argv in (["isolated_sync"],
                     ["isolated_sync", "--tooling-only", "--update-dependencies", "1.36.3"]):
            with self.subTest(argv=argv), patch("sys.argv", argv):
                with self.assertRaises(SystemExit) as raised:
                    isolated_sync.main()
                self.assertEqual(raised.exception.code, 2)

    def test_update_dependencies_must_be_a_release(self):
        with patch("sys.argv", ["isolated_sync", "--update-dependencies", "master"]):
            with self.assertRaises(SystemExit) as raised:
                isolated_sync.main()
            self.assertEqual(raised.exception.code, 2)

    def test_unlocked_builder_rejected_before_snapshot(self):
        with tempfile.TemporaryDirectory() as directory:
            root = self._root_with_lock(directory)
            with patch.object(isolated_sync, "ROOT", root), patch.object(
                isolated_sync, "snapshot") as snapshot, patch(
                "sys.argv", ["isolated_sync", "--image", "mutable:latest",
                             "--update-dependencies", "1.36.3"]):
                with self.assertRaisesRegex(ValueError, "unlocked builder"):
                    isolated_sync.main()
            snapshot.assert_not_called()

    def test_tooling_mode_runs_checks_without_sync(self):
        with tempfile.TemporaryDirectory() as directory:
            root = self._root_with_lock(directory)
            with patch.object(isolated_sync, "ROOT", root), patch.object(
                isolated_sync, "snapshot"), patch.object(
                isolated_sync.subprocess, "run",
                return_value=subprocess.CompletedProcess([], 0)) as run, patch(
                "sys.argv", ["isolated_sync", "--tooling-only"]):
                self.assertEqual(isolated_sync.main(), 0)
            command = run.call_args.args[0]
            self.assertIn(BUILD_IMAGE, command)
            self.assertIn("build_environment.py bootstrap", command[-1])
            self.assertIn("unittest discover", command[-1])
            self.assertIn("verify-boilerplate.sh", command[-1])
            self.assertNotIn("sync.sh", command[-1])

    def test_assembly_mode_runs_sync_with_selected_release(self):
        with tempfile.TemporaryDirectory() as directory:
            root = self._root_with_lock(directory)
            with patch.object(isolated_sync, "ROOT", root), patch.object(
                isolated_sync, "snapshot"), patch.object(
                isolated_sync.subprocess, "run",
                return_value=subprocess.CompletedProcess([], 0)) as run, patch(
                "sys.argv", ["isolated_sync", "--update-dependencies", "1.36.3"]):
                self.assertEqual(isolated_sync.main(), 0)
            command = run.call_args.args[0]
            self.assertIn(BUILD_IMAGE, command)
            self.assertIn("./tools/scripts/sync.sh --update-dependencies 1.36.3", command[-1])

    def test_in_place_assembly_skips_snapshot_and_streams_output(self):
        with tempfile.TemporaryDirectory() as directory:
            root = self._root_with_lock(directory)
            with patch.object(isolated_sync, "ROOT", root), patch.object(
                isolated_sync, "snapshot") as snapshot, patch.object(
                isolated_sync.subprocess, "run",
                return_value=subprocess.CompletedProcess([], 0)) as run, patch(
                "sys.argv", ["isolated_sync", "--in-place",
                             "--update-dependencies", "1.36.3"]):
                self.assertEqual(isolated_sync.main(), 0)
            snapshot.assert_not_called()
            command = run.call_args.args[0]
            self.assertIn(BUILD_IMAGE, command)
            self.assertIn(f"{root}:/workspace", command)
            self.assertIn("./tools/scripts/sync.sh --update-dependencies 1.36.3", command[-1])
            self.assertNotIn("stdout", run.call_args.kwargs)
            self.assertFalse((root / ".work").exists())


if __name__ == "__main__":
    unittest.main()
