"""Offline checks for the prune plan: no network, real tag names."""
import importlib.util
from unittest import mock

spec = importlib.util.spec_from_file_location(
    "rp", "/mnt/c/Users/steve/Code/gonk/hack/registry_prune.py")
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


def fake_glab(path, method="GET"):
    if path.endswith("/tags?per_page=100&page=1"):
        return [{"name": n} for n in TAGS]
    if "page=" in path:
        return []
    tag = path.rsplit("/", 1)[-1]
    return {"created_at": DATES.get(tag, "")}


with mock.patch.object(rp, "glab", side_effect=fake_glab):
    with mock.patch.object(rp, "glab_paged",
                           side_effect=lambda p: [{"name": n} for n in TAGS]):
        keep, delete = rp.plan_for_repo(1, "agentic/gonk-project/gonk-meter",
                                        DEPLOYED, keep_groups=5)

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
print("\nOK: keep/delete partition the tag set, deployed sha fully protected")

# The property that actually matters: at EVERY keep_groups setting, including
# the harshest (0, keep nothing but the deployed sha), no tag carrying the
# deployed sha is ever scheduled for deletion.
#
# Note on the SystemExit guard in plan_for_repo: with startswith(deployed) inside
# protected(), that assert cannot be reached by any input -- the two conditions
# are the same predicate. It is a defensive invariant against a future edit to
# protected(), not a branch these tests can exercise. Said plainly rather than
# dressed up as a mutation test that passes for the wrong reason.
for kg in range(0, 6):
    with mock.patch.object(rp, "glab", side_effect=fake_glab), \
         mock.patch.object(rp, "glab_paged",
                           side_effect=lambda p: [{"name": n} for n in TAGS]):
        k, d = rp.plan_for_repo(1, "repo", DEPLOYED, keep_groups=kg)
    assert not any(n.startswith(DEPLOYED) for n in d), f"leak at keep_groups={kg}"
    assert len([n for n in k if n.startswith(DEPLOYED)]) == 5, \
        f"all 5 deployed-sha tags must be kept at keep_groups={kg}, got {k}"
    print(f"  keep_groups={kg}: keep {len(k):2d}, delete {len(d):2d}, "
          "deployed sha intact")
print("\nOK: deployed sha protected at every keep_groups setting")
