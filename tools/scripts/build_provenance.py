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

"""Verify inherited release-tools and root build inputs without executing them."""

import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import stat
import subprocess

from assembly_sources import unique_object

ROOT = Path(__file__).resolve().parents[2]
LOCK = "tools/assembly/build-provenance.lock.json"
REPOSITORY = "https://github.com/kubernetes-csi/csi-release-tools"
ROOT_FILES = ("Makefile", "Dockerfile", ".prow.sh", ".cloudbuild.sh")
CMDS = "CMDS=csi-sidecars snapshot-controller snapshot-conversion-webhook"


def encoded(value):
    return (json.dumps(value, sort_keys=True, indent=2) + "\n").encode()


def digest(value, length):
    return isinstance(value, str) and re.fullmatch(r"[0-9a-f]{%d}" % length, value)


def name_checked(name):
    if (not isinstance(name, str) or not name or "\\" in name or
            any(part in ("", ".", "..", ".git") for part in name.split("/")) or
            any(ord(c) < 32 for c in name)):
        raise ValueError(f"invalid provenance path: {name!r}")
    return name


def link_checked(name, target):
    # Retain the upstream OWNERS_ALIASES link, but never dereference it or permit
    # directory traversal, absolute links, or links outside this inventory.
    if not isinstance(target, str):
        raise ValueError(f"invalid link target: {name}")
    name_checked(target)
    return str(PurePosixPath(name).parent / target)


def entry(mode, data):
    value = {"mode": mode, "sha256": hashlib.sha256(data).hexdigest()}
    if mode == "120000":
        value["target"] = data.decode("utf-8")
    return value


def check_inventory(files):
    if not isinstance(files, dict) or not files:
        raise ValueError("empty or invalid provenance inventory")
    for name, item in files.items():
        name_checked(name)
        if not isinstance(item, dict) or item.get("mode") not in ("100644", "100755", "120000"):
            raise ValueError(f"invalid provenance entry: {name}")
        fields = {"mode", "sha256", "target"} if item["mode"] == "120000" else {"mode", "sha256"}
        if set(item) != fields or not digest(item["sha256"], 64):
            raise ValueError(f"invalid provenance checksum: {name}")
    for name, item in files.items():
        if item["mode"] == "120000":
            target = link_checked(name, item["target"])
            if (target not in files or files[target].get("mode") == "120000" or
                    entry("120000", item["target"].encode()) != item):
                raise ValueError(f"invalid provenance link: {name}")
        if any(str(parent) in files for parent in PurePosixPath(name).parents if str(parent) != "."):
            raise ValueError(f"file/directory collision: {name}")
    return files


def file_entry(path):
    mode = path.lstat().st_mode
    if stat.S_ISLNK(mode):
        return entry("120000", os.readlink(path).encode())
    if not stat.S_ISREG(mode):
        raise ValueError(f"not a regular provenance file: {path}")
    return entry("100755" if mode & 0o111 else "100644", path.read_bytes())


def local_files(root):
    tree = root / "release-tools"
    if tree.is_symlink() or not tree.is_dir():
        raise ValueError("release-tools must be a real directory")
    files = {p.relative_to(tree).as_posix(): file_entry(p) for p in tree.rglob("*")
             if p.is_symlink() or not p.is_dir()}
    return check_inventory(files)


def root_files(root):
    names = {path.name for path in root.iterdir()}
    for name in ("GNUmakefile", "makefile"):
        if name in names:
            raise ValueError(f"unexpected Makefile override: {name}")
    files = {name: file_entry(root / name) for name in ROOT_FILES}
    if any(item["mode"] == "120000" for item in files.values()):
        raise ValueError("root build files must not be symlinks")
    if [line for line in (root / "Makefile").read_text().splitlines() if line.startswith("CMDS=")] != [CMDS]:
        raise ValueError("maintained Makefile must select exactly the three release commands")
    return files


def expected_files(lock):
    files = dict(lock["upstream"]["files"])
    for name, change in lock["local_changes"].items():
        if change["entry"] is None:
            files.pop(name)
        else:
            files[name] = change["entry"]
    return check_inventory(files)


def validate(lock):
    if (not isinstance(lock, dict) or
            set(lock) != {"schema_version", "upstream", "local_changes", "root_files"} or
            type(lock["schema_version"]) is not int or lock["schema_version"] != 1):
        raise ValueError("invalid build provenance schema")
    upstream = lock["upstream"]
    if (not isinstance(upstream, dict) or
            set(upstream) != {"repository", "commit", "tree", "project_import_commit", "files"} or
            upstream["repository"] != REPOSITORY or
            any(not digest(upstream[key], 40) for key in ("commit", "tree", "project_import_commit"))):
        raise ValueError("invalid release-tools upstream identity")
    check_inventory(upstream["files"])
    if not isinstance(lock["local_changes"], dict):
        raise ValueError("invalid local changes")
    for name, change in lock["local_changes"].items():
        name_checked(name)
        if (not isinstance(change, dict) or set(change) != {"entry", "reason"} or
                not isinstance(change["reason"], str) or not change["reason"].strip() or
                change["entry"] == upstream["files"].get(name)):
            raise ValueError(f"invalid or redundant local change: {name}")
    expected_files(lock)
    check_inventory(lock["root_files"])
    if (set(lock["root_files"]) != set(ROOT_FILES) or
            any(item["mode"] == "120000" for item in lock["root_files"].values())):
        raise ValueError("invalid root build inventory")
    return lock


