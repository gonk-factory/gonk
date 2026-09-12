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
import os
from contextlib import redirect_stderr, redirect_stdout
from unittest import mock

_HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location(
    "rit", os.path.join(_HERE, "require_image_tags.py"))
rit = importlib.util.module_from_spec(spec)
spec.loader.exec_module(rit)

GOOD_TAG = "v0.1.0-337.c0fe5bcb2c0c"


def _run(argv, glab_response):
    """Run main() with rit.glab stubbed, returning (exit code, stdout+stderr)."""
    out, err = io.StringIO(), io.StringIO()
    with mock.patch.object(rit, "glab", side_effect=glab_response):
        with redirect_stdout(out), redirect_stderr(err):
            code = rit.main(argv)
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
        ok, why = rit.tag_present(98, GOOD_TAG)
    assert ok is False
    assert why == "404 Tag Not Found"


def test_a_tag_object_naming_a_different_tag_is_not_present():
    """The other direction: a well-formed tag object is not enough either. It
    has to be the tag that was ASKED for."""
    with mock.patch.object(
            rit, "glab",
            return_value=({"name": "v0.1.0-336.b1b1b1b1b1b1"}, None)):
        ok, _ = rit.tag_present(98, GOOD_TAG)
    assert ok is False


def test_the_only_thing_that_counts_as_present_is_a_matching_name():
    with mock.patch.object(rit, "glab", return_value=({"name": GOOD_TAG}, None)):
        ok, why = rit.tag_present(98, GOOD_TAG)
    assert ok is True and why is None


def test_a_json_array_is_not_a_tag_object():
    with mock.patch.object(rit, "glab", return_value=([{"name": GOOD_TAG}], None)):
        ok, why = rit.tag_present(98, GOOD_TAG)
    assert ok is False
    assert "expected an object" in why


def test_a_transport_error_is_not_a_present_tag():
    with mock.patch.object(rit, "glab", return_value=(None, "connection refused")):
        ok, why = rit.tag_present(98, GOOD_TAG)
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
