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

CORE = ("k8s.io/api", "k8s.io/apimachinery", "k8s.io/client-go")


class SeedTests(unittest.TestCase):
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

    def test_seed_requires_an_explicit_kubernetes_release(self):
        for kubernetes in ("", "1.36", "v1.36.3", "1.36.x"):
            with self.subTest(kubernetes=kubernetes), self.assertRaisesRegex(ValueError, "explicit Kubernetes release"):
                dependencies.seed(Path("/isolated"), kubernetes)

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
        # The committed generated tree is removed (after preflight) before any
        # assembly input is created.
        self.assertLess(script.index("preflight"), script.index("cleanup.sh"))
        self.assertLess(script.index("cleanup.sh"), script.index("mkdir tmp"))


@unittest.skipUnless(shutil.which("go"), "Go required for the real source-manifest fixture")
class SourceDocumentTests(unittest.TestCase):
    def test_seed_generates_aligned_manifests_from_the_six_original_sources(self):
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
                    f"require {module} v0.36.{index}\n" for module in CORE))
            documents = dependencies.source_documents(root)
            self.assertEqual(len(documents), 6)
            self.assertTrue(any(label.startswith("snapshotter/client@") for label in documents))
            self.assertTrue(any(label.startswith("csi-lib-utils@") for label in documents))

            # seed forces every Kubernetes family module onto the selected release
            # regardless of the (deliberately mismatched) source patch levels.
            dependencies.seed(root, "1.36.3")
            generated = (root / "go.mod").read_text()
            self.assertIn(f"module {dependencies.ROOT_MODULE}", generated)
            for module in CORE:
                self.assertIn(f"replace {module} => {module} v0.36.3", generated)
            self.assertIn(
                f"replace {dependencies.LIB_MODULE} => ./{dependencies.LIBRARY}", generated)
            self.assertTrue((root / "go.work").exists())

            paths[-1].unlink()
            with self.assertRaises(subprocess.CalledProcessError):
                dependencies.source_documents(root)


if __name__ == "__main__":
    unittest.main()
