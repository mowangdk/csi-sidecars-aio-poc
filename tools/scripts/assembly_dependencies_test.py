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
import platform
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import assembly_dependencies as dependencies


def document(version="v0.36.1"):
    return {"Require": [{"Path": path, "Version": version} for path in sorted(dependencies.CORE)]}


def graph(version="v0.36.3"):
    return [{"Path": path, "Version": version} for path in sorted(dependencies.CORE)]


class DependencyCompatibilityTests(unittest.TestCase):
    def test_all_family_members_and_kubernetes_match_exact_release(self):
        modules = graph() + [
            {"Path": "k8s.io/kubernetes", "Version": "v1.36.3"},
            {"Path": "k8s.io/component-helpers", "Version": "v0.36.3"},
            {"Path": "k8s.io/new-staging-module", "Version": "v0.36.3"},
        ]
        result = dependencies.validate_graph(modules, {"attacher@revision": document()})
        self.assertEqual(len(result), 6)
        for index in range(len(modules)):
            with self.subTest(module=modules[index]["Path"]):
                changed = copy.deepcopy(modules)
                changed[index]["Version"] = changed[index]["Version"].replace("36.3", "36.2")
                with self.assertRaisesRegex(ValueError, "incompatible"):
                    dependencies.validate_graph(changed, {"attacher@revision": document()})

    def test_independently_versioned_repositories_are_not_staging(self):
        modules = graph() + [{"Path": path, "Version": "v9.1.0"} for path in dependencies.INDEPENDENT]
        modules += [{"Path": "sigs.k8s.io/controller-runtime", "Version": "v0.25.0"}]
        self.assertEqual(len(dependencies.validate_graph(modules, {"source": document()})), 3)

    def test_effective_replacements_and_placeholder_requirements(self):
        modules = graph()
        modules.append({"Path": "k8s.io/kubelet", "Version": "v0.0.0",
                        "Replace": {"Path": "k8s.io/kubelet", "Version": "v0.36.3"}})
        # A patch upgrade is allowed; a downgrade is never hidden by the label.
        modules[0]["Version"] = "v0.36.1"
        modules[0]["Replace"] = {"Path": modules[0]["Path"], "Version": "v0.36.3"}
        self.assertEqual(len(dependencies.validate_graph(modules, {"source": document()})), 4)
        for selected in ("v0.37.0", "v0.36.4"):
            modules[0]["Version"] = selected
            with self.assertRaisesRegex(ValueError, "replaces it with older"):
                dependencies.validate_graph(modules, {"source": document()})

    def test_cross_minor_sources_have_component_and_revision_diagnostics(self):
        documents = {"attacher@aaaa": document(), "csi-lib-utils@bbbb": document("v0.37.0")}
        with self.assertRaises(ValueError) as raised:
            dependencies.validate_sources(documents)
        self.assertIn("attacher@aaaa", str(raised.exception))
        self.assertIn("csi-lib-utils@bbbb", str(raised.exception))
        self.assertIn("k8s.io/client-go requires v0.37.0", str(raised.exception))
        with self.assertRaisesRegex(ValueError, "csi-lib-utils@bbbb"):
            dependencies.validate_graph(graph(), documents)

    def test_source_patch_floor_is_enforced_even_if_not_vendored(self):
        original = document()
        original["Require"].append({"Path": "k8s.io/component-helpers", "Version": "v0.36.4"})
        dependencies.validate_sources({"snapshotter/client@revision": original})
        with self.assertRaisesRegex(ValueError, "snapshotter/client@revision.*component-helpers requires v0.36.4"):
            dependencies.validate_graph(graph(), {"snapshotter/client@revision": original})
        original["Require"][-1]["Version"] = "v0.35.2"
        dependencies.validate_graph(graph(), {"source": original})

    def test_local_forks_unknown_versions_and_missing_modules_fail_closed(self):
        for replacement in ({"Path": "../fork"}, {"Path": "example.org/fork", "Version": "v0.36.3"},
                            {"Path": "k8s.io/api", "Version": "v0.36.3-rc.1"}):
            modules = graph()
            modules[0]["Replace"] = replacement
            with self.subTest(replacement=replacement), self.assertRaises(ValueError):
                dependencies.validate_graph(modules, {"source": document()})
        for modules in ([], graph()[1:], graph() + graph()):
            with self.assertRaises(ValueError):
                dependencies.validate_graph(modules, {"source": document()})
        for documents in ({}, {"source": {"Require": []}}, {"source": {"Require": document()["Require"] * 2}}):
            with self.assertRaises(ValueError):
                dependencies.validate_sources(documents)
        for path, version in (("k8s.io/new-staging", "v0.0.0"), ("k8s.io/api", "v1.36.1"),
                              ("k8s.io/kubernetes", "v0.36.1"), ("k8s.io/api", "v0.036.1")):
            with self.assertRaises(ValueError):
                dependencies.release(path, version)

    def test_core_and_monolithic_requirements_cannot_hide_behind_placeholders(self):
        for path in sorted(dependencies.CORE | {"k8s.io/kubernetes"}):
            original = document()
            original["Require"] = [item for item in original["Require"] if item["Path"] != path]
            original["Require"].append({"Path": path, "Version": "v0.0.0"})
            with self.subTest(path=path), self.assertRaisesRegex(ValueError, "concrete Kubernetes release"):
                dependencies.validate_sources({"source@revision": original})
        modules = graph() + [{"Path": "k8s.io/kubernetes", "Version": "v0.0.0",
                              "Replace": {"Path": "k8s.io/kubernetes", "Version": "v1.36.3"}}]
        with self.assertRaisesRegex(ValueError, "invalid Kubernetes release"):
            dependencies.validate_graph(modules, {"source": document()})

    def test_go_json_and_vendor_inventory_reject_empty_or_corrupt_results(self):
        text = " \n" + "\n".join(json.dumps(module) for module in graph())
        self.assertEqual(dependencies.json_stream(text), graph())
        for invalid in ("", "[]", '{"Path":"a","Path":"b"}', '{"Error":"unresolved"}', text + "broken"):
            with self.assertRaises(ValueError):
                dependencies.json_stream(invalid)
        text = ("## workspace\n# k8s.io/api v0.37.0 => k8s.io/api v0.36.1\n"
                "## explicit; go 1.26.0\nk8s.io/api/core/v1\n# k8s.io/api => k8s.io/api v0.36.1\n")
        self.assertEqual(dependencies.vendored_paths(text), ["k8s.io/api"])
        for invalid in ("", text + text):
            with self.assertRaises(ValueError):
                dependencies.vendored_paths(invalid)

    def test_go_commands_ignore_ambient_workspace_and_module_flags(self):
        with patch.dict(os.environ, {"GOWORK": "/unrelated", "GOFLAGS": "-modfile=/unrelated"}), patch.object(
            dependencies.subprocess, "check_output", return_value="{}"
        ) as run:
            dependencies.go(Path("/isolated"), "list", "-m", "-json", "all")
            env = run.call_args.kwargs["env"]
            self.assertEqual(env["GOWORK"], "/isolated/go.work")
            self.assertEqual(env["GOFLAGS"], "")
            self.assertEqual(env["GOTOOLCHAIN"], "local")
            self.assertEqual(env["GOSUMDB"], "sum.golang.org")
            self.assertEqual(env["GONOSUMDB"], "")
            self.assertEqual(env["GOENV"], "off")
            dependencies.go(Path("/isolated"), "mod", "edit", "-json", workspace=False)
            self.assertEqual(run.call_args.kwargs["env"]["GOWORK"], "off")

    def test_sync_checks_sources_graph_and_vendor_before_builds_without_bypass(self):
        script = Path(__file__).with_name("sync.sh").read_text()
        self.assertIn("SKIP_SANITY_CHECK is no longer supported", script)
        self.assertNotIn("gomod-k8sapi", script)
        self.assertNotIn("s/v0.35.0/v0.35.2/g", script)
        checkpoints = ["checkout csi-lib-utils", "assembly_dependencies.py sources",
                       "retry_go_dependencies go mod tidy", "assembly_dependencies.py graph",
                       "retry_go_dependencies go work vendor", "assembly_dependencies.py vendor",
                       "assembly_lock.py capture", "make build"]
        positions = [script.index(checkpoint) for checkpoint in checkpoints]
        self.assertEqual(positions, sorted(positions))
        self.assertEqual(script.count("retry_go_dependencies go mod tidy"), 1)


