"""Offline checks for the prune plan: no network, real tag names.

Run with `pytest hack/test_registry_prune.py`, or directly with
`python3 hack/test_registry_prune.py` (the __main__ block at the bottom runs
the same functions, so the way this file was invoked before still works).

NOTE, AND IT IS A GAP: no CI job in this repo runs any Python test. `make gate`
is fmt/vet/test/lint over Go only, and neither .github/workflows/ nor
.gitlab-ci.yml mentions pytest. These assertions are therefore only as good as
somebody remembering to run them -- filed as gonk-5ed1.
"""
import importlib.util
import os
from unittest import mock

# Resolve the module RELATIVE TO THIS FILE. It used to be an absolute path into
# /mnt/c/Users/steve/Code/gonk, so running the tests from a worktree silently
# tested the primary checkout's script instead of the one being changed -- a
# test that reports on code you are not editing.
_HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location(
    "rp", os.path.join(_HERE, "registry_prune.py"))
rp = importlib.util.module_from_spec(spec)
spec.loader.exec_module(rp)

DEPLOYED = "v0.1.0-4b3c7e185b55"

# Newest-first; shas invented except the deployed one.
TAGS = [
    "v0.1.0-aaaaaaaaaaaa", "v0.1.0-aaaaaaaaaaaa-amd64", "v0.1.0-aaaaaaaaaaaa-arm64",
    "v0.1.0-bbbbbbbbbbbb", "v0.1.0-bbbbbbbbbbbb-amd64", "v0.1.0-bbbbbbbbbbbb-arm64",
    "v0.1.0-cccccccccccc", "v0.1.0-cccccccccccc-amd64", "v0.1.0-cccccccccccc-arm64",
    "v0.1.0-dddddddddddd", "v0.1.0-dddddddddddd-amd64", "v0.1.0-dddddddddddd-arm64",
    "v0.1.0-eeeeeeeeeeee", "v0.1.0-eeeeeeeeeeee-amd64", "v0.1.0-eeeeeeeeeeee-arm64",
    "v0.1.0-ffffffffffff", "v0.1.0-ffffffffffff-amd64",           # 6th group: goes
    DEPLOYED, DEPLOYED + "-amd64", DEPLOYED + "-arm64",           # old but deployed
    DEPLOYED + "-testclock", DEPLOYED + "-testclock-amd64",       # carries the sha
    "v0.1.0-999999999999-testclock", "v0.1.0-999999999999",       # old: goes
]

# created_at descending in list order
DATES = {n: f"2026-08-{28 - i // 3:02d}T00:00:00Z" for i, n in enumerate(TAGS)}

# The same registry AFTER the tag-shape change (0f13d63): default-branch builds
# publish v0.1.0-<iid>.<sha12>, branch builds keep v0.1.0-<sha12>, and the
# legacy tags from before the change are still sitting there. All three coexist,
# which is the state this prune has to be correct in.
NEW_DEPLOYED = "v0.1.0-337.c0fe5bcb2c0c"
MIXED_TAGS = [
    NEW_DEPLOYED, NEW_DEPLOYED + "-amd64", NEW_DEPLOYED + "-arm64",
    NEW_DEPLOYED + "-testclock",
    "v0.1.0-336.b1b1b1b1b1b1", "v0.1.0-336.b1b1b1b1b1b1-amd64",
    "v0.1.0-335.a2a2a2a2a2a2", "v0.1.0-335.a2a2a2a2a2a2-amd64",
    "v0.1.0-334.939393939393", "v0.1.0-334.939393939393-amd64",
    "v0.1.0-333.848484848484", "v0.1.0-333.848484848484-amd64",
    "v0.1.0-332.757575757575",                            # 6th group: goes
    "v0.1.0-deadbeefcafe", "v0.1.0-deadbeefcafe-amd64",   # a branch build: goes
    "v0.1.0-95e4599f470d",                                # legacy: goes
]
MIXED_DATES = {n: f"2026-09-{20 - i // 3:02d}T00:00:00Z"
               for i, n in enumerate(MIXED_TAGS)}


def _fake_registry(tags, dates):
    """A stand-in for rp.glab covering both endpoints plan_for_repo hits."""
    def fake_glab(path, method="GET"):
        if path.endswith("/tags?per_page=100&page=1"):
            return [{"name": n} for n in tags]
        if "page=" in path:
            return []
        tag = path.rsplit("/", 1)[-1]
        return {"created_at": dates.get(tag, "")}
    return fake_glab


def _plan(tags, dates, deployed, keep_groups):
    with mock.patch.object(rp, "glab", side_effect=_fake_registry(tags, dates)), \
         mock.patch.object(rp, "glab_paged",
                           side_effect=lambda p: [{"name": n} for n in tags]):
        return rp.plan_for_repo(1, "agentic/gonk-project/gonk-meter",
                                deployed, keep_groups=keep_groups)


# ---------------------------------------------------------------------------
# TAG_RE: what the script is willing to call "the deployed tag".
# ---------------------------------------------------------------------------

