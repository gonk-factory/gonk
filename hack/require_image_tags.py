#!/usr/bin/env python3
"""Assert that image tags EXIST in the registry before anything deploys them.

WHY THIS EXISTS (2026-09-07). A tag bump in gitops went out against images that
had never been built, and meter and controller went into ImagePullBackOff. The
bump was not careless -- it was "verified" first. The check was:

    glab api .../registry/repositories/<id>/tags/<tag> | python3 -c "json.load(...)"

treating any parseable JSON as success. GitLab answers a missing tag with
{"message":"404 Tag Not Found"}, which parses perfectly. So it confirmed THE API
REPLIED, not that the image existed, and reported all four images present when
none were. Earlier bumps had passed the same check only because those tags
happened to exist.

The lesson generalises past this one script: an API check must assert on a FIELD
IT EXPECTS, never on the response being well-formed. That is what this encodes.

    hack/require_image_tags.py v0.1.0-1ef548b06598
    hack/require_image_tags.py --tag-from-git        # tag of the current HEAD

Exits 0 only when every gonk image carries the tag. Anything else is non-zero
and says which are missing.
"""
import json
import subprocess
import sys

PROJECT = 69
# repository id -> image name, from `glab api projects/69/registry/repositories`
REPOS = {
    98: "gonk-controller",
    101: "gonk-intake",
    102: "gonk-meter",
    103: "gonk-agent",
}


def glab(path):
    out = subprocess.run(
        ["glab", "api", path], capture_output=True, text=True
    )
    if out.returncode != 0:
        # glab writes unrelated warnings to stderr on this box (a snap/dbus
        # complaint); keep only the line that says what actually went wrong, or
        # the message is longer than the report it appears in.
        lines = [
            ln.strip()
            for ln in out.stderr.splitlines()
            if ln.strip() and "cannot start document portal" not in ln
        ]
        return None, lines[-1] if lines else f"glab exited {out.returncode}"
    try:
        return json.loads(out.stdout), None
    except json.JSONDecodeError as exc:
        return None, f"response was not JSON: {exc}"


def tag_present(repo_id, tag):
    """True only when the response is a tag object naming the tag we asked for.

    NOT "the call returned JSON". A 404 body is JSON.
    """
    body, err = glab(f"projects/{PROJECT}/registry/repositories/{repo_id}/tags/{tag}")
    if err is not None:
        return False, err
    if not isinstance(body, dict):
        return False, f"expected an object, got {type(body).__name__}"
    if body.get("name") != tag:
        return False, body.get("message") or f"no `name` field (keys: {sorted(body)[:6]})"
    return True, None


def head_tag():
    sha = subprocess.run(
        ["git", "rev-parse", "HEAD"], capture_output=True, text=True, check=True
    ).stdout.strip()[:12]
    version = "0.1.0"
    try:
        with open("images/versions.env", encoding="utf-8") as fh:
            for line in fh:
                if line.startswith("GONK_VERSION="):
                    version = line.split("=", 1)[1].strip()
                    break
    except OSError:
        pass
    return f"{version}-{sha}"


def main(argv):
    if not argv or argv[0] == "--tag-from-git":
        tag = head_tag()
    else:
        tag = argv[0]

    print(f"requiring tag {tag} on {len(REPOS)} images")
    missing = []
    for repo_id, name in sorted(REPOS.items(), key=lambda kv: kv[1]):
        ok, why = tag_present(repo_id, tag)
        print(f"  {'PRESENT' if ok else 'ABSENT ':8} {name}" + ("" if ok else f"  <- {why}"))
        if not ok:
            missing.append(name)

    if missing:
        print(
            f"\n{len(missing)} image(s) missing: {', '.join(missing)}."
            "\nDo NOT bump the gitops tag: the pods will ImagePullBackOff."
            "\nCheck the pipeline actually built them --"
            " a green `lint` is not a built image.",
            file=sys.stderr,
        )
        return 1
    print("\nall present; safe to bump the gitops tag")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
