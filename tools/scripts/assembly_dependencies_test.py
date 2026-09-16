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
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import assembly_dependencies as dependencies


def document(version="v0.36.1"):
    return {"Require": [{"Path": path, "Version": version} for path in sorted(dependencies.CORE)]}


class SourceValidationTests(unittest.TestCase):
    def test_cross_minor_sources_have_component_and_revision_diagnostics(self):
        documents = {"attacher@aaaa": document(), "csi-lib-utils@bbbb": document("v0.37.0")}
        with self.assertRaises(ValueError) as raised:
            dependencies.validate_sources(documents)
        self.assertIn("attacher@aaaa", str(raised.exception))
        self.assertIn("csi-lib-utils@bbbb", str(raised.exception))
        self.assertIn("k8s.io/client-go requires v0.37.0", str(raised.exception))

    def test_missing_modules_and_unknown_versions_fail_closed(self):
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

    def test_sync_seeds_manifests_then_builds_without_redundant_checks(self):
        script = Path(__file__).with_name("sync.sh").read_text()
        # The removed post-resolution consistency checks must not come back.
        for removed in ("assembly_dependencies.py sources", "assembly_dependencies.py graph",
                        "assembly_dependencies.py vendor", "build_binaries.py", "build_provenance.py"):
            self.assertNotIn(removed, script)
        checkpoints = ["checkout csi-lib-utils", "assembly_dependencies.py seed",
                       "retry_go_dependencies go mod tidy", "retry_go_dependencies go work vendor",
                       "make build"]
        positions = [script.index(checkpoint) for checkpoint in checkpoints]
        self.assertEqual(positions, sorted(positions))
        self.assertEqual(script.count("retry_go_dependencies go mod tidy"), 1)


@unittest.skipUnless(shutil.which("go"), "Go required for the real source-manifest fixture")
class SourceDocumentTests(unittest.TestCase):
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
            paths[-1].unlink()
            with self.assertRaises(subprocess.CalledProcessError):
                dependencies.source_documents(root)


if __name__ == "__main__":
    unittest.main()