def test_tag_re_matches_both_deployable_shapes():
    """Legacy/branch v<ver>-<sha12> AND default-branch v<ver>-<iid>.<sha12>.

    Before gonk-7ywn only the first matched, so the moment gitops pinned a
    new-shape tag deployed_tags() came back empty and the prune refused to run
    -- fail-safe, but a prune that never runs is how the volume filled once
    already (gonk-mzm).
    """
    assert rp.TAG_RE.findall("tag: v0.1.0-95e4599f470d\n") == ["v0.1.0-95e4599f470d"]
    assert rp.TAG_RE.findall("tag: v0.1.0-337.c0fe5bcb2c0c\n") == ["v0.1.0-337.c0fe5bcb2c0c"]
    # The arch legs and the testclock meter reduce to their BASE tag, which is
    # what deployed_tags() wants -- protection is a prefix test, so the base
    # covers every child.
    assert rp.TAG_RE.findall("v0.1.0-337.c0fe5bcb2c0c-amd64") == ["v0.1.0-337.c0fe5bcb2c0c"]
    assert rp.TAG_RE.findall("v0.1.0-337.c0fe5bcb2c0c-testclock") == ["v0.1.0-337.c0fe5bcb2c0c"]
    # A larger version number, and a one-digit IID.
    assert rp.TAG_RE.findall("v10.20.30-1.abcdefabcdef") == ["v10.20.30-1.abcdefabcdef"]
    # A sha12 that is all digits must not be mistaken for an IID.
    assert rp.TAG_RE.findall("v0.1.0-123456789012") == ["v0.1.0-123456789012"]
    assert rp.TAG_RE.findall("v0.1.0-42.000123456789") == ["v0.1.0-42.000123456789"]


def test_tag_re_refuses_to_truncate_a_longer_hex_run():
    """No partial match. A truncated `deployed` would still be SAFE (protected()
    is a startswith, so a short prefix over-protects) but it would print a tag
    that does not exist in the "protecting deployed tag X" banner, and the
    operator reading that banner is the last line of defence. Refusing is the
    better failure."""
    assert rp.TAG_RE.findall("v0.1.0-c0fe5bcb2c0cdeadbeef") == []
    assert rp.TAG_RE.findall("v0.1.0-337.c0fe5bcb2c0cdeadbeef") == []


def test_tag_re_ignores_things_that_are_not_tags():
    """The HelmRelease also carries chart versions, a dolt image tag and prose."""
    for junk in ("tag: 2.1.7", "version: 0.1.2", "# v0.1.0-<sha12> at all",
                 "sha256:020c0d20b9880058cbe785a9db107156c3c75c2ac944a6aa7ab59f2"):
        assert rp.TAG_RE.findall(junk) == [], junk


def test_deployed_tags_returns_every_pin_not_just_the_first(tmp_path):
    """The keep-vs-delete defect this change exists to close.

    The old deployed_tag() did sorted(set(...))[0] behind a `len(found) == 1`
    test whose two branches were identical, so a HelmRelease naming two tags
    protected the alphabetically-first and put the other -- also deployed -- in
    the delete set, with the hard stop looking only for the one it was handed.
    """
    hr = tmp_path / "helmrelease-gonk.yaml"
    hr.write_text(
        "controller:\n  image:\n    tag: v0.1.0-337.c0fe5bcb2c0c\n"
        "meter:\n  image:\n    tag: v0.1.0-95e4599f470d\n")
    assert rp.deployed_tags(str(hr), None) == (
        "v0.1.0-337.c0fe5bcb2c0c", "v0.1.0-95e4599f470d")

    tags = MIXED_TAGS + ["v0.1.0-95e4599f470d-amd64"]
    dates = dict(MIXED_DATES, **{"v0.1.0-95e4599f470d-amd64": "2026-09-01T00:00:00Z"})
    keep, delete = _plan(tags, dates, rp.deployed_tags(str(hr), None), keep_groups=1)
    assert "v0.1.0-95e4599f470d" in keep
    assert "v0.1.0-95e4599f470d-amd64" in keep
    assert "v0.1.0-337.c0fe5bcb2c0c" in keep
    assert not any(n.startswith(("v0.1.0-95e4599f470d", "v0.1.0-337.c0fe5bcb2c0c"))
                   for n in delete)


def test_deployed_tags_is_empty_when_the_file_is_unreadable():
    assert rp.deployed_tags("/nonexistent/helmrelease.yaml", None) == ()


def test_deployed_tag_override_wins_and_accepts_several():
    assert rp.deployed_tags("/nonexistent", ["v0.1.0-1.aaaaaaaaaaaa"]) == (
        "v0.1.0-1.aaaaaaaaaaaa",)
    assert rp.deployed_tags("/nonexistent", ["b", "a", "a"]) == ("a", "b")


# ---------------------------------------------------------------------------
# The keep/delete plan. NOTHING in it may depend on the shape of a tag.
# ---------------------------------------------------------------------------

