"""Offline checks for the image-tag guard: no network, no glab.

The thing under test is the assertion CLAUDE.md singles out as the guard
against the 2026-09-07 ImagePullBackOff outage -- that a tag counts as present
only when the response object's `name` equals the tag asked for, never when the
call merely returned parseable JSON. GitLab answers a missing tag with
{"message": "404 Tag Not Found"}, which parses perfectly; that body is a test
case below, not a hypothetical.

Run with `pytest hack/test_require_image_tags.py`, or directly with
`python3 hack/test_require_image_tags.py`.

NOTE, AND IT IS A GAP: no CI job in this repo runs any Python test -- see
gonk-5ed1. These assertions are only as good as somebody remembering them.
"""
import importlib.util
import io
import json
import os
import tempfile
from contextlib import redirect_stderr, redirect_stdout
from unittest import mock

_HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location(
    "rit", os.path.join(_HERE, "require_image_tags.py"))
rit = importlib.util.module_from_spec(spec)
spec.loader.exec_module(rit)

GOOD_TAG = "v0.1.0-337.c0fe5bcb2c0c"

# A site of the tests' own. The script carries NO project or repository id
# (gonk-qzkq): which registry it asks is configuration, so these tests run the
# same in a tree that has no site file at all.
SITE = {
    "project": 7,
    "repositories": {
        "gonk-agent": 14,
        "gonk-controller": 11,
        "gonk-intake": 12,
        "gonk-meter": 13,
    },
}


def _site_file(content):
    """Write a site file and return its path. content is a dict (dumped as
    JSON) or a str (written as is)."""
    fd, path = tempfile.mkstemp(suffix=".site.json")
    with os.fdopen(fd, "w") as fh:
        fh.write(content if isinstance(content, str) else json.dumps(content))
    return path


def _run(argv, glab_response, site=SITE, env=None):
    """Run main() with rit.glab stubbed, returning (exit code, stdout+stderr).

    site is what the site file holds; None means there is NO site file at the
    default path. env replaces the script's view of the environment."""
    out, err = io.StringIO(), io.StringIO()
    default = (_site_file(site) if site is not None
               else os.path.join(tempfile.mkdtemp(), "absent.site.json"))
    try:
        with mock.patch.object(rit, "glab", side_effect=glab_response), \
                mock.patch.object(rit, "DEFAULT_SITE", default), \
                mock.patch.dict(os.environ, env or {}, clear=True):
            with redirect_stdout(out), redirect_stderr(err):
                code = rit.main(argv)
    finally:
        if site is not None:
            os.unlink(default)
    return code, out.getvalue() + err.getvalue()


# ---------------------------------------------------------------------------
# The core assertion. Do not weaken these.
# ---------------------------------------------------------------------------

def test_a_404_body_that_parses_perfectly_is_not_a_present_tag():
    """THE OUTAGE, as a test. Any check that asked only "did this parse" passed
    on this exact body and reported four images present when none existed."""
    with mock.patch.object(
            rit, "glab",
            return_value=({"message": "404 Tag Not Found"}, None)):
        ok, why = rit.tag_present(7, 11, GOOD_TAG)
    assert ok is False
    assert why == "404 Tag Not Found"


def test_a_tag_object_naming_a_different_tag_is_not_present():
    """The other direction: a well-formed tag object is not enough either. It
    has to be the tag that was ASKED for."""
    with mock.patch.object(
            rit, "glab",
            return_value=({"name": "v0.1.0-336.b1b1b1b1b1b1"}, None)):
        ok, _ = rit.tag_present(7, 11, GOOD_TAG)
    assert ok is False


def test_the_only_thing_that_counts_as_present_is_a_matching_name():
    with mock.patch.object(rit, "glab", return_value=({"name": GOOD_TAG}, None)):
        ok, why = rit.tag_present(7, 11, GOOD_TAG)
    assert ok is True and why is None


def test_a_json_array_is_not_a_tag_object():
    with mock.patch.object(rit, "glab", return_value=([{"name": GOOD_TAG}], None)):
        ok, why = rit.tag_present(7, 11, GOOD_TAG)
    assert ok is False
    assert "expected an object" in why


def test_a_transport_error_is_not_a_present_tag():
    with mock.patch.object(rit, "glab", return_value=(None, "connection refused")):
        ok, why = rit.tag_present(7, 11, GOOD_TAG)
    assert ok is False and why == "connection refused"


# ---------------------------------------------------------------------------
# The tag shape is not the script's business -- it asserts existence, not form.
# ---------------------------------------------------------------------------

def test_both_tag_shapes_are_accepted_verbatim():
    """The positional path is format-agnostic and must stay that way: the
    default branch publishes v<ver>-<iid>.<sha12> and every other ref publishes
    v<ver>-<sha12>, and both are pullable and deployable by hand."""
    for tag in (GOOD_TAG, "v0.1.0-c0fe5bcb2c0c", "v0.1.0-95e4599f470d"):
        code, text = _run([tag], lambda path, _t=tag: ({"name": _t}, None))
        assert code == 0, text
        assert "all present" in text


