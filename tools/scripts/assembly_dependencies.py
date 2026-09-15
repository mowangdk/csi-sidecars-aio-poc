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

"""Reject Kubernetes family drift and replacement downgrades before building."""

import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys

import assembly_sources

# These repositories have independent release schedules, not Kubernetes staging
# versions. Unknown k8s.io modules are checked, never silently excluded.
INDEPENDENT = frozenset({
    "k8s.io/gengo", "k8s.io/gengo/v2", "k8s.io/klog", "k8s.io/klog/v2",
    "k8s.io/kube-openapi", "k8s.io/utils", "k8s.io/system-validators",
})
CORE = frozenset({"k8s.io/api", "k8s.io/apimachinery", "k8s.io/client-go"})
LIBRARY = Path("staging/src/github.com/kubernetes-csi/csi-lib-utils")
VERSION = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)")


def is_family(path):
    return path.startswith("k8s.io/") and path not in INDEPENDENT


def release(path, version):
    """Map k8s.io/kubernetes v1.X.Y and staging v0.X.Y to one release."""
    match = VERSION.fullmatch(version)
    if not match:
        raise ValueError(f"{path}: unsupported Kubernetes release {version!r}")
    major, minor, patch = map(int, match.groups())
    if major != (1 if path == "k8s.io/kubernetes" else 0) or minor == 0:
        raise ValueError(f"{path}: invalid Kubernetes release {version!r}")
    return minor, patch


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


def source_requirements(documents):
    requirements = []
    for label, document in documents.items():
        seen = set()
        for item in document.get("Require") or []:
            path, version = item["Path"], item["Version"]
            if not is_family(path):
                continue
            if path in seen:
                raise ValueError(f"{label}: duplicate requirement {path}")
            seen.add(path)
            # Kubernetes' staging references are placeholders. Their effective
            # replacements must still pass the resolved-graph check.
            if version == "v0.0.0" and (path in CORE or path == "k8s.io/kubernetes"):
                raise ValueError(f"{label}: {path} must declare a concrete Kubernetes release")
            if version != "v0.0.0":
                requirements.append((label, path, version, release(path, version)))
        if not CORE.issubset(seen):
            raise ValueError(f"{label}: missing core Kubernetes requirements: {sorted(CORE - seen)}")
    if not requirements:
        raise ValueError("no original Kubernetes requirements found")
    return requirements


def validate_sources(documents):
    requirements = source_requirements(documents)
    minors = {value[3][0] for value in requirements if value[1] in CORE}
    if len(minors) != 1:
        details = sorted({f"{label}: {path} requires {version}"
                          for label, path, version, _ in requirements if path in CORE})
        raise ValueError("original sources require different Kubernetes minors; select a compatible "
                         "source candidate (do not force a replace downgrade):\n  " + "\n  ".join(details))
    return requirements


def json_stream(text):
    decoder = json.JSONDecoder(object_pairs_hook=assembly_sources.unique_object)
    values = []
    while text.strip():
        value, end = decoder.raw_decode(text.lstrip())
        if not isinstance(value, dict) or value.get("Error"):
            raise ValueError(f"invalid resolved module: {value}")
        values.append(value)
        text = text.lstrip()[end:]
    if not values:
        raise ValueError("empty resolved module graph")
    return values


def vendored_paths(text):
    """Select module headers, not package lines or versionless replace trailers."""
    headers = set()
    for line in text.splitlines():
        fields = line.split()
        if len(fields) >= 3 and fields[0] == "#" and fields[2].startswith("v"):
            header = (fields[1], fields[2])
            if header in headers:
                raise ValueError("duplicate vendored module/version header")
            headers.add(header)
    if not headers:
        raise ValueError("empty vendored module inventory")
    # Workspace members may require different versions of one module. Go emits
    # explicit-only headers for the lower requirements as well as the selected
    # version. Ask Go once per path for the effective build-list version; never
    # treat those requirement-only headers as additional compiled modules.
    return sorted({path for path, _ in headers})


def resolved_modules(root, vendored=False):
    if vendored:
        paths = vendored_paths((root / "vendor/modules.txt").read_text())
        return json_stream(go(root, "list", "-m", "-mod=vendor", "-json", *paths))
    return json_stream(go(root, "list", "-m", "-mod=readonly", "-json", "all"))


def validate_graph(modules, documents):
    """Check effective bytes' versions, not just the pre-replacement labels."""
    requirements = source_requirements(documents)
    effective = {}
    errors = []
    for module in modules:
        path = module["Path"]
        if not is_family(path):
            continue
        if path in effective:
            raise ValueError(f"duplicate resolved module {path}")
        replacement = module.get("Replace", module)
        if replacement["Path"] != path or not replacement.get("Version"):
            raise ValueError(f"{path}: unversioned/local or alternate-module replacement is not allowed")
        version = replacement["Version"]
        value = release(path, version)
        effective[path] = (version, value)
        selected = module.get("Version", "")
        if (selected != "v0.0.0" or path == "k8s.io/kubernetes") and release(path, selected) > value:
            errors.append(f"resolved graph: {path} selected {selected} but replaces it with older {version}")
    if not CORE.issubset(effective):
        raise ValueError(f"resolved graph missing core modules: {sorted(CORE - effective.keys())}")
    target = effective["k8s.io/api"][1]
    for path, (version, value) in sorted(effective.items()):
        if value != target:
            errors.append(f"resolved graph: {path} uses {version}; expected Kubernetes 1.{target[0]}.{target[1]}")
    for label, path, version, value in requirements:
        # Some source/test requirements need no packages in the assembled graph.
        # Core clients must target the same minor; other modules may have older
        # minimum requirements. No module may require more than we provide.
        if (path in CORE and value[0] != target[0]) or value > target:
            errors.append(f"{label}: {path} requires {version}; assembled family is "
                          f"Kubernetes 1.{target[0]}.{target[1]}")
    if errors:
        raise ValueError("incompatible Kubernetes dependency graph:\n  " + "\n  ".join(errors))
    return {path: version for path, (version, _) in sorted(effective.items())}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path.cwd())
    parser.add_argument("phase", choices=("sources", "graph", "vendor"))
    args = parser.parse_args()
    root = args.root.resolve()
    try:
        documents = source_documents(root)
        if args.phase == "sources":
            validate_sources(documents)
            print("Original source Kubernetes minor requirements agree")
        else:
            versions = validate_graph(resolved_modules(root, args.phase == "vendor"), documents)
            print(json.dumps({"schema_version": 1, "kubernetes_modules": versions}, indent=2, sort_keys=True))
    except (OSError, ValueError, KeyError, subprocess.SubprocessError) as error:
        print(f"Dependency compatibility failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