def test_legacy_shape_plan_partitions_and_protects_the_deployed_sha():
    keep, delete = _plan(TAGS, DATES, DEPLOYED, keep_groups=5)
    print("keep  :", keep)
    print("delete:", delete)

    assert DEPLOYED in keep, "deployed tag must survive"
    assert DEPLOYED + "-amd64" in keep and DEPLOYED + "-arm64" in keep
    assert DEPLOYED + "-testclock" in keep, "testclock of the deployed sha must survive"
    assert DEPLOYED + "-testclock-amd64" in keep
    assert not any(n.startswith(DEPLOYED) for n in delete)
    assert "v0.1.0-ffffffffffff" in delete, "6th-newest group should be pruned"
    assert "v0.1.0-999999999999-testclock" in delete
    assert set(keep) | set(delete) == set(TAGS), "every tag must be classified"
    assert not (set(keep) & set(delete)), "no tag in both sets"


def test_deployed_sha_is_protected_at_every_keep_groups_setting():
    """The property that actually matters: at EVERY keep_groups setting,
    including the harshest (0, keep nothing but the deployed sha), no tag
    carrying the deployed sha is ever scheduled for deletion.

    Note on the SystemExit guard in plan_for_repo: with startswith(deployed)
    inside protected(), that assert cannot be reached by any input -- the two
    conditions are the same predicate. It is a defensive invariant against a
    future edit to protected(), not a branch these tests can exercise. Said
    plainly rather than dressed up as a mutation test that passes for the wrong
    reason.
    """
    for kg in range(0, 6):
        k, d = _plan(TAGS, DATES, DEPLOYED, keep_groups=kg)
        assert not any(n.startswith(DEPLOYED) for n in d), f"leak at keep_groups={kg}"
        assert len([n for n in k if n.startswith(DEPLOYED)]) == 5, \
            f"all 5 deployed-sha tags must be kept at keep_groups={kg}, got {k}"
        print(f"  keep_groups={kg}: keep {len(k):2d}, delete {len(d):2d}, "
              "deployed sha intact")


def test_new_shape_plan_behaves_exactly_like_the_old_one():
    """Grouping, ordering and protection are shape-independent, so the same
    assertions must hold over a registry of <iid>.<sha> tags."""
    keep, delete = _plan(MIXED_TAGS, MIXED_DATES, NEW_DEPLOYED, keep_groups=5)
    print("keep  :", keep)
    print("delete:", delete)

    assert NEW_DEPLOYED in keep
    assert NEW_DEPLOYED + "-amd64" in keep and NEW_DEPLOYED + "-arm64" in keep
    assert NEW_DEPLOYED + "-testclock" in keep
    assert not any(n.startswith(NEW_DEPLOYED) for n in delete)
    assert "v0.1.0-332.757575757575" in delete, "6th-newest group should be pruned"
    assert "v0.1.0-deadbeefcafe" in delete, "an old branch build is prunable"
    assert "v0.1.0-95e4599f470d" in delete, "an old legacy tag is prunable"
    assert set(keep) | set(delete) == set(MIXED_TAGS)
    assert not (set(keep) & set(delete))

    for kg in range(0, 6):
        k, d = _plan(MIXED_TAGS, MIXED_DATES, NEW_DEPLOYED, keep_groups=kg)
        assert not any(n.startswith(NEW_DEPLOYED) for n in d), f"leak at keep_groups={kg}"
        assert len([n for n in k if n.startswith(NEW_DEPLOYED)]) == 4, \
            f"all 4 deployed-sha tags must be kept at keep_groups={kg}, got {k}"


def test_widening_the_regex_cannot_move_a_tag_from_keep_to_delete():
    """The hazard the brief names: matching MORE tags must not delete more.

    Protecting a SUPERSET of the tags can only keep a superset, because
    protected() is a disjunction over the deployed set and keepset is a union
    with it. Demonstrated rather than asserted in prose: the same registry,
    planned with one pin and then with two, and the second keep set contains
    the first.
    """
    one_keep, one_delete = _plan(MIXED_TAGS, MIXED_DATES, NEW_DEPLOYED, keep_groups=2)
    two_keep, two_delete = _plan(
        MIXED_TAGS, MIXED_DATES,
        (NEW_DEPLOYED, "v0.1.0-95e4599f470d"), keep_groups=2)
    assert set(one_keep) <= set(two_keep)
    assert set(two_delete) <= set(one_delete)
    assert "v0.1.0-95e4599f470d" in one_delete
    assert "v0.1.0-95e4599f470d" in two_keep


def test_plan_accepts_a_bare_string_for_backwards_compatibility():
    a_keep, a_delete = _plan(TAGS, DATES, DEPLOYED, keep_groups=3)
    b_keep, b_delete = _plan(TAGS, DATES, (DEPLOYED,), keep_groups=3)
    assert a_keep == b_keep and a_delete == b_delete


if __name__ == "__main__":
    import tempfile
    import pathlib
    failures = 0
    for _name, _fn in sorted(globals().items()):
        if not _name.startswith("test_") or not callable(_fn):
            continue
        with tempfile.TemporaryDirectory() as _td:
            _args = (pathlib.Path(_td),) if _fn.__code__.co_argcount else ()
            try:
                _fn(*_args)
            except AssertionError as exc:
                failures += 1
                print(f"FAIL {_name}: {exc}")
            else:
                print(f"OK   {_name}")
    raise SystemExit(1 if failures else 0)
