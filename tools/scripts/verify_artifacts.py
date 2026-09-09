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

"""Smoke-test assembled binaries and images without a cluster or CSI socket."""

import argparse
import filecmp
import json
from pathlib import Path
import re
import shlex
import subprocess
import sys
import tempfile


ROOT = Path(__file__).resolve().parents[2]
# A distinct flag from each command, not just the shared logging flags.
COMMAND_FLAGS = {
    "csi-sidecars": "controllers",
    "snapshot-controller": "retry-crd-interval-max",
    "snapshot-conversion-webhook": "tls-private-key-file",
}


def run(command):
    return subprocess.run(
        command, check=True, capture_output=True, text=True, timeout=30
    )


def read_example_args(readme):
    # Deliberately parse only the marked, double-quoted YAML argument list,
    # not arbitrary YAML or the separate CSI driver's command line.
    begin = "# BEGIN AIO CLI ARGS"
    end = "# END AIO CLI ARGS"
    if readme.count(begin) != 1 or readme.count(end) != 1:
        raise ValueError("README must contain exactly one marked AIO argument list")
    body = readme.split(begin, 1)[1]
    if end not in body:
        raise ValueError("README AIO argument markers are out of order")
    args = []
    for line in body.split(end, 1)[0].splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        if not line.startswith('- "'):
            raise ValueError(f"Expected a double-quoted YAML argument, got: {line}")
        arg = json.loads(line[2:])
        if not isinstance(arg, str) or not arg.startswith("--"):
            raise ValueError(f"Expected an AIO flag, got: {arg!r}")
        # Help must be last when we invoke the binary, otherwise flag parsing
        # could stop before validating the remainder of the example.
        if arg == "--" or arg.split("=", 1)[0] == "--help":
            raise ValueError("Do not stop flag parsing with -- or --help in the README argument list")
        args.append(arg)
    if not args:
        raise ValueError("README AIO argument list is empty")
    return args


def check_help(command, output):
    flag = COMMAND_FLAGS[command]
    if not re.search(r"^\s*--?" + re.escape(flag) + r"(?:\s|=|$)", output, re.M):
        raise ValueError(f"{command}: help is missing its expected flag --{flag}")


def verify_cli(root):
    args = read_example_args((root / "README.md").read_text())
    result = run([str(root / "bin/csi-sidecars"), *args, "--help"])
    check_help("csi-sidecars", result.stdout + result.stderr)
    print("README AIO arguments parse successfully (no controllers started)")


def verify_image(root, engine, command, tag):
    image = f"{command}:{tag}"
    binary = root / "bin" / command
    if not binary.is_file():
        raise ValueError(f"Missing reference binary: {binary}; run make build first")
    result = run([*engine, "image", "inspect", "--format", "{{json .Config.Entrypoint}}", image])
    if json.loads(result.stdout) != [f"/{command}"]:
        raise ValueError(f"{image}: expected entrypoint /{command}, got {result.stdout.strip()}")

    # Inspect the file without requiring a shell in the distroless image. Use
    # the same container for help, with networking disabled. Always remove it,
    # including when the executable fails or the help command times out.
    container = run([*engine, "create", "--network=none", image, "--help"]).stdout.strip()
    try:
        with tempfile.TemporaryDirectory(prefix="csi-image-smoke-") as directory:
            extracted = Path(directory) / command
            run([*engine, "cp", f"{container}:/{command}", str(extracted)])
            if not filecmp.cmp(binary, extracted, shallow=False):
                raise ValueError(f"{image}: packaged executable differs from {binary}")
        result = run([*engine, "start", "--attach", container])
        # docker start --attach is not sufficient to assert the process exit
        # status across container engines; inspect the stopped container too.
        status = run([*engine, "inspect", "--format", "{{.State.ExitCode}}", container])
        if status.stdout.strip() != "0":
            raise ValueError(f"{image}: --help exited with status {status.stdout.strip()}")
        check_help(command, result.stdout + result.stderr)
        print(f"{image}: entrypoint, executable contents, and help verified")
    finally:
        run([*engine, "rm", "--force", container])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("cli", "images"))
    parser.add_argument("--engine", default="docker", help="Container CLI, e.g. podman")
    parser.add_argument("--tag", default="latest", help="Local image tag to check")
    args = parser.parse_args()
    try:
        if args.mode == "cli":
            verify_cli(ROOT)
        else:
            engine = shlex.split(args.engine)
            if not engine:
                raise ValueError("Container engine must not be empty")
            for command in COMMAND_FLAGS:
                verify_image(ROOT, engine, command, args.tag)
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        print(f"Artifact verification failed: {error}", file=sys.stderr)
        if isinstance(error, subprocess.CalledProcessError):
            print(error.stdout + error.stderr, file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
