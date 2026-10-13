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

    hack/require_image_tags.py v0.1.0-337.c0fe5bcb2c0c

Exits 0 only when every gonk image carries the tag. Anything else is non-zero
and says which are missing.

--tag-from-git IS GONE (gonk-7ywn). It reconstructed $GONK_VERSION-<sha12>
from git, which stopped being the tag a DEFAULT-BRANCH build publishes when
0f13d63 split the shapes:

    default branch   $GONK_VERSION-$CI_PIPELINE_IID.<sha12>   v0.1.0-337.c0fe5bcb2c0c
    any other ref    $GONK_VERSION-<sha12>                    v0.1.0-c0fe5bcb2c0c

and gitops pins default-branch builds, so the case the flag was used for is
exactly the case it got wrong. It failed loudly -- all four images ABSENT --
but it lied about WHY, pointing at a pipeline that had not built instead of at
a tag that was never going to exist.

IT CANNOT BE FIXED FROM GIT. The pipeline IID is assigned by GitLab when the
pipeline is created and is recorded nowhere in the repository; no amount of
`git rev-parse` reaches it. Recovering it would mean a SECOND API call
(projects/<project>/pipelines?sha=...) picking one pipeline out of however many
ran for that commit -- retries, merge-request pipelines, manual runs -- and then
asserting against a tag nobody is deploying while printing "all present". That
is a new instance of exactly the trust-the-API-reply failure this script exists
to prevent, so it is not worth having.

PASS THE TAG. Read it from the pipeline that built the images: every image job
of the GitLab pipeline echoes GONK_TAG=... as its first line. Or ask the
registry what it has:

    glab api "projects/<project>/registry/repositories/<id>/tags?per_page=20" |
      python3 -c 'import json,sys;[print(t["name"]) for t in json.load(sys.stdin)]'

WHICH REGISTRY IS CONFIGURATION (gonk-qzkq). This script holds no project id
and no repository id. It reads them from a SITE FILE:

    {"project": 123,
     "repositories": {"gonk-controller": 1, "gonk-intake": 2,
                      "gonk-meter": 3, "gonk-agent": 4}}

`project` is the GitLab project that owns the container registry: its numeric
id, or its path with namespace. `repositories` maps each image that must carry
the tag to its registry repository id (`glab api
projects/<project>/registry/repositories` lists them). The file is
require_image_tags.site.json beside this script, or the path in
$GONK_IMAGE_REGISTRY_SITE. The maintainers' own site file is not published;
without one the script refuses to run rather than ask about a project it made
up. It talks to whatever GitLab host `glab` is logged into, so check
`glab auth status` first. It checks a GitLab container registry only.
"""
import json
import os
import subprocess
import sys
import urllib.parse

SITE_ENV = "GONK_IMAGE_REGISTRY_SITE"
DEFAULT_SITE = os.path.join(
    os.path.dirname(os.path.abspath(__file__)), "require_image_tags.site.json")


class SiteError(Exception):
    """The site file is missing or does not say which registry to ask."""


def site_path():
    """The site file to read, and whether the environment named it."""
    named = os.environ.get(SITE_ENV, "")
    return (named, True) if named else (DEFAULT_SITE, False)


def load_site(path, named):
    """Return (project, {image name: repository id}) from the site file.

    Everything that is not exactly that is refused. In particular an EMPTY
    repository map is refused: the caller loops over it, and zero iterations
    would end in "all present".
    """
    try:
        with open(path, encoding="utf-8") as fh:
            text = fh.read()
    except OSError as exc:
        if named:
            raise SiteError(f"the site file {path} (from ${SITE_ENV}) cannot be read: {exc.strerror}")
        raise SiteError(
            f"no site file: {path} does not exist and ${SITE_ENV} is not set.\n"
            "This script holds no registry of its own. Write a site file naming the\n"
            "GitLab project and the registry repository id of each image (the format\n"
            f"is in this script's docstring), and put it there or point ${SITE_ENV} at it.")
    try:
        site = json.loads(text)
    except json.JSONDecodeError as exc:
        raise SiteError(f"the site file {path} is not JSON: {exc}")
    if not isinstance(site, dict):
        raise SiteError(f"the site file {path} must hold a JSON object")
    project = site.get("project")
    is_id = isinstance(project, int) and not isinstance(project, bool) and project > 0
    is_path = isinstance(project, str) and project.strip() != ""
    if not (is_id or is_path):
        raise SiteError(
            f"the site file {path}: `project` must be a GitLab project id or its path with namespace")
    repos = site.get("repositories")
    if not isinstance(repos, dict) or not repos:
        raise SiteError(
            f"the site file {path}: `repositories` must map at least one image name to its registry repository id")
    for name, repo_id in repos.items():
        if not name.strip():
            raise SiteError(f"the site file {path}: an image in `repositories` has an empty name")
        if not isinstance(repo_id, int) or isinstance(repo_id, bool) or repo_id <= 0:
            raise SiteError(
                f"the site file {path}: repository id of {name!r} must be a positive integer, got {repo_id!r}")
    if is_path:
        project = urllib.parse.quote(project.strip(), safe="")
    return project, dict(repos)


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


def tag_present(project, repo_id, tag):
    """True only when the response is a tag object naming the tag we asked for.

    NOT "the call returned JSON". A 404 body is JSON.
    """
    body, err = glab(f"projects/{project}/registry/repositories/{repo_id}/tags/{tag}")
    if err is not None:
        return False, err
    if not isinstance(body, dict):
        return False, f"expected an object, got {type(body).__name__}"
    if body.get("name") != tag:
        return False, body.get("message") or f"no `name` field (keys: {sorted(body)[:6]})"
    return True, None


USAGE = """usage: require_image_tags.py <tag>

Give the EXACT tag, e.g. v0.1.0-337.c0fe5bcb2c0c (default branch) or
v0.1.0-c0fe5bcb2c0c (any other ref). See the module docstring for where to
read it from; there is no way to derive it from a git checkout, because the
pipeline IID in a default-branch tag exists only in GitLab.

Which registry is asked comes from a site file: require_image_tags.site.json
beside this script, or the path in $GONK_IMAGE_REGISTRY_SITE."""

# --tag-from-git is still RECOGNISED so it produces this explanation rather
# than being taken for a literal tag name and reported as four absent images,
# which is a worse lie than the one it used to tell.
REMOVED_FLAG_MESSAGE = """--tag-from-git was removed (gonk-7ywn).

It rebuilt $GONK_VERSION-<sha12> from git. Since 0f13d63 that is NOT the tag a
default-branch build publishes -- those are $GONK_VERSION-<iid>.<sha12>, and
the pipeline IID is assigned by GitLab and recorded nowhere in the repo, so no
git command can reach it. Against a main build the flag reported all four
images ABSENT and named the wrong reason.

""" + USAGE


def main(argv):
    if not argv or argv[0] in ("-h", "--help"):
        print(USAGE, file=sys.stderr)
        return 2
    if argv[0] == "--tag-from-git":
        print(REMOVED_FLAG_MESSAGE, file=sys.stderr)
        return 2
    tag = argv[0]
    try:
        project, repos = load_site(*site_path())
    except SiteError as exc:
        print(f"require_image_tags.py: {exc}", file=sys.stderr)
        return 2

    print(f"requiring tag {tag} on {len(repos)} images")
    missing = []
    for name, repo_id in sorted(repos.items()):
        ok, why = tag_present(project, repo_id, tag)
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
