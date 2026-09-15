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

import assembly_lock as lock
import isolated_sync
import build_environment
from build_provenance_test import fixture as provenance_fixture

BUILD_LOCK = (build_environment.ROOT / build_environment.LOCK).read_bytes()
BUILD_IMAGE = build_environment.select_image(build_environment.load())


def fixture(root):
    provenance_fixture(root)
    for name in ("tmp", "tools/scripts", lock.LIB, "vendor", "tools/assembly"):
        (root / name).mkdir(parents=True, exist_ok=True)
    active = lock.sources.DEFAULT_LOCK.read_bytes()
    (root / "tmp/sources.lock.json").write_bytes(active)
    (root / "tools/assembly/sources.lock.json").write_bytes(active)
    (root / build_environment.LOCK).write_bytes(BUILD_LOCK)
    (root / "tools/scripts/generate.py").write_text("# maintained input\n")
    for name in lock.MANIFESTS:
        if name != "go.work.sum":
            (root / name).write_text("fixture " + name + "\n")
    (root / "vendor/modules.txt").write_text("# fixture inventory\n")
    (root / "vendor/input.go").write_text("package fixture\n")
    modules = [{"Path": "k8s.io/api", "Version": "v0.36.3"}]
    value = {"schema_version": 1, "sources": lock.sources.load(root / "tmp/sources.lock.json"),
             "inputs_sha256": lock.inputs_hash(root), "go_version": "go1.26.3",
             "manifests": lock.manifests(root), "modules": modules,
             "vendor_sha256": lock.vendor_hash(root), "adapted_vendor_sha256": lock.vendor_hash(root)}
    return seal(value)


def seal(value):
    value.pop("payload_sha256", None)
    value["payload_sha256"] = lock.sha(lock.encoded(value))
    return value