@unittest.skipUnless(shutil.which("go"), "Go required for real module parser/vendor fixtures")
class GoModuleFixtureTests(unittest.TestCase):
    def test_real_workspace_vendor_handles_distinct_requirement_headers(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "library").mkdir()
            (root / "vendor").mkdir()
            (root / "go.work").write_text("go 1.26.0\nuse (\n.\n./library\n)\n")
            for name, version in (("go.mod", "v0.36.3"), ("library/go.mod", "v0.36.0")):
                (root / name).write_text(f"module example.org/{name}\ngo 1.26.0\n" + "".join(
                    f"require {path} {version}\n" for path in sorted(dependencies.CORE)))
            inventory = "## workspace\n" + "".join(
                f"# {path} {version}\n## explicit; go 1.26.0\n"
                for path in sorted(dependencies.CORE) for version in ("v0.36.0", "v0.36.3"))
            (root / "vendor/modules.txt").write_text(inventory)
            with patch.dict(os.environ, {"GOPROXY": "off"}):
                modules = dependencies.resolved_modules(root, vendored=True)
            self.assertEqual(len(modules), 3)
            self.assertTrue(all(module["Version"] == "v0.36.3" for module in modules))
            dependencies.validate_graph(modules, {"original-library@revision": document("v0.36.0")})
            with self.assertRaisesRegex(ValueError, "duplicate"):
                dependencies.vendored_paths(inventory + "# k8s.io/api v0.36.0\n")

    def test_real_go_parser_and_vendor_resolution_preserve_effective_versions(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "go.work").write_text("go 1.26.0\nuse .\n")
            mod = "module example.org/fixture\ngo 1.26.0\nrequire (\n"
            mod += "".join(f" {path} v0.37.0\n" for path in sorted(dependencies.CORE)) + ")\nreplace (\n"
            mod += "".join(f" {path} => {path} v0.36.1\n" for path in sorted(dependencies.CORE)) + ")\n"
            (root / "go.mod").write_text(mod)
            vendor = root / "vendor/modules.txt"
            vendor.parent.mkdir()
            vendor.write_text("## workspace\n" + "".join(
                f"# {path} v0.37.0 => {path} v0.36.1\n## explicit; go 1.26.0\n"
                for path in sorted(dependencies.CORE)) + "".join(
                f"# {path} => {path} v0.36.1\n" for path in sorted(dependencies.CORE)))
            with patch.dict(os.environ, {"GOPROXY": "off"}):
                original = json.loads(dependencies.go(root, "mod", "edit", "-json", workspace=False))
                modules = dependencies.resolved_modules(root, vendored=True)
            self.assertEqual(original["Replace"][0]["New"]["Version"], "v0.36.1")
            self.assertEqual(len(modules), 3)
            self.assertEqual(modules[0]["Version"], "v0.37.0")
            self.assertEqual(modules[0]["Replace"]["Version"], "v0.36.1")
            with self.assertRaisesRegex(ValueError, "replaces it with older"):
                dependencies.validate_graph(modules, {"fixture@original-sha": original})
            self.assertEqual((root / "go.mod").read_text(), mod)
            self.assertFalse((root / "go.sum").exists())
            self.assertFalse((root / "go.work.sum").exists())

    def test_all_six_original_manifests_use_frozen_revision_labels(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            frozen = root / "tmp/sources.lock.json"
            frozen.parent.mkdir()
            frozen.write_bytes(dependencies.assembly_sources.DEFAULT_LOCK.read_bytes())
            paths = [root / f"tmp/external-{name}/pkg/{name}/go.mod"
                     for name in dependencies.assembly_sources.CONTROLLERS]
            paths += [paths[-1].parent / "client/go.mod", root / dependencies.LIBRARY / "go.mod"]
            for index, path in enumerate(paths):
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("module example.org/source\ngo 1.26.0\n" + "\n".join(
                    f"require {module} v0.36.{index}\n" for module in sorted(dependencies.CORE)))
            documents = dependencies.source_documents(root)
            self.assertEqual(len(documents), 6)
            self.assertTrue(any(label.startswith("snapshotter/client@") for label in documents))
            self.assertTrue(any(label.startswith("csi-lib-utils@") for label in documents))
            dependencies.validate_sources(documents)
            dependencies.validate_graph(graph("v0.36.5"), documents)
            paths[-1].unlink()
            with self.assertRaises(subprocess.CalledProcessError):
                dependencies.source_documents(root)

    def test_skip_environment_cannot_bypass_sync(self):
        if platform.system() != "Linux":
            self.skipTest("Linux sync preflight required")
        script = Path(__file__).with_name("sync.sh").resolve()
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            scripts = root / "tools/scripts"
            scripts.mkdir(parents=True)
            for name in ("sync.sh", "assembly_sources.py", "retry-go-dependencies.sh"):
                shutil.copy2(script.with_name(name), scripts / name)
            lock = root / "tools/assembly/sources.lock.json"
            lock.parent.mkdir()
            lock.write_bytes(dependencies.assembly_sources.DEFAULT_LOCK.read_bytes())
            result = subprocess.run(["bash", str(scripts / "sync.sh")], text=True, capture_output=True,
                                    env={**os.environ, "SKIP_SANITY_CHECK": "true", "NORMALIZED_LOGGING": "true"},
                                    timeout=30)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("SKIP_SANITY_CHECK is no longer supported", result.stdout)
            self.assertFalse((root / "tmp").exists())


if __name__ == "__main__":
    unittest.main()
