#!/usr/bin/env python3
"""Prune gonk's own container images from the orac GitLab registry.

WHY THIS EXISTS (gonk-1t4, 2026-08-13): the registry PVC hit 100% and stopped
every image push cluster-wide (gonk-mzm). gonk was 183 GB of the 294 GB, and
agentic/gonk-project/cache -- the kaniko layer cache -- was 139 GB of that on
its own. .gitlab-ci.yml has since been fixed so it stops growing that fast; this
cleans up what already accumulated, and can be re-run later.

WHAT IT TOUCHES: only repositories under agentic/gonk-project. Nothing owned by
aether-fm, openclaw, recognizer or anyone else -- those have live deploys and
this script has no way to know which of their tags are load-bearing.

HOW RECLAIM ACTUALLY HAPPENS: this registry is GitLab Container Registry v4.38.0
with the metadata database enabled (database.enabled: true), so manifests and
tags live in Postgres, not on disk. Do NOT run the old `registry
garbage-collect` against it -- offline GC is unsupported in that mode. Online GC
is already running (gc.disabled: false); it simply had nothing to reclaim
because nothing had been untagged. Deleting tags is what feeds it. Disk
therefore frees up GRADUALLY over minutes-to-hours after this exits, not at the
moment it exits.

USAGE
    hack/registry_prune.py              # dry run: print what would be deleted
    hack/registry_prune.py --apply      # actually delete

Requires glab, authenticated against gitlab.orac.local (`glab auth status`).
"""

import argparse
import json
import re
import subprocess
import sys

PROJECT = "agentic%2Fgonk-project"
DEFAULT_HELMRELEASE = "../gitops/clusters/orac/apps/gonk/helmrelease-gonk.yaml"
TAG_RE = re.compile(r"v[0-9]+\.[0-9]+\.[0-9]+-[0-9a-f]{12}")
ARCH_SUFFIX_RE = re.compile(r"-(amd64|arm64)$")


def glab(path, method="GET"):
    """Call the GitLab API through glab and return parsed JSON (None on error)."""
    cmd = ["glab", "api"]
    if method != "GET":
        cmd += ["--method", method]
    cmd.append(path)
    proc = subprocess.run(cmd, capture_output=True, text=True)
    if proc.returncode != 0:
        return None
    out = proc.stdout.strip()
    if not out:
        return {}
    try:
        return json.loads(out)
    except json.JSONDecodeError:
        return None


def glab_paged(path):
    """Page through a list endpoint explicitly.

    NOT `glab api --paginate`: that concatenates one JSON array per page, which
    json.loads cannot parse. gonk-meter has 391 tags, so this is not theoretical
    -- it would have silently failed on the largest repository.
    """
    items, page = [], 1
    sep = "&" if "?" in path else "?"
    while True:
        batch = glab(f"{path}{sep}per_page=100&page={page}")
        if not batch:
            break
        items.extend(batch)
        if len(batch) < 100:
            break
        page += 1
    return items


def base_tag(name):
    """The build group a tag belongs to: v0.1.0-abc-amd64 -> v0.1.0-abc."""
    return ARCH_SUFFIX_RE.sub("", name)


def deployed_tag(helmrelease_path, override):
    if override:
        return override
    try:
        with open(helmrelease_path) as fh:
            found = sorted(set(TAG_RE.findall(fh.read())))
    except OSError:
        return None
    return found[0] if len(found) == 1 else (found[0] if found else None)


