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

import copy
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import build_provenance as provenance
import isolated_sync
from image_inputs_test import fixture as image_fixture


def fixture(root):
    """Small independent build inventory, shared by orchestration fixtures."""
    root.mkdir(parents=True, exist_ok=True)
    for name in provenance.ROOT_FILES:
        (root / name).write_text(provenance.CMDS + "\n" if name == "Makefile" else "fixture\n")
    image_fixture(root)
    tree = root / "release-tools"
    tree.mkdir(exist_ok=True)
    (tree / "build.make").write_text("fixture build rules\n")
    (tree / "LICENSE").write_text("fixture license\n")
    (tree / "OWNERS_ALIASES").symlink_to("LICENSE")
    lock = {"schema_version": 1,
            "upstream": {"repository": provenance.REPOSITORY, "commit": "1" * 40, "tree": "2" * 40,
                         "project_import_commit": "3" * 40, "files": provenance.local_files(root)},
            "local_changes": {}, "root_files": provenance.root_files(root)}
    path = root / provenance.LOCK
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(provenance.encoded(lock))
    return lock


class BuildProvenanceTests(unittest.TestCase):
    def test_current_lock_matches_preserved_tree(self):
        lock = provenance.verify(provenance.ROOT)
        self.assertEqual(set(lock["local_changes"]), {"build.make", "prow.sh"})
        self.assertEqual(lock["upstream"]["tree"], "74aa8aff92eea2c4889162eeee65341d34e7688d")

    def test_schema_and_inventory_reject_ambiguous_or_unsafe_entries(self):
        with tempfile.TemporaryDirectory() as directory:
            original = fixture(Path(directory))
            changes = (
                lambda x: x.update(schema_version=True),
                lambda x: x.update(extra=1),
                lambda x: x["upstream"].update(commit="master"),
                lambda x: x["upstream"].update(repository="https://example.org/fork"),
                lambda x: x["root_files"].pop("Dockerfile"),
                lambda x: x["upstream"]["files"].update({"../escape": provenance.entry("100644", b"x")}),
                lambda x: x["upstream"]["files"]["LICENSE"].update(mode="160000"),
                lambda x: x["upstream"]["files"]["LICENSE"].update(sha256="bad"),
                lambda x: x["upstream"]["files"].update({"LICENSE/child": provenance.entry("100644", b"x")}),
                lambda x: x["local_changes"].update({"build.make": {"entry": None, "reason": ""}}),
                lambda x: x["local_changes"].update({"missing": {"entry": None, "reason": "not a change"}}),
                lambda x: x["local_changes"].update({"build.make": {"entry": False, "reason": "bad entry"}}),
            )
            for change in changes:
                altered = copy.deepcopy(original)
                change(altered)
                with self.subTest(lock=altered), self.assertRaises(ValueError):
                    provenance.validate(altered)
            path = Path(directory) / provenance.LOCK
            path.write_text('{"schema_version":1,"schema_version":1}')
            with self.assertRaisesRegex(ValueError, "duplicate"):
                provenance.load(Path(directory))

    def test_bytes_modes_additions_deletions_and_root_drift_fail_closed(self):
        for name in (*provenance.ROOT_FILES, "release-tools/build.make", "release-tools/LICENSE"):
            for kind in ("bytes", "mode", "delete"):
                with self.subTest(name=name, kind=kind), tempfile.TemporaryDirectory() as directory:
                    root = Path(directory)
                    fixture(root)
                    path = root / name
                    if kind == "bytes":
                        with path.open("a") as output:
                            output.write("changed\n")
                    elif kind == "mode":
                        path.chmod(0o755)
                    else:
                        path.unlink()
                    with self.assertRaises((ValueError, OSError)):
                        provenance.verify(root)
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture(root)
            (root / "release-tools/.ignored-input").write_text("unexpected")
            with self.assertRaisesRegex(ValueError, "ignored-input"):
                provenance.verify(root)

    def test_makefile_overrides_and_root_links_are_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture(root)
            (root / "GNUmakefile").write_text("unlocked build rules")
            with self.assertRaisesRegex(ValueError, "Makefile override"):
                provenance.verify(root)
            (root / "GNUmakefile").unlink()
            (root / "Dockerfile").unlink()
            (root / "Dockerfile").symlink_to("Makefile")
            with self.assertRaisesRegex(ValueError, "root build files"):
                provenance.verify(root)

    def test_link_targets_directory_links_and_special_files_rejected(self):
        for target in ("/outside", "../outside", "missing", "OWNERS_ALIASES", "LICENSE/child"):
            with self.subTest(target=target), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                fixture(root)
                path = root / "release-tools/OWNERS_ALIASES"
                path.unlink()
                path.symlink_to(target)
                with self.assertRaises(ValueError):
                    provenance.verify(root)
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture(root)
            (root / "release-tools/directory").mkdir()
            link = root / "release-tools/alias"
            link.symlink_to("directory", target_is_directory=True)
            with self.assertRaisesRegex(ValueError, "link"):
                provenance.verify(root)
            link.unlink()
            os.mkfifo(link)
            with self.assertRaisesRegex(ValueError, "regular"):
                provenance.verify(root)
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture(root)
            path = root / provenance.LOCK
            path.rename(path.with_suffix(".saved"))
            path.symlink_to(path.with_suffix(".saved").name)
            with self.assertRaisesRegex(ValueError, "symlink"):
                provenance.load(root)

    def test_intentional_changes_need_exact_inventory_and_change_descriptions(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            lock = fixture(root)
            (root / "release-tools/build.make").write_text("local patch\n")
            (root / "release-tools/new.sh").write_text("local addition\n")
            (root / "release-tools/OWNERS_ALIASES").unlink()
            for name in ("build.make", "new.sh", "OWNERS_ALIASES"):
                lock["local_changes"][name] = {"entry": provenance.local_files(root).get(name), "reason": "fixture change"}
            (root / provenance.LOCK).write_bytes(provenance.encoded(provenance.validate(lock)))
            self.assertEqual(provenance.verify(root), lock)
            (root / "release-tools/new.sh").write_text("unrecorded patch")
            with self.assertRaisesRegex(ValueError, "new.sh"):
                provenance.verify(root)

    def test_exact_git_objects_and_project_import_are_verified_without_checkout(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture(root)
            env = dict(os.environ, GIT_AUTHOR_NAME="Fixture", GIT_COMMITTER_NAME="Fixture",
                       GIT_AUTHOR_EMAIL="fixture@localhost", GIT_COMMITTER_EMAIL="fixture@localhost")

            def git(*args, data=None):
                return subprocess.check_output(["git", "-C", str(root), *args], input=data,
                                               env=env, timeout=30).decode().strip()

            git("init")
            records = []
            for name, item in sorted(provenance.local_files(root).items()):
                data = item["target"].encode() if item["mode"] == "120000" else (root / "release-tools" / name).read_bytes()
                blob = git("hash-object", "-w", "--stdin", data=data)
                records.append(f"{item['mode']} blob {blob}\t{name}\n")
            tree = git("mktree", data="".join(records).encode())
            upstream = git("commit-tree", tree, data=b"fixture upstream\n")
            project_tree = git("mktree", data=f"040000 tree {tree}\trelease-tools\n".encode())
            imported = git("commit-tree", project_tree, data=b"fixture import\n")
            (root / "release-tools/build.make").write_text("local patch")
            with self.assertRaisesRegex(ValueError, "explicit reasons"):
                provenance.candidate(root, root, upstream, imported, {})
            lock = provenance.candidate(root, root, upstream, imported, {"build.make": "fixture patch"})
            self.assertEqual(lock["upstream"]["tree"], tree)
            provenance.verify_upstream(root, lock, root)
            lock["upstream"]["files"]["build.make"]["sha256"] = "a" * 64
            with self.assertRaisesRegex(ValueError, "Git objects"):
                provenance.verify_upstream(root, lock, root)
            with self.assertRaisesRegex(ValueError, "project import"):
                provenance.candidate(root, root, upstream, upstream, {"build.make": "fixture patch"})
            with self.assertRaises((ValueError, subprocess.CalledProcessError)):
                provenance.upstream_files(root, "9" * 40)

    def test_host_drift_is_rejected_before_snapshot_or_container(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture(root)
            path = root / isolated_sync.build_environment.LOCK
            path.write_bytes((provenance.ROOT / isolated_sync.build_environment.LOCK).read_bytes())
            (root / "release-tools/new.sh").write_text("unrecorded")
            with patch.object(isolated_sync, "ROOT", root), patch.object(isolated_sync, "snapshot") as snapshot, patch(
                "sys.argv", ["isolated_sync", "--tooling-only"]
            ), patch.object(isolated_sync.subprocess, "run") as run:
                with self.assertRaisesRegex(ValueError, "new.sh"):
                    isolated_sync.main()
            snapshot.assert_not_called()
            run.assert_not_called()

    def test_snapshot_drift_is_rejected_before_container_execution(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture(root)
            (root / isolated_sync.build_environment.LOCK).write_bytes(
                (provenance.ROOT / isolated_sync.build_environment.LOCK).read_bytes())

            def snapshot(_, destination):
                fixture(destination)
                (destination / "release-tools/build.make").write_text("changed during copy")

            with patch.object(isolated_sync, "ROOT", root), patch.object(isolated_sync, "snapshot", side_effect=snapshot), patch(
                "sys.argv", ["isolated_sync", "--tooling-only"]
            ), patch.object(isolated_sync.subprocess, "run") as run:
                with self.assertRaisesRegex(ValueError, "provenance mismatch"):
                    isolated_sync.main()
            run.assert_not_called()

    def test_sync_update_rejects_drift_before_bootstrap_and_never_rewrites_makefile(self):
        script = Path(__file__).with_name("sync.sh").read_text()
        self.assertNotIn("CMDS_VALUE", script)
        self.assertLess(script.index("build_provenance.py verify"), script.index("build_environment.py bootstrap"))
        if os.uname().sysname != "Linux":
            self.skipTest("Linux sync preflight required")
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture(root)
            scripts = root / "tools/scripts"
            scripts.mkdir()
            for name in ("sync.sh", "assembly_sources.py", "build_provenance.py", "retry-go-dependencies.sh"):
                shutil.copy2(Path(__file__).with_name(name), scripts / name)
            (root / "tools/assembly/sources.lock.json").write_bytes(isolated_sync.assembly_sources.DEFAULT_LOCK.read_bytes())
            with (root / "Makefile").open("a") as output:
                output.write("# unrecorded edit\n")
            original = (root / "Makefile").read_bytes()
            result = subprocess.run(["bash", str(scripts / "sync.sh"), "--update-dependencies", "1.36.3"],
                                    capture_output=True, text=True, timeout=30,
                                    env={**os.environ, "NORMALIZED_LOGGING": "true", "SKIP_SANITY_CHECK": "false"})
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("root build provenance mismatch", result.stderr)
            self.assertEqual((root / "Makefile").read_bytes(), original)
            self.assertFalse((root / ".assembly-env").exists())
            self.assertFalse((root / "tmp").exists())


if __name__ == "__main__":
    unittest.main()