def load(root):
    path = root / LOCK
    for parent in (path, *path.parents):
        if parent == root:
            break
        if parent.is_symlink():
            raise ValueError("symlink in build provenance lock path")
    return validate(json.loads(path.read_text(), object_pairs_hook=unique_object))


def compare(expected, actual, label):
    changed = sorted(name for name in expected.keys() | actual.keys() if expected.get(name) != actual.get(name))
    if changed:
        raise ValueError(f"{label} provenance mismatch: {', '.join(changed)}")


def verify(root):
    lock = load(root)
    compare(expected_files(lock), local_files(root), "release-tools")
    compare(lock["root_files"], root_files(root), "root build")
    return lock


def git(repository, *args):
    # Read objects only: do not check out, run upstream scripts, or edit Git state.
    return subprocess.check_output(["git", "--no-replace-objects", "-C", str(repository), *args], timeout=60)


def upstream_files(repository, commit):
    if not digest(commit, 40) or git(repository, "rev-parse", commit + "^{commit}").decode().strip() != commit:
        raise ValueError("expected an exact upstream commit")
    tree = git(repository, "rev-parse", commit + "^{tree}").decode().strip()
    files = {}
    for record in git(repository, "ls-tree", "-rz", commit).split(b"\0"):
        if not record:
            continue
        metadata, raw_name = record.split(b"\t", 1)
        mode, kind, blob = metadata.decode().split()
        name = name_checked(raw_name.decode())
        if kind != "blob" or mode not in ("100644", "100755", "120000"):
            raise ValueError(f"unsupported upstream entry: {name}")
        files[name] = entry(mode, git(repository, "cat-file", "blob", blob))
    return tree, check_inventory(files)


def verify_upstream(root, lock, repository):
    upstream = lock["upstream"]
    tree, files = upstream_files(repository, upstream["commit"])
    if tree != upstream["tree"] or files != upstream["files"]:
        raise ValueError("upstream Git objects differ from recorded release-tools provenance")
    try:
        imported = git(root, "rev-parse", upstream["project_import_commit"] + ":release-tools").decode().strip()
    except subprocess.CalledProcessError as error:
        raise ValueError("project import is missing its release-tools tree") from error
    if imported != tree:
        raise ValueError("project import does not match the original upstream tree")


def candidate(root, repository, commit, imported, reasons):
    tree, files = upstream_files(repository, commit)
    local = local_files(root)
    changed = {name for name in files.keys() | local.keys() if files.get(name) != local.get(name)}
    if set(reasons) != changed:
        raise ValueError(f"explicit reasons required for exactly these local changes: {sorted(changed)}")
    lock = validate({"schema_version": 1,
                     "upstream": {"repository": REPOSITORY, "commit": commit, "tree": tree,
                                  "project_import_commit": imported, "files": files},
                     "local_changes": {name: {"entry": local.get(name), "reason": reasons[name]} for name in changed},
                     "root_files": root_files(root)})
    verify_upstream(root, lock, repository)
    return lock


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("phase", choices=("verify", "candidate"))
    parser.add_argument("--root", type=Path, default=ROOT)
    parser.add_argument("--upstream-repository", type=Path, help="Already fetched Git objects; never fetched implicitly")
    parser.add_argument("--commit")
    parser.add_argument("--import-commit")
    parser.add_argument("--local-change", action="append", default=[], metavar="PATH=REASON")
    args = parser.parse_args()
    if args.phase == "candidate":
        if not args.upstream_repository or not args.commit or not args.import_commit:
            parser.error("candidate requires upstream repository, exact commit, and project import commit")
        reasons = {}
        for change in args.local_change:
            name, separator, reason = change.partition("=")
            if not separator or name in reasons:
                parser.error("local changes require unique PATH=REASON entries")
            reasons[name] = reason
        # Reviewable output only; this command never replaces the active lock/tree.
        print(encoded(candidate(args.root, args.upstream_repository, args.commit, args.import_commit, reasons)).decode(), end="")
    else:
        if args.commit or args.import_commit or args.local_change:
            parser.error("selection arguments apply only to candidate generation")
        lock = verify(args.root)
        if args.upstream_repository:
            verify_upstream(args.root, lock, args.upstream_repository)
        print("Root build and release-tools provenance verified")


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError) as error:
        raise SystemExit(f"Build provenance failed: {error}") from error