def plan_for_repo(rid, rpath, deployed, keep_groups):
    """Return (keep, delete) tag-name lists for one repository, newest first."""
    names = [t["name"] for t in glab_paged(
        f"projects/{PROJECT}/registry/repositories/{rid}/tags")]

    # created_at is not in the list payload, so it costs one GET per tag.
    dated = []
    for n in names:
        detail = glab(
            f"projects/{PROJECT}/registry/repositories/{rid}/tags/{n}") or {}
        dated.append((detail.get("created_at") or "", n))
    dated.sort(reverse=True)

    seen, order = set(), []
    for _, n in dated:
        b = base_tag(n)
        if b not in seen:
            seen.add(b)
            order.append(b)

    keepset = set(order[:keep_groups]) | {deployed}

    def protected(name):
        # Keep the recent build groups, and keep EVERY tag carrying the deployed
        # sha -- not just its build group. gonk-meter also publishes
        # <tag>-testclock (and its two arch children), which base_tag does not
        # fold into the deployed group because -testclock is not an arch suffix.
        # Nothing in gitops pins it, so deleting it would probably be harmless,
        # and "probably harmless" is not worth three tags of disk.
        return base_tag(name) in keepset or name.startswith(deployed)

    keep = [n for _, n in dated if protected(n)]
    delete = [n for _, n in dated if not protected(n)]

    # Hard stop. Removing what Flux has pinned is how you unschedule production.
    if any(n.startswith(deployed) for n in delete):
        raise SystemExit(
            f"REFUSING: {rpath} delete set contains the deployed tag {deployed}")
    return keep, delete


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--apply", action="store_true",
                    help="actually delete (default is a dry run)")
    ap.add_argument("--keep-groups", type=int, default=5,
                    help="recent build groups to keep per image repo (default 5)")
    ap.add_argument("--helmrelease", default=DEFAULT_HELMRELEASE)
    ap.add_argument("--deployed-tag", default=None,
                    help="override the deployed tag read from the HelmRelease")
    ap.add_argument("--keep-cache", action="store_true",
                    help="do not delete the kaniko cache repository")
    args = ap.parse_args()

    deployed = deployed_tag(args.helmrelease, args.deployed_tag)
    if not deployed:
        print("REFUSING TO RUN: could not determine the deployed tag from "
              f"{args.helmrelease}.\nDeleting tags without knowing what is "
              "deployed is how you unschedule production.\nPass --deployed-tag "
              "explicitly, or point --helmrelease at the right file.",
              file=sys.stderr)
        return 1

    mode = "APPLY" if args.apply else "DRY RUN"
    print(f"[{mode}] protecting deployed tag {deployed}; "
          f"keeping {args.keep_groups} newest build groups per repo\n")

    repos = glab_paged(f"projects/{PROJECT}/registry/repositories")
    if not repos:
        print("no container repositories found for the project", file=sys.stderr)
        return 1

    deleted_tags = 0
    for repo in sorted(repos, key=lambda r: r["path"]):
        rid, rpath = repo["id"], repo["path"]

        if rpath.endswith("/cache"):
            # Every tag here is a content-addressed kaniko cache entry. Nothing
            # deploys from it and kaniko rebuilds what it needs, so there is no
            # keep-set to reason about -- the whole repository goes.
            if args.keep_cache:
                print(f"{rpath}: skipped (--keep-cache)")
                continue
            if args.apply:
                print(f"{rpath}: DELETING whole repository (id={rid})")
                glab(f"projects/{PROJECT}/registry/repositories/{rid}",
                     method="DELETE")
            else:
                print(f"{rpath}: would DELETE whole repository (id={rid})")
            continue

        keep, delete = plan_for_repo(rid, rpath, deployed, args.keep_groups)
        print(f"{rpath}: keep {len(keep)}, delete {len(delete)}")
        for n in delete:
            if args.apply:
                glab(f"projects/{PROJECT}/registry/repositories/{rid}/tags/{n}",
                     method="DELETE")
                deleted_tags += 1
            else:
                print(f"    would delete {n}")

    print()
    if args.apply:
        print(f"Done: {deleted_tags} tags deleted. Online GC now dereferences "
              "the untagged manifests and frees blobs.\nWatch it drain with:\n"
              "  kubectl -n gitlab exec deploy/gitlab-registry -c registry -- "
              "df -h /var/lib/registry")
    else:
        print("DRY RUN -- nothing was deleted. Re-run with --apply to do it.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