def test_exit_is_nonzero_and_names_the_missing_images():
    def absent(path):
        return {"message": "404 Tag Not Found"}, None
    code, text = _run([GOOD_TAG], absent)
    assert code == 1
    for name in ("gonk-agent", "gonk-controller", "gonk-intake", "gonk-meter"):
        assert name in text
    assert "Do NOT bump the gitops tag" in text


# ---------------------------------------------------------------------------
# Which registry is asked is CONFIGURATION, never a constant (gonk-qzkq).
# ---------------------------------------------------------------------------

def test_the_script_carries_no_project_or_repository_id():
    """It used to hard-code one site's project id and four repository ids, and
    then asked whatever host glab happened to be logged into about them."""
    assert not hasattr(rit, "PROJECT")
    assert not hasattr(rit, "REPOS")


def test_the_paths_asked_come_from_the_site_file():
    asked = []

    def present(path):
        asked.append(path)
        return {"name": GOOD_TAG}, None
    code, text = _run([GOOD_TAG], present)
    assert code == 0, text
    assert sorted(asked) == [
        f"projects/7/registry/repositories/{n}/tags/{GOOD_TAG}" for n in (11, 12, 13, 14)]


def test_without_a_site_it_refuses_and_never_reaches_the_registry():
    """No site file, nothing in the environment: there is no registry to ask.
    It must say so and exit 2 -- not ask a default project, and above all not
    print "all present" about zero images."""
    def explode(path):
        raise AssertionError("with no site there is no registry to ask")
    code, text = _run([GOOD_TAG], explode, site=None)
    assert code == 2, text
    assert "no site file" in text
    assert rit.SITE_ENV in text
    assert "all present" not in text


def test_the_environment_names_another_site_file():
    other = _site_file({"project": "group/proj", "repositories": {"only-image": 5}})
    asked = []

    def present(path):
        asked.append(path)
        return {"name": GOOD_TAG}, None
    try:
        code, text = _run([GOOD_TAG], present, env={rit.SITE_ENV: other})
    finally:
        os.unlink(other)
    assert code == 0, text
    # A path-with-namespace project is URL-encoded, as the GitLab API wants.
    assert asked == [f"projects/group%2Fproj/registry/repositories/5/tags/{GOOD_TAG}"]
    assert "only-image" in text


def test_a_site_file_named_by_the_environment_that_is_missing_is_refused():
    def explode(path):
        raise AssertionError("a missing site file must not fall back to another")
    code, text = _run([GOOD_TAG], explode, env={rit.SITE_ENV: "/nonexistent/x.site.json"})
    assert code == 2, text
    assert "/nonexistent/x.site.json" in text


def test_a_malformed_site_is_refused_not_read_as_zero_images():
    """An empty repository map would loop zero times and print "all present".
    Every one of these must be a usage error instead."""
    def explode(path):
        raise AssertionError("a malformed site must not reach the registry")
    for bad in ("not json", "[]", {}, {"project": 7},
                {"project": 7, "repositories": {}},
                {"project": 7, "repositories": []},
                {"project": "", "repositories": {"a": 1}},
                {"project": None, "repositories": {"a": 1}},
                {"project": True, "repositories": {"a": 1}},
                {"project": 7, "repositories": {"a": "1"}},
                {"project": 7, "repositories": {"a": True}},
                {"project": 7, "repositories": {"": 1}}):
        code, text = _run([GOOD_TAG], explode, site=bad)
        assert code == 2, (bad, text)
        assert "site file" in text, (bad, text)
        assert "all present" not in text


# ---------------------------------------------------------------------------
# --tag-from-git (gonk-7ywn).
# ---------------------------------------------------------------------------

def test_tag_from_git_explains_itself_instead_of_guessing_a_tag():
    """It must NOT be silently taken for a literal tag name and reported as
    four absent images, and it must NOT reconstruct v<ver>-<sha12> -- which is
    not what a default-branch build publishes, and gitops pins default-branch
    builds."""
    def explode(path):
        raise AssertionError("--tag-from-git must not reach the registry")
    code, text = _run(["--tag-from-git"], explode)
    assert code == 2
    assert "--tag-from-git was removed" in text
    assert "gonk-7ywn" in text
    assert "<iid>.<sha12>" in text


def test_no_arguments_prints_usage_rather_than_guessing():
    """It used to fall through to the same git reconstruction with no argument
    at all, so a bare invocation silently checked a tag nobody asked about."""
    def explode(path):
        raise AssertionError("a bare invocation must not reach the registry")
    for argv in ([], ["--help"], ["-h"]):
        code, text = _run(argv, explode)
        assert code == 2, argv
        assert "usage:" in text


def test_there_is_no_head_tag_function_left_to_call():
    assert not hasattr(rit, "head_tag"), (
        "head_tag() reconstructed the tag from git; it cannot know the pipeline "
        "IID and must not come back")


if __name__ == "__main__":
    failures = 0
    for _name, _fn in sorted(globals().items()):
        if not _name.startswith("test_") or not callable(_fn):
            continue
        try:
            _fn()
        except AssertionError as exc:
            failures += 1
            print(f"FAIL {_name}: {exc}")
        else:
            print(f"OK   {_name}")
    raise SystemExit(1 if failures else 0)