class CanonicalLockTests(unittest.TestCase):
    def test_bundle_rejects_corruption_unknown_fields_and_duplicate_keys(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            value = fixture(root)
            lock.validate(value)
            for change in (lambda b: b.update(extra=True),
                           lambda b: b.update(schema_version=True),
                           lambda b: b["manifests"]["go.sum"].update(content="tampered"),
                           lambda b: b["manifests"].update({"../escape": {}}),
                           lambda b: b.update(vendor_sha256="bad"),
                           lambda b: b.update(go_version="go1.26"),
                           lambda b: b["modules"].append(b["modules"][0]),
                           lambda b: b.update(inputs_sha256="a" * 64)):
                altered = copy.deepcopy(value)
                change(altered)
                with self.assertRaises(ValueError):
                    lock.validate(altered)
            path = root / "invalid.json"
            path.write_text('{"schema_version":1,"schema_version":1}')
            with self.assertRaisesRegex(ValueError, "duplicate"):
                lock.load(path)

    def test_input_fingerprint_tracks_code_modes_additions_and_deletions(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            value = fixture(root)
            original = value["inputs_sha256"]
            (root / "tools/sync.log").write_text("not an input")
            (root / "tools/README.md").write_text("documentation")
            self.assertEqual(lock.inputs_hash(root), original)
            path = root / "tools/scripts/generate.py"
            path.chmod(0o755)
            self.assertNotEqual(lock.inputs_hash(root), original)
            path.chmod(0o644)
            self.assertEqual(lock.inputs_hash(root), original)
            extra = root / "tools/extra.go"
            extra.write_text("package fixture")
            self.assertNotEqual(lock.inputs_hash(root), original)
            extra.unlink()
            self.assertEqual(lock.inputs_hash(root), original)
            path.write_text("changed")
            self.assertNotEqual(lock.inputs_hash(root), original)

    def test_input_and_toolchain_mismatch_rejected_before_install(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            value = fixture(root)
            with patch.object(lock.dependencies, "go", return_value="go1.26.3\n"):
                lock.check_inputs(root, value)
                changed = copy.deepcopy(value)
                changed["sources"]["sources"]["attacher"]["commit"] = "a" * 40
                seal(changed)
                with self.assertRaisesRegex(ValueError, "source revisions"):
                    lock.check_inputs(root, changed)
                with patch.object(lock.dependencies, "go", return_value="go1.26.5\n"):
                    with self.assertRaisesRegex(ValueError, "requires go1.26.3"):
                        lock.check_inputs(root, value)
                (root / "tools/scripts/generate.py").write_text("changed")
                with self.assertRaisesRegex(ValueError, "maintained code changed"):
                    lock.check_inputs(root, value)

    def test_install_preserves_original_library_and_optional_missing_sum(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            value = fixture(root)
            for name in lock.MANIFESTS[:3]:
                (root / name).unlink()
            library = (root / lock.LIB / "go.mod").read_bytes()
            with patch.object(lock.dependencies, "go", return_value="go1.26.3"):
                lock.install(root, value)
                with self.assertRaisesRegex(ValueError, "existing generated"):
                    lock.install(root, value)
            self.assertEqual(lock.manifests(root), value["manifests"])
            self.assertFalse((root / "go.work.sum").exists())
            self.assertEqual((root / lock.LIB / "go.mod").read_bytes(), library)

    def test_install_prevalidates_all_paths_before_any_write(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            value = fixture(root)
            for name in lock.MANIFESTS[:3]:
                (root / name).unlink()
            (root / lock.LIB / "go.sum").write_text("wrong original")
            with patch.object(lock.dependencies, "go", return_value="go1.26.3"):
                with self.assertRaisesRegex(ValueError, "original"):
                    lock.install(root, value)
            self.assertFalse((root / "go.mod").exists())
            (root / "go.mod").symlink_to("missing")
            with self.assertRaisesRegex(ValueError, "symlink"):
                lock.manifests(root)

    def test_vendor_manifest_graph_and_adapted_tampering_fail(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            value = fixture(root)
            with patch.object(lock.dependencies, "go", return_value="go1.26.3"), patch.object(
                lock, "graph", return_value=value["modules"]
            ) as graph:
                for phase in ("graph", "vendor", "adapted"):
                    lock.verify(root, value, phase)
                graph.return_value = [{"Path": "k8s.io/api", "Version": "v0.37.0"}]
                with self.assertRaisesRegex(ValueError, "graph differs"):
                    lock.verify(root, value, "graph")
                (root / "vendor/input.go").write_text("tampered")
                for phase in ("vendor", "adapted"):
                    with self.assertRaisesRegex(ValueError, "tree checksum"):
                        lock.verify(root, value, phase)
                (root / "go.work.sum").write_text("unexpected resolution")
                with self.assertRaisesRegex(ValueError, "manifests changed"):
                    lock.verify(root, value, "adapted")

    def test_special_vendor_files_and_nonboolean_module_flags_are_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            value = fixture(root)
            value["modules"] = [{"Path": lock.ROOT_MODULE, "Main": 1}]
            seal(value)
            with self.assertRaisesRegex(ValueError, "noncanonical module graph"):
                lock.validate(value)
            os.mkfifo(root / "vendor/extra.go")
            with self.assertRaisesRegex(ValueError, "not a regular"):
                lock.vendor_hash(root)

    def test_vendor_rejects_symlinks_and_missing_inventory(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture(root)
            (root / "vendor/alias").symlink_to("input.go")
            with self.assertRaisesRegex(ValueError, "symlink"):
                lock.vendor_hash(root)
            (root / "vendor/alias").unlink()
            (root / "vendor/modules.txt").unlink()
            with self.assertRaisesRegex(ValueError, "missing"):
                lock.vendor_hash(root)

    def test_module_normalization_omits_machine_paths_but_keeps_versions(self):
        modules = [{"Path": lock.ROOT_MODULE, "Main": True, "Dir": "/one"},
                   {"Path": lock.LIB_MODULE, "Main": True, "Dir": "/one/staging"},
                   {"Path": "k8s.io/api", "Version": "v0.36.1", "Dir": "/cache",
                    "Replace": {"Path": "k8s.io/api", "Version": "v0.36.3", "Dir": "/cache"}}]
        normalized = lock.normalize_modules(modules)
        self.assertNotIn("/cache", json.dumps(normalized))
        self.assertIn('"Version": "v0.36.3"', json.dumps(normalized))
        modules[-1]["Replace"] = {"Path": "../outside"}
        with self.assertRaisesRegex(ValueError, "local replacement"):
            lock.normalize_modules(modules)

    def test_update_capture_requires_pristine_record_and_unchanged_manifests(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            value = fixture(root)
            with self.assertRaises(FileNotFoundError):
                lock.capture(root)
            pristine = {key: value[key] for key in ("manifests", "modules", "vendor_sha256")}
            (root / "tmp/dependencies.pristine.json").write_bytes(lock.encoded(pristine))
            with patch.object(lock.dependencies, "go", return_value="go1.26.3"), patch.object(
                lock, "graph", return_value=value["modules"]
            ):
                lock.capture(root)
                self.assertEqual(lock.load(root / lock.CANDIDATE), value)
                with self.assertRaises(FileExistsError):
                    lock.capture(root)
                (root / "go.sum").write_text("changed")
                with self.assertRaisesRegex(ValueError, "changed after pristine"):
                    lock.capture(root)

    def test_sync_resolves_only_in_explicit_update_mode(self):
        script = Path(__file__).with_name("sync.sh").read_text()
        self.assertNotIn("gomod-require", script)
        self.assertNotIn("gomod-replace", script)
        self.assertIn('if [[ -n ${UPDATE_KUBERNETES} ]]; then\n  retry_go_dependencies go mod tidy\nelse\n', script)
        self.assertIn('export GOFLAGS="-mod=vendor"', script)
        self.assertLess(script.index("assembly_lock.py preflight"), script.index("build_environment.py bootstrap"))
        self.assertLess(script.index("assembly_lock.py vendor"), script.index("assembly_lock.py capture"))

    def test_normal_sync_missing_lock_fails_before_install_or_fetch(self):
        if os.uname().sysname != "Linux":
            self.skipTest("Linux sync preflight required")
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            scripts = root / "tools/scripts"
            scripts.mkdir(parents=True)
            for name in ("sync.sh", "assembly_sources.py", "assembly_dependencies.py", "assembly_lock.py",
                         "build_provenance.py", "image_inputs.py", "retry-go-dependencies.sh"):
                shutil.copy2(Path(__file__).with_name(name), scripts / name)
            active = root / "tools/assembly/sources.lock.json"
            active.parent.mkdir()
            active.write_bytes(lock.sources.DEFAULT_LOCK.read_bytes())
            result = subprocess.run(["bash", str(scripts / "sync.sh")], capture_output=True, text=True,
                                    env={**os.environ, "NORMALIZED_LOGGING": "true", "SKIP_SANITY_CHECK": "false"}, timeout=30)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("dependencies.lock.json", result.stderr)
            self.assertNotIn("pip install", result.stderr)
            self.assertFalse((root / "tmp").exists())
            self.assertFalse((root / build_environment.ENVIRONMENT).exists())

    def test_image_lock_changes_invalidate_dependency_bundle(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            value = fixture(root)
            path = root / lock.image_inputs.LOCK
            changed = json.loads(path.read_text())
            changed["runtime"]["platforms"]["linux/arm64"] = "sha256:" + "a" * 64
            path.write_text(json.dumps(changed))
            with patch.object(lock.dependencies, "go") as go:
                with self.assertRaisesRegex(ValueError, "maintained code changed"):
                    lock.check_inputs(root, value)
            go.assert_not_called()
            path.unlink()
            with self.assertRaises(FileNotFoundError):
                lock.inputs_hash(root)

    def test_builder_lock_changes_invalidate_dependency_bundle(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            value = fixture(root)
            path = root / build_environment.LOCK
            changed = json.loads(path.read_text())
            changed["versions"]["python"] = "3.13.6"
            path.write_text(json.dumps(changed))
            with patch.object(lock.dependencies, "go") as go:
                with self.assertRaisesRegex(ValueError, "maintained code changed"):
                    lock.check_inputs(root, value)
            go.assert_not_called()
            path.unlink()
            with self.assertRaisesRegex(ValueError, "not a regular"):
                lock.inputs_hash(root)

    def test_provenance_drift_and_reviewed_lock_changes_invalidate_bundle(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            value = fixture(root)
            path = root / "release-tools/build.make"
            path.write_text("intentional local patch")
            with self.assertRaisesRegex(ValueError, "provenance mismatch"):
                lock.inputs_hash(root)
            provenance = lock.build_provenance.load(root)
            provenance["local_changes"]["build.make"] = {
                "entry": lock.build_provenance.file_entry(path), "reason": "fixture reviewed patch"}
            (root / lock.build_provenance.LOCK).write_bytes(lock.encoded(provenance))
            with patch.object(lock.dependencies, "go") as go:
                with self.assertRaisesRegex(ValueError, "maintained code changed"):
                    lock.check_inputs(root, value)
            go.assert_not_called()

    def test_malformed_modules_and_maintained_directory_links_fail_closed(self):
        for modules in ([None], [{"Path": "example.org/a"}], [{"Path": "example.org/a", "Version": 3}],
                        [{"Path": "example.org/a", "Version": "latest"}]):
            with self.assertRaises(ValueError):
                lock.normalize_modules(modules)
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture(root)
            (root / "tools/alias").symlink_to("scripts", target_is_directory=True)
            with self.assertRaisesRegex(ValueError, "symlink"):
                lock.inputs_hash(root)

    def test_semver_ordering_includes_test_and_prerelease_requirements(self):
        versions = ["v1.0.0-alpha.2", "v1.0.0-alpha.10", "v1.0.0-beta", "v1.0.0", "v1.0.1"]
        self.assertEqual(sorted(reversed(versions), key=lock.version_key), versions)
        with self.assertRaises(ValueError):
            lock.version_key("latest")


@unittest.skipUnless(shutil.which("go"), "Go required for seed parser fixture")
class SeedTests(unittest.TestCase):
    def test_workspace_and_local_replacement_escapes_fail_before_graph_loading(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture(root)
            (root / "go.mod").write_text(f"module {lock.ROOT_MODULE}\ngo 1.26.0\n")
            (root / lock.LIB / "go.mod").write_text(f"module {lock.LIB_MODULE}\ngo 1.26.0\n")
            work = f"go 1.26.0\nuse (\n.\n./{lock.LIB}\n)\n"
            (root / "go.work").write_text(work)
            lock.check_layout(root)
            for body in ("go 1.26.0\nuse /unrelated\n", work + "replace example.org/a => /unrelated\n"):
                (root / "go.work").write_text(body)
                with patch.object(lock.dependencies, "resolved_modules") as resolve:
                    with self.assertRaisesRegex(ValueError, "canonical workspace"):
                        lock.graph(root)
                resolve.assert_not_called()
            (root / "go.work").write_text(work)
            with (root / "go.mod").open("a") as output:
                output.write("replace example.org/a => /unrelated\n")
            with patch.object(lock.dependencies, "resolved_modules") as resolve:
                with self.assertRaisesRegex(ValueError, "local replacement"):
                    lock.graph(root)
            resolve.assert_not_called()

    def test_explicit_patch_alignment_is_go_parsed_and_never_edits_upstream(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture(root)
            for name in lock.MANIFESTS[:3]:
                (root / name).unlink()
            paths = {name: root / f"tmp/external-{name}/pkg/{name}/go.mod" for name in lock.sources.CONTROLLERS}
            paths["snapshotter/client"] = paths["snapshotter"].parent / "client/go.mod"
            paths["csi-lib-utils"] = root / lock.LIB / "go.mod"
            for name, path in paths.items():
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(f"module github.com/kubernetes-csi/{name}\ngo 1.26.0\n" + "".join(
                    f"require {p} v0.36.1\nreplace {p} => {p} v0.36.1\n" for p in sorted(lock.dependencies.CORE)) +
                    "require example.org/test-only v1.2.3 // indirect\n")
            originals = {name: path.read_bytes() for name, path in paths.items()}
            with patch.dict(os.environ, {"GOPROXY": "off"}):
                with self.assertRaisesRegex(ValueError, "cannot target"):
                    lock.seed(root, "1.35.9")
                self.assertFalse((root / "go.mod").exists())
                lock.seed(root, "1.36.3")
                parsed = json.loads(lock.dependencies.go(root, "mod", "edit", "-json", workspace=False))
            requires = {item["Path"]: item["Version"] for item in parsed["Require"]}
            self.assertEqual(requires["example.org/test-only"], "v1.2.3")
            for path in lock.dependencies.CORE:
                self.assertEqual(requires[path], "v0.36.3")
            self.assertEqual({name: path.read_bytes() for name, path in paths.items()}, originals)


class CompleteCandidateTests(unittest.TestCase):
    def test_locked_replay_uses_embedded_sources_without_resolving_channels(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            value = fixture(root)
            selected = root / "selected.json"
            selected.write_bytes(lock.encoded(value))
            output = root / ".work/rechecked.json"
            active_bytes = (root / "tools/assembly/sources.lock.json").read_bytes()

            def snapshot(_, destination):
                fixture(destination)

            with patch.object(isolated_sync, "ROOT", root), patch.object(
                isolated_sync, "snapshot", side_effect=snapshot
            ), patch.object(isolated_sync.subprocess, "run", return_value=subprocess.CompletedProcess([], 0)) as run, patch(
                "sys.argv", ["isolated_sync", "--image", BUILD_IMAGE, "--dependency-lock", str(selected),
                             "--candidate-output", str(output)]
            ), patch.object(lock.sources, "resolve_candidate") as resolve:
                self.assertEqual(isolated_sync.main(), 0)
            resolve.assert_not_called()
            self.assertNotIn("--update-dependencies", run.call_args.args[0][-1])
            self.assertTrue(run.call_args.args[0][-1].endswith("--lock tools/assembly/dependencies.lock.json adapted"))
            self.assertEqual(output.read_bytes(), selected.read_bytes())
            self.assertEqual((root / "tools/assembly/sources.lock.json").read_bytes(), active_bytes)
            self.assertFalse((root / lock.LOCK).exists())

    def test_invalid_dependency_options_rejected_before_snapshot(self):
        for extra in (["--update-dependencies", "latest"], ["--update-dependencies", "1.36.3"],
                      ["--update-dependencies", "1.36.3", "--runtime-baseline", ".work/baseline"],
                      ["--update-dependencies", "1.36.3", "--dependency-lock", "missing"],
                      ["--dependency-lock", "missing"]):
            with self.subTest(extra=extra), patch.object(isolated_sync, "snapshot") as snapshot, patch(
                "sys.argv", ["isolated_sync", "--image", BUILD_IMAGE, *extra]
            ), self.assertRaises(SystemExit):
                isolated_sync.main()
            snapshot.assert_not_called()

    def test_dependency_update_exports_one_bundle_only_after_all_checks(self):
        for status, existing in ((0, False), (23, False), (0, True), (23, True)):
            with self.subTest(status=status), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                active = fixture(root)
                active_data = lock.encoded(active)
                if existing:
                    (root / lock.LOCK).write_bytes(active_data)
                snapshots = []
                output = root / ".work/candidate.json"

                def snapshot(_, destination):
                    value = fixture(destination)
                    snapshots.append(destination)
                    if existing:
                        (destination / lock.LOCK).write_bytes(active_data)
                    (destination / lock.CANDIDATE).write_bytes(lock.encoded(value))

                with patch.object(isolated_sync, "ROOT", root), patch.object(
                    isolated_sync, "snapshot", side_effect=snapshot
                ), patch.object(isolated_sync.subprocess, "run", return_value=subprocess.CompletedProcess([], status)) as run, patch(
                    "sys.argv", ["isolated_sync", "--image", BUILD_IMAGE, "--update-dependencies", "1.36.3",
                                 "--candidate-output", str(output)]
                ), patch.object(lock.sources, "resolve_candidate") as resolve:
                    self.assertEqual(isolated_sync.main(), status)
                resolve.assert_not_called()
                self.assertEqual(output.exists(), status == 0)
                self.assertIn("sync.sh --update-dependencies 1.36.3 &&", run.call_args.args[0][-1])
                self.assertTrue(run.call_args.args[0][-1].endswith("--lock tmp/dependencies.candidate.json adapted"))
                self.assertEqual(lock.sources.load(root / "tools/assembly/sources.lock.json"), active["sources"])
                self.assertFalse((snapshots[0] / lock.LOCK).exists())
                backup = snapshots[0].parent / "previous-dependencies.lock.json"
                self.assertEqual(backup.exists(), existing)
                self.assertEqual((root / lock.LOCK).exists(), existing)
                if existing:
                    self.assertEqual(backup.read_bytes(), active_data)
                    self.assertEqual((root / lock.LOCK).read_bytes(), active_data)
                if status == 0:
                    self.assertEqual(lock.load(output), active)
                    with self.assertRaises(FileExistsError):
                        isolated_sync.export_candidate(output.read_bytes(), output, lock.validate)


if __name__ == "__main__":
    unittest.main()
