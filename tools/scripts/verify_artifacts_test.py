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

import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import verify_artifacts as verify


def example(body):
    return f"# BEGIN AIO CLI ARGS\n{body}\n# END AIO CLI ARGS"


class ReadmeTests(unittest.TestCase):
    def test_extracts_only_aio_arguments(self):
        readme = '- "--endpoint=/driver.sock"\n' + example(
            '# Common options\n- "--controllers=attacher,resizer"\n- "--attacher-timeout=30s"'
        )
        self.assertEqual(
            verify.read_example_args(readme),
            ["--controllers=attacher,resizer", "--attacher-timeout=30s"],
        )

    def test_rejects_missing_empty_or_malformed_examples(self):
        cases = [
            "", example(""), example('# Only a comment'),
            example('- "--controllers=attacher"') * 2,
            "# END AIO CLI ARGS\n# BEGIN AIO CLI ARGS",
            example('- "unterminated'), example('- --controllers=attacher'),
            example('- "--help"'), example('- "--help=true"'), example('- "--"'),
            example('- "positional"'),
        ]
        for readme in cases:
            with self.subTest(readme=readme), self.assertRaises(ValueError):
                verify.read_example_args(readme)

    def test_help_is_last_and_example_is_passed_unchanged(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "README.md").write_text(example('- "--controllers=attacher"'))
            with patch.object(verify, "run") as run:
                run.return_value = subprocess.CompletedProcess([], 0, "--controllers string", "")
                verify.verify_cli(root)
                run.assert_called_once_with([
                    str(root / "bin/csi-sidecars"), "--controllers=attacher", "--help"
                ])

    def test_invalid_flag_failure_is_not_swallowed(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "README.md").write_text(example('- "--timeout=30s"'))
            with patch.object(verify, "run", side_effect=subprocess.CalledProcessError(2, "binary")):
                with self.assertRaises(subprocess.CalledProcessError):
                    verify.verify_cli(root)


class ImageTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        (self.root / "bin").mkdir()
        self.command = "snapshot-controller"
        self.contents = b"expected snapshot-controller executable"
        (self.root / "bin" / self.command).write_bytes(self.contents)
        self.entrypoint = ["/snapshot-controller"]
        self.packaged = self.contents
        self.help = "  -retry-crd-interval-max duration\n"
        self.exit_code = "0"
        self.start_error = None
        self.calls = []
        self.mock = patch.object(verify, "run", side_effect=self.engine)
        self.mock.start()
        self.addCleanup(self.mock.stop)

    def engine(self, argv):
        self.calls.append(argv)
        operation = argv[1]
        output = ""
        if operation == "image":
            output = json.dumps(self.entrypoint)
        elif operation == "create":
            self.assertIn("--network=none", argv)
            self.assertEqual(argv[-1], "--help")
            output = "smoke-container\n"
        elif operation == "cp":
            Path(argv[-1]).write_bytes(self.packaged)
        elif operation == "start":
            if self.start_error:
                raise self.start_error
            output = self.help
        elif operation == "inspect":
            output = self.exit_code
        elif operation != "rm":
            self.fail(f"Unexpected container operation: {argv}")
        return subprocess.CompletedProcess(argv, 0, output, "")

    def verify_image(self):
        verify.verify_image(self.root, ["docker"], self.command, "latest")

    def assert_cleaned_up(self):
        self.assertEqual(self.calls[-1], ["docker", "rm", "--force", "smoke-container"])

    def test_correct_image(self):
        self.verify_image()
        self.assert_cleaned_up()

    def test_rejects_aio_entrypoint_under_snapshot_tag(self):
        self.entrypoint = ["/csi-sidecars"]
        with self.assertRaisesRegex(ValueError, "expected entrypoint"):
            self.verify_image()
        self.assertEqual(len(self.calls), 1)

    def test_rejects_wrong_binary_even_with_correct_entrypoint(self):
        self.packaged = b"an unrelated executable"
        with self.assertRaisesRegex(ValueError, "packaged executable differs"):
            self.verify_image()
        self.assert_cleaned_up()

    def test_rejects_wrong_component_help(self):
        self.help = "  --controllers string\n"
        with self.assertRaisesRegex(ValueError, "expected flag"):
            self.verify_image()
        self.assert_cleaned_up()

    def test_rejects_nonzero_exit_even_if_help_looks_correct(self):
        self.exit_code = "2"
        with self.assertRaisesRegex(ValueError, "exited with status 2"):
            self.verify_image()
        self.assert_cleaned_up()

    def test_removes_container_after_timeout(self):
        self.start_error = subprocess.TimeoutExpired("docker start", 30)
        with self.assertRaises(subprocess.TimeoutExpired):
            self.verify_image()
        self.assert_cleaned_up()

    def test_removes_container_after_start_failure(self):
        self.start_error = subprocess.CalledProcessError(1, "docker start")
        with self.assertRaises(subprocess.CalledProcessError):
            self.verify_image()
        self.assert_cleaned_up()

    def test_component_flags_accept_go_and_pflag_help(self):
        for command, flag in verify.COMMAND_FLAGS.items():
            for prefix in ("-", "--"):
                with self.subTest(command=command, prefix=prefix):
                    verify.check_help(command, f"  {prefix}{flag} string\n")
            with self.assertRaises(ValueError):
                verify.check_help(command, f"Description mentioning --{flag}")


if __name__ == "__main__":
    unittest.main()
