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

"""Generate go.mod/go.work from the original sources, aligning the Kubernetes
family on the explicitly selected release."""

import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys

import assembly_sources

# These repositories have independent release schedules, not Kubernetes staging
# versions, so they are never folded into the aligned Kubernetes family.
INDEPENDENT = frozenset({
    "k8s.io/gengo", "k8s.io/gengo/v2", "k8s.io/klog", "k8s.io/klog/v2",
    "k8s.io/kube-openapi", "k8s.io/utils", "k8s.io/system-validators",
})
LIBRARY = Path("staging/src/github.com/kubernetes-csi/csi-lib-utils")
ROOT_MODULE = "github.com/kubernetes-csi/csi-sidecars"
LIB_MODULE = "github.com/kubernetes-csi/csi-lib-utils"
SEMVER = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)"
                    r"(?:-([0-9A-Za-z.-]+))?(?:\+incompatible)?")


def is_family(path):
    return path.startswith("k8s.io/") and path not in INDEPENDENT


def go(root, *args, workspace=True):
    env = {**os.environ, "GOTOOLCHAIN": "local", "GOFLAGS": "", "GOENV": "off",
           "GOSUMDB": "sum.golang.org", "GOPRIVATE": "", "GONOSUMDB": "",
           "GOWORK": str(root / "go.work") if workspace else "off"}
    return subprocess.check_output(["go", *args], cwd=root, env=env, text=True, timeout=300)


def source_documents(root):
    """Use Go's parser on retained original manifests, including snapshot client."""
    lock = assembly_sources.load(root / "tmp/sources.lock.json")
    documents = {}
    paths = {name: Path(f"tmp/external-{name}/pkg/{name}/go.mod")
             for name in assembly_sources.CONTROLLERS}
    paths["snapshotter/client"] = paths["snapshotter"].parent / "client/go.mod"
    paths["csi-lib-utils"] = LIBRARY / "go.mod"
    for name, path in paths.items():
        revision = lock["sources"][name.split("/")[0]]["commit"]
        label = f"{name}@{revision}"
        documents[label] = json.loads(go(root, "mod", "edit", "-json", str(root / path), workspace=False))
    return documents


def version_key(version):
    match = SEMVER.fullmatch(version)
    if not match:
        raise ValueError(f"unsupported module version: {version}")
    major, minor, patch, prerelease = match.groups()
    identifiers = tuple((0, int(item)) if item.isdigit() else (1, item)
                        for item in prerelease.split(".")) if prerelease else ()
    return (int(major), int(minor), int(patch), prerelease is None, identifiers)


def seed(root, kubernetes):
    """Generate go.mod/go.work by merging the original source requirements.

    Aligns every Kubernetes staging module to the explicitly selected release,
    keeps independent modules at their highest source minimum, and points the
    library module at its staged checkout.
    """
    if not re.fullmatch(r"1\.[0-9]+\.[0-9]+", kubernetes):
        raise ValueError("generation requires an explicit Kubernetes release, e.g. 1.36.3")
    documents = source_documents(root)
    merged, replacements, family = {}, {}, set()
    folded = {doc["Module"]["Path"] for label, doc in documents.items()
              if not label.startswith("csi-lib-utils@")}
    go_versions = []
    for label, document in documents.items():
        go_versions.append(tuple(map(int, document["Go"].split("."))))
        if document.get("Exclude") or document.get("Tool"):
            raise ValueError(f"{label}: unsupported exclude/tool directive; explicit adaptation required")
        for item in document.get("Require") or []:
            path, version = item["Path"], item["Version"]
            if path in folded:
                continue
            if is_family(path):
                family.add(path)
                version = "v" + (kubernetes if path == "k8s.io/kubernetes" else "0." + kubernetes[2:])
            if path not in merged or version_key(version) > version_key(merged[path]):
                merged[path] = version
        for item in document.get("Replace") or []:
            old, new = item["Old"], item["New"]
            path = old["Path"]
            if path in folded and new["Path"] == "./client":
                continue
            if is_family(path):
                if new["Path"] != path or not new.get("Version"):
                    raise ValueError(f"{label}: cannot align replacement for {path}")
                family.add(path)
            else:
                if not new.get("Version"):
                    raise ValueError(f"{label}: unsupported local replacement for {path}")
                key = (path, old.get("Version", ""))
                if key in replacements and replacements[key] != new:
                    raise ValueError(f"conflicting source replacements for {path}")
                replacements[key] = new
    go_version = ".".join(map(str, max(go_versions)))
    lines = [f"module {ROOT_MODULE}", "", f"go {go_version}", "", "require ("]
    lines += [f"\t{path} {version}" for path, version in sorted(merged.items())]
    lines += [")", ""]
    for path in sorted(family):
        version = "v" + (kubernetes if path == "k8s.io/kubernetes" else "0." + kubernetes[2:])
        lines.append(f"replace {path} => {path} {version}")
    for (path, version), new in sorted(replacements.items()):
        lines.append(f"replace {path} {version} => {new['Path']} {new['Version']}")
    lines.append(f"replace {LIB_MODULE} => ./{LIBRARY}")
    for name, content in (("go.mod", "\n".join(lines) + "\n"),
                          ("go.work", f"go {go_version}\n\nuse (\n\t.\n\t./{LIBRARY}\n)\n")):
        with (root / name).open("x") as output:
            output.write(content)
    go(root, "mod", "edit", "-fmt")
    go(root, "work", "edit", "-fmt")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path.cwd())
    parser.add_argument("--kubernetes", help="Kubernetes release to align go.mod generation on")
    parser.add_argument("phase", choices=("seed",))
    args = parser.parse_args()
    try:
        seed(args.root.resolve(), args.kubernetes or "")
        print("Generated go.mod and go.work from original source requirements")
    except (OSError, ValueError, KeyError, subprocess.SubprocessError) as error:
        print(f"Dependency compatibility failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
