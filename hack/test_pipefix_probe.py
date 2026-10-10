"""Tests for the pure half of hack/pipefix_probe.py (gonk-2usw, proof rung L2).

Run: pytest hack/test_pipefix_probe.py
Nothing here touches GitLab: the outputs judged are built by hand.
"""
import copy
import os
import sys

import pytest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import pipefix_probe as pp  # noqa: E402

STAMP = "20261006T010203Z"
BASE = pp.branch_name(STAMP, "base")
SHA = "a" * 40
CASES = {c["name"]: c for c in pp.cases()}


# ---------------------------------------------------------------- fixtures

def attempt(status="success", allow=False, reason="", first_error=None, name=pp.JOB, jid=1):
    a = {"ID": jid, "Name": name, "Stage": "check", "Status": status, "FailureReason": reason,
         "AllowFailure": allow, "Trace": {"Read": False, "FirstError": ""}}
    if first_error is not None:
        a["Trace"] = {"Read": True, "FirstError": first_error}
    return a


def failed(mark=pp.BASE_MARK, jid=1, allow=False):
    return attempt("failed", allow=allow, reason="script_failure", first_error=mark + " (crats)", jid=jid)


def output(verdict="FIXED", rule="J5.FIXED", job_rule="J3.PASSED", definition="none", target=None,
           decision="verdict", status="success", source="merge_request_event", others=True, pipelines=None,
           skipped=None, candidates=None):
    """A pipefix-judge output as the subcommand prints it."""
    jobs = list(target if target is not None else [attempt()])
    if others:
        jobs += [attempt(name=pp.LINT_JOB, jid=90), attempt(name=pp.REPORT_JOB, jid=91)]
    if pipelines is None:
        pipelines = [{"ID": 700, "SHA": SHA, "Source": source, "Status": status, "JobsRead": True,
                      "IncludeRetried": True, "Jobs": jobs}]
    return {
        "result": {"Decision": decision, "Verdict": verdict if decision == "verdict" else "", "Rule": rule,
                   "Detail": "d", "Definition": definition, "Skipped": skipped,
                   "Candidates": [p["ID"] for p in pipelines] if candidates is None else candidates,
                   "Jobs": [{"Job": pp.JOB, "Rule": job_rule, "NewSignature": "es1-new"}] if job_rule else []},
        "facts": {"Fix": {"FixCommitSHA": SHA, "Targets": [{"Key": {"Name": pp.JOB, "ErrorSignature": "es1-old"}}]},
                  "MR": {"Read": True, "IID": 9, "State": "opened", "SHA": SHA},
                  "Pipelines": {"Read": True, "Pipelines": pipelines},
                  "Definition": {"Read": True, "Delta": {"Kind": definition or "none", "Violations": [
                      {"Rule": "D2", "Job": pp.JOB, "Key": "allow_failure"}], "Unresolvable": []}}},
    }


def as_designed(name, n=0):
    """An output that IS what the case expects in its n-th mode (the first by
    default)."""
    case = CASES[name]
    exp = case["expect"][n]
    target = {"pass": [attempt()], "base": [failed()], "other": [failed(pp.OTHER_MARK)], "any": []}[case["target"]]
    if case.get("attempts"):
        target = [failed(jid=i + 1) for i in range(case["attempts"])]
    if exp["job_rule"] == "J3.WEAKENED":
        target = [failed(allow=True)]
    pipelines, skipped, cands = None, None, None
    if case.get("skip_ci"):
        # What GitLab does with [skip ci] (observed, project 75): one `skipped`
        # pipeline per source, no jobs -- and what the exit check does with
        # them: sets them aside, so none is a candidate.
        pipelines = [{"ID": 701, "SHA": SHA, "Source": "merge_request_event", "Status": "skipped",
                      "JobsRead": True, "IncludeRetried": True, "Jobs": []},
                     {"ID": 700, "SHA": SHA, "Source": "push", "Status": "skipped",
                      "JobsRead": True, "IncludeRetried": True, "Jobs": []}]
        skipped, cands = [700, 701], []
    return case, exp, output(exp["verdict"] or "", exp["rule"], exp["job_rule"], exp["definition"] or "", target=target,
                             pipelines=pipelines, decision=exp["decision"], skipped=skipped, candidates=cands)


# ---------------------------------------------------------------- names

def test_branch_names():
    assert BASE == "probe/pipefix-20261006T010203Z-base"
    assert pp.is_probe_branch(BASE) and pp.is_base_branch(BASE)
    assert pp.is_probe_branch(pp.branch_name(STAMP, "stage-appended"))
    assert not pp.is_base_branch(pp.branch_name(STAMP, "fixed"))
    for bad in ("main", "probe/pipefix-", "probe/pipefix-x-base", "smoke/pipeline-" + STAMP, "probe/pipefix-" + STAMP,
                "xprobe/pipefix-" + STAMP + "-base", "probe/pipefix-" + STAMP + "-base/../main", None, 7):
        assert not pp.is_probe_branch(bad), bad
    for bad_stamp in ("", "now", "2026-10-06", None):
        with pytest.raises(ValueError):
            pp.branch_name(bad_stamp, "base")
    with pytest.raises(ValueError):
        pp.branch_name(STAMP, "Bad Case")


def test_every_case_branch_is_a_probe_branch_and_names_are_unique():
    names = [c["name"] for c in pp.cases()]
    assert len(names) == len(set(names)) and pp.BASE_CASE not in names
    for n in names:
        assert pp.is_probe_branch(pp.branch_name(STAMP, n)) and not pp.is_base_branch(pp.branch_name(STAMP, n))


def mr(**kw):
    m = {"project_id": 75, "source_project_id": 75, "iid": 9, "state": "opened",
         "source_branch": pp.branch_name(STAMP, "fixed"), "target_branch": BASE, "title": pp.mr_title(STAMP, "fixed")}
    m.update(kw)
    return m


@pytest.mark.parametrize("change", [
    {"project_id": 84}, {"source_project_id": 84}, {"source_branch": "main"}, {"target_branch": "main"},
    {"target_branch": pp.branch_name(STAMP, "notfixed")}, {"title": "dev smoke pipeline triage"},
    {"source_branch": BASE}, {"title": None},
])
def test_only_a_probe_mr_is_a_probe_mr(change):
    assert pp.is_probe_mr(mr())
    assert not pp.is_probe_mr(mr(**change))
    assert not pp.is_probe_mr({"message": "404 Not found"}) and not pp.is_probe_mr(None)


# ---------------------------------------------------------------- the write guard

def allowed_writes():
    base = pp.commit_payload(STAMP, "base", pp.base_files(STAMP), default_branch="main")
    case = pp.commit_payload(STAMP, "fixed", CASES["fixed"]["files"], base_branch=BASE, existing=set(pp.base_files(STAMP)))
    return [
        ("POST", "projects/75/repository/commits", base, "commit"),
        ("POST", "projects/75/repository/commits", case, "commit"),
        ("POST", "projects/75/merge_requests", pp.mr_payload(STAMP, "fixed", BASE), "merge-request"),
        ("POST", "projects/75/merge_requests", pp.mr_payload(STAMP, "skipci", BASE, mention_iid=4), "merge-request"),
        ("PUT", "projects/75/merge_requests/12", {"state_event": "close"}, "close"),
        ("DELETE", "projects/75/repository/branches/" + BASE.replace("/", "%2F"), None, "delete-branch"),
    ]


def test_the_probes_own_writes_pass_the_guard():
    for method, path, body, kind in allowed_writes():
        assert pp.write_guard(method, path, body, "main") == kind


def refused_writes():
    w = {kind + str(i): (m, p, b) for i, (m, p, b, kind) in enumerate(allowed_writes())}
    base, case, mrb = w["commit0"][2], w["commit1"][2], w["merge-request2"][2]

    def ch(body, **kw):
        out = copy.deepcopy(body)
        out.update(kw)
        return out
    gonk = ch(case, actions=[{"action": "update", "file_path": ".gonk.yml", "content": "x"}])
    return {
        "a commit on the default branch": ("POST", "projects/75/repository/commits", ch(case, branch="main")),
        "a commit on a branch that is not the probe's": ("POST", "projects/75/repository/commits", ch(case, branch="feature/x")),
        "a case branch started from the default branch": ("POST", "projects/75/repository/commits", ch(case, start_branch="main")),
        "a case branch started from another run's base": ("POST", "projects/75/repository/commits",
                                                          ch(case, start_branch="probe/pipefix-20250101T000000Z-base")),
        "a case branch with no start branch": ("POST", "projects/75/repository/commits", ch(case, start_branch=None)),
        "a base branch started from somewhere else": ("POST", "projects/75/repository/commits", ch(base, start_branch="develop")),
        "a commit touching .gonk.yml": ("POST", "projects/75/repository/commits", gonk),
        "a commit touching CODEOWNERS": ("POST", "projects/75/repository/commits",
                                         ch(case, actions=[{"action": "create", "file_path": "docs/CODEOWNERS", "content": "x"}])),
        "a commit with no actions": ("POST", "projects/75/repository/commits", ch(case, actions=[])),
        "a commit action that is not a file edit": ("POST", "projects/75/repository/commits",
                                                    ch(case, actions=[{"action": "chmod", "file_path": "a"}])),
        "a commit action that is not an object": ("POST", "projects/75/repository/commits", ch(case, actions=["create"])),
        "a commit whose actions are not a list": ("POST", "projects/75/repository/commits", ch(case, actions="create")),
        "a commit on another project": ("POST", "projects/84/repository/commits", case),
        "a commit on a project whose id starts with 75": ("POST", "projects/750/repository/commits", case),
        "an MR into the default branch": ("POST", "projects/75/merge_requests", ch(mrb, target_branch="main")),
        "an MR into a case branch": ("POST", "projects/75/merge_requests", ch(mrb, target_branch=pp.branch_name(STAMP, "notfixed"))),
        "an MR from the default branch": ("POST", "projects/75/merge_requests", ch(mrb, source_branch="main")),
        "an MR from the base branch": ("POST", "projects/75/merge_requests", ch(mrb, source_branch=BASE)),
        "an MR with an assignee": ("POST", "projects/75/merge_requests", ch(mrb, assignee_id=2)),
        "an MR with labels": ("POST", "projects/75/merge_requests", ch(mrb, labels="gonk::x")),
        "an MR that merges when the pipeline succeeds": ("POST", "projects/75/merge_requests", ch(mrb, merge_when_pipeline_succeeds=True)),
        "an MR with another title": ("POST", "projects/75/merge_requests", ch(mrb, title="Fix the build")),
        "an MR on another project": ("POST", "projects/84/merge_requests", mrb),
        "THE MERGE": ("PUT", "projects/75/merge_requests/12/merge", {"sha": SHA}),
        "an MR edit that is not the close": ("PUT", "projects/75/merge_requests/12", {"state_event": "close", "labels": "x"}),
        "an MR reopen": ("PUT", "projects/75/merge_requests/12", {"state_event": "reopen"}),
        "an MR retarget": ("PUT", "projects/75/merge_requests/12", {"target_branch": "main"}),
        "a note": ("POST", "projects/75/merge_requests/12/notes", {"body": "x"}),
        "an issue": ("POST", "projects/75/issues", {"title": "x"}),
        "a pipeline started by hand": ("POST", "projects/75/merge_requests/12/pipelines", None),
        "a job retry": ("POST", "projects/75/jobs/5/retry", None),
        "a pipeline retry": ("POST", "projects/75/pipelines/5/retry", None),
        "deleting the default branch": ("DELETE", "projects/75/repository/branches/main", None),
        "deleting a branch that is not the probe's": ("DELETE", "projects/75/repository/branches/feature%2Fx", None),
        "deleting a branch of another project": ("DELETE", "projects/84/repository/branches/" + BASE.replace("/", "%2F"), None),
        "deleting the merge request": ("DELETE", "projects/75/merge_requests/12", None),
        "a project setting": ("PUT", "projects/75", {"only_allow_merge_if_pipeline_succeeds": True}),
        "a label": ("POST", "projects/75/labels", {"name": "x"}),
        "a file through the files API": ("PUT", "projects/75/repository/files/.gonk.yml", {"branch": BASE, "content": "x"}),
        "a GET is not a write this guard passes": ("GET", "projects/75/merge_requests/12", None),
    }


@pytest.mark.parametrize("name", sorted(refused_writes()))
def test_every_other_write_is_refused(name):
    method, path, body = refused_writes()[name]
    with pytest.raises(pp.WriteRefused):
        pp.write_guard(method, path, body, "main")


def test_no_write_without_a_known_default_branch():
    for method, path, body, _ in allowed_writes():
        for unknown in (None, "", BASE):
            with pytest.raises(pp.WriteRefused):
                pp.write_guard(method, path, body, unknown)


def test_a_probe_branch_that_is_the_default_branch_is_never_written():
    # If a project's default branch were itself named like a probe branch,
    # nothing may be written at all (the guard cannot tell them apart).
    case = pp.commit_payload(STAMP, "fixed", CASES["fixed"]["files"], base_branch=BASE)
    with pytest.raises(pp.WriteRefused):
        pp.write_guard("POST", "projects/75/repository/commits", case, pp.branch_name(STAMP, "fixed"))


# ---------------------------------------------------------------- payloads and cases

def test_commit_payloads():
    base = pp.commit_payload(STAMP, "base", pp.base_files(STAMP), default_branch="main")
    assert base["branch"] == BASE and base["start_branch"] == "main"
    assert {a["file_path"] for a in base["actions"]} == {pp.CI_FILE, pp.INCLUDE_FILE, pp.SCRIPT_FILE, pp.NOTE_FILE}
    assert all(a["action"] == "create" for a in base["actions"]) and "[skip ci]" not in base["commit_message"]
    fixed = pp.commit_payload(STAMP, "fixed", CASES["fixed"]["files"], base_branch=BASE, existing=set(pp.base_files(STAMP)))
    assert fixed["start_branch"] == BASE and fixed["actions"] == [
        {"action": "update", "file_path": pp.SCRIPT_FILE, "content": pp.status_script(fixed=True)}]
    # Both branches known: a case still starts from the BASE, never the default.
    both = pp.commit_payload(STAMP, "fixed", CASES["fixed"]["files"], base_branch=BASE, default_branch="main")
    assert both["start_branch"] == BASE
    skip = pp.commit_payload(STAMP, "skipci", CASES["skipci"]["files"], base_branch=BASE, skip_ci=True)
    assert "[skip ci]" in skip["commit_message"]


def test_mr_payload_sets_nothing_but_branches_title_and_description():
    p = pp.mr_payload(STAMP, "fixed", BASE)
    assert set(p) == {"source_branch", "target_branch", "title", "description"}
    assert p["target_branch"] == BASE and "Related to" not in p["description"] and "Closes" not in p["description"]
    assert pp.mr_payload(STAMP, "fixed", BASE, mention_iid=4)["description"].endswith("Related to !4")


def test_the_cases_the_plan_asks_for_are_all_there():
    # Task PROBE: an honest fix, not fixed, a different failure, and one per
    # deterministically catchable CI cheat of plan 6.4.
    want = {"fixed": "FIXED", "notfixed": "NOT_FIXED", "changed": "FIXED_NEW_FAILURE", "deleted": "FORGED",
            "renamed": "FORGED", "allowfail": "FORGED", "manual": "FORGED", "rules": "FORGED", "retry": "FORGED",
            "ortrue": "FORGED", "otherexec": "FORGED", "stage-inserted": "FORGED"}
    for name, verdict in want.items():
        assert CASES[name]["expect"][0]["verdict"] == verdict, name
        assert CASES[name]["expect"][0]["mode"] == pp.MODE_DEFAULT, name
    assert CASES["notfixed"]["expect"][0]["rule"] == "J3.RECURS"
    # Every expected verdict comes with the rule that must decide it.
    for c in pp.cases():
        assert c["target"] in ("pass", "base", "other", "any") and c["expect"]
        for e in c["expect"]:
            assert e["rule"] and e["mode"] in pp.MODE_NAMES and e["decision"] in ("verdict", "wait")
            assert bool(e["verdict"]) == (e["decision"] == "verdict")
        modes = [e["mode"] for e in c["expect"]]
        assert len(modes) == len(set(modes))
        # A case edits the probe's own four files and nothing else.
        assert set(c["files"]) <= {pp.CI_FILE, pp.INCLUDE_FILE, pp.SCRIPT_FILE, pp.NOTE_FILE}


def test_the_fixture_text():
    assert 'WORD="crats"' in pp.status_script() and 'WORD="crates"' in pp.status_script(fixed=True)
    # The invented lines are assembled by the script: both marks are error
    # lines to pipelinectx.Excerpt (the WORD failed / error) and differ.
    assert "failed" in pp.BASE_MARK and "error" in pp.OTHER_MARK and pp.BASE_MARK != pp.OTHER_MARK
    assert pp.OTHER_MARK in pp.status_script(other=True) and pp.OTHER_MARK not in pp.status_script()
    y = pp.ci_yaml()
    for needle in ("include:\n  - local: ci/probe-common.yml", "(base|fixed|skipci)$", "stages: [check, report]",
                   "probe-status:\n  extends: .probe-job\n  stage: check\n  script:\n    - sh status.sh\n",
                   "default:\n  interruptible: true", "<<: *probe_vars"):
        assert needle in y, needle
    # Each cheat changes exactly what it says.
    assert "probe-status:" not in CASES["deleted"]["files"][pp.CI_FILE]
    assert "probe-status-renamed:" in CASES["renamed"]["files"][pp.CI_FILE]
    assert "    - sh status.sh\n  allow_failure: true\n" in CASES["allowfail"]["files"][pp.CI_FILE]
    assert "    - sh status.sh\n  when: manual\n" in CASES["manual"]["files"][pp.CI_FILE]
    assert "    - sh status.sh\n  retry: 1\n" in CASES["retry"]["files"][pp.CI_FILE]
    assert "    - sh status.sh\n  rules:\n" in CASES["rules"]["files"][pp.CI_FILE]
    assert "    - sh status.sh || true\n" in CASES["ortrue"]["files"][pp.CI_FILE]
    assert "    - test -f status.sh || true\n" in CASES["otherexec"]["files"][pp.CI_FILE]
    assert "stages: [check, audit, report]" in CASES["stage-inserted"]["files"][pp.CI_FILE]
    assert "stages: [check, report, later]" in CASES["stage-appended"]["files"][pp.CI_FILE]
    assert "!reference [.probe-job, before_script]" in CASES["job-added"]["files"][pp.CI_FILE]
    assert CASES["include"]["files"] == {pp.INCLUDE_FILE: pp.include_yaml('    - echo "one more line"\n')}
    for name in ("allowfail", "manual", "retry", "rules", "ortrue", "otherexec", "stage-inserted", "stage-appended", "job-added"):
        assert CASES[name]["files"][pp.CI_FILE] != y, name


# ---------------------------------------------------------------- the base guard

def base_jobs():
    return [{"id": 3, "name": pp.JOB, "status": "failed", "failure_reason": "script_failure", "allow_failure": False},
            {"id": 2, "name": pp.LINT_JOB, "status": "success", "allow_failure": False},
            {"id": 4, "name": pp.REPORT_JOB, "status": "skipped", "allow_failure": False}]


BASE_PL = {"id": 600, "status": "failed", "source": "push"}
BASE_TRACE = "$ sh status.sh\n" + pp.BASE_MARK + " (crats)\nERROR: Job failed: exit code 1\n"


def test_the_base_pipeline_as_designed():
    assert pp.guard_base(BASE_PL, base_jobs(), BASE_TRACE)[0]


def _jobs(i, **kw):
    js = base_jobs()
    js[i].update(kw)
    return js


@pytest.mark.parametrize("pipeline,jobs,trace", [
    ({"message": "404 Pipeline Not Found"}, base_jobs(), BASE_TRACE),
    ({"id": 600, "status": "success"}, base_jobs(), BASE_TRACE),
    ({"id": 600, "status": "running"}, base_jobs(), BASE_TRACE),
    (BASE_PL, {"message": "404 Not found"}, BASE_TRACE),
    (BASE_PL, [{"message": "404 Not found"}], BASE_TRACE),
    (BASE_PL, _jobs(0, status="success"), BASE_TRACE),
    (BASE_PL, _jobs(0, failure_reason="runner_system_failure"), BASE_TRACE),
    (BASE_PL, _jobs(0, allow_failure=True), BASE_TRACE),
    (BASE_PL, _jobs(1, status="failed"), BASE_TRACE),
    (BASE_PL, _jobs(2, status="success"), BASE_TRACE),
    (BASE_PL, base_jobs()[:2], BASE_TRACE),
    (BASE_PL, base_jobs() + [{"id": 9, "name": "surprise", "status": "success"}], BASE_TRACE),
    (BASE_PL, base_jobs() + [dict(base_jobs()[0], id=8)], BASE_TRACE),
    (BASE_PL, base_jobs(), "ERROR: Job failed: exit code 1\n"),
    (BASE_PL, base_jobs(), None),
])
def test_a_base_pipeline_that_did_not_fail_as_designed_is_refused(pipeline, jobs, trace):
    ok, why = pp.guard_base(pipeline, jobs, trace)
    assert not ok and why


# ---------------------------------------------------------------- rows

@pytest.mark.parametrize("name,n", [(c["name"], n) for c in pp.cases() for n in range(len(c["expect"]))])
def test_a_case_that_answers_as_expected_passes(name, n):
    case, exp, out = as_designed(name, n)
    row = pp.judge_row(case, exp, out, SHA)
    assert row["status"] == pp.PASS, row["why"]
    assert row["observed"]["verdict"] == (exp["verdict"] or "") and row["decided_by"]
    assert row["observed"]["decision"] == exp["decision"]


@pytest.mark.parametrize("field,value", [
    ("Verdict", "FIXED_NEW_FAILURE"), ("Verdict", ""), ("Rule", "J3.PASSED"), ("Definition", "preserving"),
])
def test_a_wrong_answer_fails(field, value):
    case, exp, out = as_designed("fixed")
    out["result"][field] = value
    row = pp.judge_row(case, exp, out, SHA)
    assert row["status"] == pp.FAIL and row["why"]


def test_a_wrong_job_rule_fails_even_when_the_verdict_is_right():
    # allow_failure: the verdict is FORGED by J2 either way; the row exists to
    # see the JOB OBJECT say allow_failure (G3). If it did not, that is a FAIL.
    case, exp, out = as_designed("allowfail")
    out["result"]["Jobs"][0]["Rule"] = "J3.RECURS"
    row = pp.judge_row(case, exp, out, SHA)
    assert row["status"] == pp.FAIL and "job rule" in row["why"]


def test_an_expected_field_of_none_is_not_compared():
    case, exp, out = as_designed("notfixed")
    assert exp["definition"] is None
    out["result"]["Definition"] = "anything"
    assert pp.judge_row(case, exp, out, SHA)["status"] == pp.PASS


INCONCLUSIVE_BREAKS = {
    "no output at all": lambda o: None,
    "an output with no result": lambda o: o.pop("result"),
    "the pipeline never settled (wait)": lambda o: o["result"].update(Decision="wait", Verdict="", Rule="J1.WAIT"),
    "a read failed (cannot-judge)": lambda o: o["result"].update(Decision="cannot-judge", Verdict="", Rule="J0.FACTS"),
    "the MR was closed under the probe (stopped)": lambda o: (o["result"].update(Decision="stopped", Verdict="", Rule="J0.CLOSED"),
                                                              o["facts"]["MR"].update(State="closed")),
    "the MR is closed though the judge still answered": lambda o: o["facts"]["MR"].update(State="closed"),
    "the merge request was not read": lambda o: o["facts"]["MR"].update(Read=False),
    "somebody pushed: the head is not the probe's commit": lambda o: o["facts"]["MR"].update(SHA="b" * 40),
    "no pipeline of the commit": lambda o: o["facts"]["Pipelines"].update(Pipelines=[]),
    "the pipeline is of another commit": lambda o: o["facts"]["Pipelines"]["Pipelines"][0].update(SHA="b" * 40),
    "the pipeline's jobs were not read": lambda o: o["facts"]["Pipelines"]["Pipelines"][0].update(JobsRead=False),
    "the pipeline has no jobs": lambda o: o["facts"]["Pipelines"]["Pipelines"][0].update(Jobs=[]),
    "an unexpected pipeline source": lambda o: o["facts"]["Pipelines"]["Pipelines"][0].update(Source="web"),
    "a job failed for the runner's reasons": lambda o: o["facts"]["Pipelines"]["Pipelines"][0]["Jobs"][1].update(
        Status="failed", FailureReason="runner_system_failure"),
    "a job was canceled": lambda o: o["facts"]["Pipelines"]["Pipelines"][0]["Jobs"][1].update(Status="canceled"),
    "a job is still running": lambda o: o["facts"]["Pipelines"]["Pipelines"][0]["Jobs"][2].update(Status="running"),
}


@pytest.mark.parametrize("name", sorted(INCONCLUSIVE_BREAKS))
def test_a_fixture_that_did_not_behave_is_inconclusive_never_pass(name):
    # THE rule (check 8's): the answer in `result` is exactly the expected one,
    # and the row must still not pass.
    case, exp, out = as_designed("fixed")
    if INCONCLUSIVE_BREAKS[name](out) is None and name == "no output at all":
        out = None
    row = pp.judge_row(case, exp, out, SHA, problem="timeout" if out is None else None)
    assert row["status"] == pp.INCONCLUSIVE and row["why"], row


@pytest.mark.parametrize("name,breakit", [
    ("fixed", lambda o: o["facts"]["Pipelines"]["Pipelines"][0]["Jobs"][0].update(Status="failed", FailureReason="script_failure")),
    ("fixed", lambda o: o["facts"]["Pipelines"]["Pipelines"][0]["Jobs"].pop(0)),
    ("notfixed", lambda o: o["facts"]["Pipelines"]["Pipelines"][0]["Jobs"][0].update(Status="success")),
    ("notfixed", lambda o: o["facts"]["Pipelines"]["Pipelines"][0]["Jobs"][0]["Trace"].update(FirstError="some other error")),
    ("notfixed", lambda o: o["facts"]["Pipelines"]["Pipelines"][0]["Jobs"][0]["Trace"].update(Read=False)),
    ("notfixed", lambda o: o["facts"]["Pipelines"]["Pipelines"][0]["Jobs"][0]["Trace"].update(FirstError=pp.OTHER_MARK)),
    ("changed", lambda o: o["facts"]["Pipelines"]["Pipelines"][0]["Jobs"][0]["Trace"].update(FirstError=pp.BASE_MARK)),
    ("retry", lambda o: o["facts"]["Pipelines"]["Pipelines"][0]["Jobs"].pop(0)),
    ("retry", lambda o: o["facts"]["Pipelines"]["Pipelines"][0]["Jobs"][0].update(Status="success")),
])
def test_a_diagnosed_job_that_did_not_do_what_the_case_designed_is_inconclusive(name, breakit):
    case, exp, out = as_designed(name)
    breakit(out)
    row = pp.judge_row(case, exp, out, SHA)
    assert row["status"] == pp.INCONCLUSIVE and "did not behave as designed" in row["why"]


@pytest.mark.parametrize("breakit", [
    lambda o: o["facts"]["Pipelines"].update(Pipelines=[]),
    lambda o: o["facts"]["Pipelines"]["Pipelines"][0].update(Jobs=[]),
    lambda o: o["facts"]["Pipelines"]["Pipelines"][0].update(Jobs=None),
])
def test_a_case_whose_job_may_be_absent_still_needs_a_pipeline_that_ran(breakit):
    # `deleted` expects the diagnosed job to be ABSENT. No pipeline at all, or
    # a pipeline with no jobs, is not that: it is a fixture that did not run.
    case, exp, out = as_designed("deleted")
    assert pp.judge_row(case, exp, out, SHA)["status"] == pp.PASS
    breakit(out)
    assert pp.judge_row(case, exp, out, SHA)["status"] == pp.INCONCLUSIVE


def test_an_unknown_target_is_inconclusive():
    case, exp, out = as_designed("fixed")
    case = dict(case, target="whatever")
    assert pp.judge_row(case, exp, out, SHA)["status"] == pp.INCONCLUSIVE


def test_the_skip_ci_case_needs_its_skipped_pipelines():
    case, exp, out = as_designed("skipci")
    assert pp.judge_row(case, exp, out, SHA)["status"] == pp.PASS
    out["facts"]["MR"]["SHA"] = "b" * 40
    assert pp.judge_row(case, exp, out, SHA)["status"] == pp.INCONCLUSIVE
    # One that ran a job is not the design.
    case, exp, out = as_designed("skipci")
    out["facts"]["Pipelines"]["Pipelines"][0]["Jobs"] = [attempt()]
    row = pp.judge_row(case, exp, out, SHA)
    assert row["status"] == pp.INCONCLUSIVE and "designed to run no job" in row["why"]


SKIPCI_NOT_AS_DESIGNED = {
    # GitLab has listed nothing yet: a `wait` now would be "no pipeline", the
    # right word for the wrong reason.
    "no pipeline of the commit at all": (lambda o: o["facts"]["Pipelines"].update(Pipelines=[]), "no pipeline of the probe's commit"),
    "the only pipelines are of another commit": (
        lambda o: [p.update(SHA="b" * 40) for p in o["facts"]["Pipelines"]["Pipelines"]], "no pipeline of the probe's commit"),
    "a pipeline of the commit is not skipped": (
        lambda o: o["facts"]["Pipelines"]["Pipelines"][1].update(Status="success"), "not skipped"),
    "a pipeline of the commit is still being created": (
        lambda o: o["facts"]["Pipelines"]["Pipelines"][0].update(Status="created"), "not skipped"),
    "the jobs of a skipped pipeline were not read": (
        lambda o: o["facts"]["Pipelines"]["Pipelines"][1].update(JobsRead=False), "were not read"),
}


@pytest.mark.parametrize("n", [0, 1])
@pytest.mark.parametrize("name", sorted(SKIPCI_NOT_AS_DESIGNED))
def test_a_skip_ci_fixture_that_did_not_behave_is_inconclusive(name, n):
    # The answer in `result` is exactly the expected one; the row must still
    # not pass, in either mode.
    breakit, why = SKIPCI_NOT_AS_DESIGNED[name]
    case, exp, out = as_designed("skipci", n)
    assert pp.judge_row(case, exp, out, SHA)["status"] == pp.PASS
    breakit(out)
    row = pp.judge_row(case, exp, out, SHA)
    assert row["status"] == pp.INCONCLUSIVE and why in row["why"], row


def skipci_wait():
    case, exp, out = as_designed("skipci", 1)
    assert exp["decision"] == "wait" and exp["mode"] == pp.MODE_DEFAULT and exp["verdict"] is None
    return case, exp, out


def test_the_skip_ci_rows_are_wait_then_timeout():
    # Plan 6.3 J1 / 5.5 (gonk-vi9k), pinned: past the deadline UNVERIFIABLE by
    # J1.TIMEOUT; before it, `wait` -- and in both the pipelines are set aside.
    late, early = CASES["skipci"]["expect"]
    assert (late["mode"], late["decision"], late["verdict"], late["rule"], late["job_rule"], late["set_aside"]) == (
        pp.MODE_PAST_DEADLINE, "verdict", "UNVERIFIABLE", "J1.TIMEOUT", pp.ABSENT, True)
    assert (early["mode"], early["decision"], early["verdict"], early["rule"], early["set_aside"]) == (
        pp.MODE_DEFAULT, "wait", None, "J1.WAIT", True)
    # Every other row expects a verdict and sets nothing aside.
    for c in pp.cases():
        if c["name"] != "skipci":
            assert all(e["decision"] == "verdict" and e["verdict"] and not e["set_aside"] for e in c["expect"]), c["name"]


def test_a_verdict_where_a_wait_is_expected_fails():
    # What the build of 2026-10-07 answered: UNVERIFIABLE at once. The fixture
    # behaved, the exit check judged it: FAIL, not noise.
    case, exp, out = skipci_wait()
    out["result"].update(Decision="verdict", Verdict="UNVERIFIABLE", Rule=pp.ABSENT, Skipped=None, Candidates=[700, 701])
    row = pp.judge_row(case, exp, out, SHA)
    assert row["status"] == pp.FAIL and "want 'wait'" in row["why"] and "UNVERIFIABLE" in row["why"], row
    # ... and a PASS verdict there is a FAIL too, of course.
    out["result"].update(Verdict="FIXED", Rule="J5.FIXED")
    assert pp.judge_row(case, exp, out, SHA)["status"] == pp.FAIL


@pytest.mark.parametrize("decision,rule", [("cannot-judge", "J0.FACTS"), ("stopped", "J0.TAKEN_OVER")])
def test_another_non_verdict_where_a_wait_is_expected_is_inconclusive(decision, rule):
    case, exp, out = skipci_wait()
    out["result"].update(Decision=decision, Rule=rule)
    row = pp.judge_row(case, exp, out, SHA)
    assert row["status"] == pp.INCONCLUSIVE and "no wait" in row["why"], row


@pytest.mark.parametrize("n", [0, 1])
@pytest.mark.parametrize("change,why", [
    ({"Rule": "J1.SOMETHING"}, "rule"),
    ({"Skipped": None}, "result.Skipped"),
    ({"Skipped": []}, "result.Skipped"),
    ({"Skipped": [700]}, "result.Skipped"),          # one of the two was not set aside
    ({"Skipped": [700, 701, 702]}, "result.Skipped"),
    ({"Candidates": [701]}, "result.Candidates"),    # ... because it is still a candidate
])
def test_a_skip_ci_row_for_the_wrong_reason_fails(n, change, why):
    case, exp, out = as_designed("skipci", n)
    out["result"].update(change)
    row = pp.judge_row(case, exp, out, SHA)
    assert row["status"] == pp.FAIL and why in row["why"], row


def test_a_verdict_on_a_wait_answer_fails():
    case, exp, out = skipci_wait()
    out["result"]["Verdict"] = "FIXED"
    row = pp.judge_row(case, exp, out, SHA)
    assert row["status"] == pp.FAIL and "on a 'wait' answer" in row["why"]


def test_keep_looking():
    fixed, exp, out = as_designed("fixed")
    assert pp.keep_looking(fixed, exp, out) is False
    for decision in ("wait", "cannot-judge"):
        o = copy.deepcopy(out)
        o["result"]["Decision"] = decision
        assert pp.keep_looking(fixed, exp, o) is True
    assert pp.keep_looking(fixed, exp, None) is True
    stopped = copy.deepcopy(out)
    stopped["result"]["Decision"] = "stopped"
    assert pp.keep_looking(fixed, exp, stopped) is False          # an answer; the row says what it is worth
    nopl = copy.deepcopy(out)
    nopl["facts"]["Pipelines"]["Pipelines"] = []
    assert pp.keep_looking(fixed, exp, nopl) is False             # only [skip ci] waits for a listing
    # [skip ci], deadline passed: a verdict at once -- but not before GitLab
    # has listed the skipped pipelines.
    case, late, o = as_designed("skipci", 0)
    assert pp.keep_looking(case, late, o) is False
    o["facts"]["Pipelines"]["Pipelines"] = []
    assert pp.keep_looking(case, late, o) is True
    # [skip ci], before the deadline: `wait` IS the answer; looking again would
    # never end.
    case, early, o = as_designed("skipci", 1)
    assert pp.keep_looking(case, early, o) is False
    o["result"]["Decision"] = "cannot-judge"
    assert pp.keep_looking(case, early, o) is True


# ---------------------------------------------------------------- the verdict of the run

def rows_for(names, status=pp.PASS):
    out = []
    for n in names:
        for e in CASES[n]["expect"]:
            out.append({"case": n, "mode": pp.MODE_NAMES[e["mode"]], "status": status, "why": "",
                        "expected": e, "observed": {}, "decided_by": "", "cheat": "-", "what": ""})
    return out


def test_overall():
    sel = [CASES["fixed"], CASES["ortrue"]]
    planned = pp.planned_rows(sel)
    assert len(planned) == 4
    rows = rows_for(["fixed", "ortrue"])
    assert pp.overall(rows, planned)[0] == pp.EXIT_PASS == 0
    # INCONCLUSIVE is exit 3, never 0.
    r = copy.deepcopy(rows)
    r[1]["status"] = pp.INCONCLUSIVE
    assert pp.overall(r, planned)[0] == pp.EXIT_INCONCLUSIVE == 3
    # A FAIL outranks an INCONCLUSIVE.
    r[2]["status"] = pp.FAIL
    assert pp.overall(r, planned)[0] == pp.EXIT_FAIL == 1
    # A planned row that is missing: not run is not a pass.
    assert pp.overall(rows[:-1], planned)[0] == pp.EXIT_INCONCLUSIVE
    assert pp.overall(rows_for(["fixed"]), planned)[0] == pp.EXIT_INCONCLUSIVE
    # Nothing run at all.
    assert pp.overall([], planned)[0] == pp.EXIT_INCONCLUSIVE
    assert pp.overall([], [])[0] == pp.EXIT_INCONCLUSIVE
    # A status nobody defined is not a pass either.
    r = copy.deepcopy(rows)
    r[0]["status"] = "OK"
    assert pp.overall(r, planned)[0] == pp.EXIT_INCONCLUSIVE
    assert "4 PASS, 0 FAIL, 0 INCONCLUSIVE, 0 NOT RUN of 4 planned" in pp.overall(rows, planned)[1]
    r = copy.deepcopy(rows)
    r[1]["status"], r[2]["status"] = pp.INCONCLUSIVE, pp.FAIL
    assert "2 PASS, 1 FAIL, 1 INCONCLUSIVE, 0 NOT RUN of 4 planned" in pp.overall(r, planned)[1]
    assert "1 NOT RUN" in pp.overall(rows[:-1], planned)[1]


def test_the_table_shows_every_row_and_why():
    case, exp, out = as_designed("allowfail")
    good = pp.judge_row(case, exp, out, SHA)
    out2 = copy.deepcopy(out)
    out2["result"]["Verdict"] = "FIXED"
    bad = pp.judge_row(case, exp, out2, SHA)
    none = pp.judge_row(case, exp, None, SHA, problem="timeout")
    table = pp.render_l2([good, bad, none])
    lines = table.splitlines()
    assert len(lines) == 5 and lines[0].startswith("| case | cheat | mode |")
    assert "| PASS |" in lines[2] and "FORGED / J2.VIOLATION / J3.WEAKENED" in lines[2]
    assert "allow_failure=true" in lines[2] and "cidef D2 probe-status/allow_failure" in lines[2]
    assert "FAIL: verdict 'FIXED', want 'FORGED'" in lines[3]
    assert "INCONCLUSIVE: timeout" in lines[4]
    # A row that is not a verdict shows its decision, expected and observed.
    case, exp, out = as_designed("skipci", 1)
    wait = pp.judge_row(case, exp, out, SHA)
    out2 = copy.deepcopy(out)
    out2["result"].update(Decision="verdict", Verdict="UNVERIFIABLE", Rule=pp.ABSENT)
    judged = pp.judge_row(case, exp, out2, SHA)
    lines = pp.render_l2([wait, judged]).splitlines()
    assert "| skipci | (G6) | default | (wait) / J1.WAIT / - | (wait) / J1.WAIT / - | - | PASS |" in lines[2]
    assert "| (wait) / J1.WAIT / - | UNVERIFIABLE / J3.NOT_OBSERVED(absent) / - | " in lines[3] and "| FAIL: decision 'verdict'" in lines[3]


def test_decided_by_names_the_field():
    _, _, out = as_designed("deleted")
    assert "pipelines[].status=success ;; jobs?include_retried=true -> no job named probe-status among ['probe-lint', 'probe-report']" in pp.decided_by(out)
    _, _, out = as_designed("changed")
    assert "signature es1-new != recorded es1-old" in pp.decided_by(out)
    for n in (0, 1):
        _, _, out = as_designed("skipci", n)
        text = pp.decided_by(out)
        assert "merge_requests/:iid/pipelines -> 701 sha=aaaaaaaa status=skipped, 700 sha=aaaaaaaa status=skipped" in text
        assert "result.Skipped=[700, 701] result.Candidates=[]" in text
        assert "pipelines[].status=skipped,skipped ;; jobs?include_retried=true -> no job named probe-status among []" in text
    out["facts"]["Pipelines"]["Pipelines"] = []
    assert "merge_requests/:iid/pipelines -> []" in pp.decided_by(out)
    _, _, out = as_designed("fixed")
    assert "pipelines[].status=success" in pp.decided_by(out) and "status=success allow_failure=false" in pp.decided_by(out)
    _, _, out = as_designed("retry")
    assert "probe-status x2" in pp.decided_by(out)


# ---------------------------------------------------------------- the assumptions

def full_obs():
    return {
        "retry": {"with_retried": 2, "ids": [11, 10], "statuses": ["failed", "failed"], "without_retried": 1},
        "manual": {"status": "manual", "allow_failure": True},
        "skipped": {"status": "skipped"},
        "rules": {"present": False, "names": ["probe-lint", "probe-report"]},
        "allowfail": {"allow_failure": True, "status": "failed", "pipeline_status": "success", "base_allow_failure": False},
        "lint": {"include_key": False, "included_text": True, "context_ok": True, "extends_key": True, "extends_merged": True,
                 "default_key": True, "reference_tag": True, "reference_resolved": False, "anchor_resolved": True,
                 "pre_post": True, "dry_valid": False, "dry_errors": ["x"]},
        "mr_pipelines": {"sources": ["merge_request_event", "push"], "refs": ["a", "b"], "all_have_sha": True,
                         "project_sources": ["merge_request_event", "push"], "branch_pipeline_exists": True},
        "skipci": {"mr": [], "project": [], "ran": 0},
        "crossref": {"system_note": True, "body": "mentioned in merge request !9"},
        "merge_status": {"fixed": "mergeable"},
        "pipeline_user": {"me": 2, "push": 2, "mr": 2},
    }


def verdicts(obs):
    return {g: v for g, v, _ in pp.settle_assumptions(obs)}


def test_nothing_observed_confirms_nothing():
    v = verdicts({})
    assert set(v) == {f"G{i}" for i in range(1, 12)}
    assert set(v.values()) == {pp.OPEN}


def test_assumptions_as_assumed():
    v = verdicts(full_obs())
    assert v == {"G1": pp.CONFIRMED, "G2": pp.CONFIRMED, "G3": pp.CONFIRMED, "G4": pp.CONFIRMED, "G5": pp.CONFIRMED,
                 "G6": pp.PARTIAL, "G7": pp.PARTIAL, "G8": pp.PARTIAL, "G9": pp.OPEN, "G10": pp.PARTIAL, "G11": pp.OPEN}


@pytest.mark.parametrize("g,key,change,want", [
    ("G1", "retry", {"with_retried": 1}, "REFUTED"),
    ("G1", "retry", {"without_retried": 2}, "REFUTED"),
    ("G1", "retry", {"ids": [11, 11]}, "REFUTED"),
    ("G2", "manual", {"status": "skipped"}, "REFUTED"),
    ("G2", "skipped", {"status": "created"}, "REFUTED"),
    ("G2", "rules", {"present": True}, "REFUTED"),
    ("G3", "allowfail", {"allow_failure": False}, "REFUTED"),
    ("G3", "allowfail", {"base_allow_failure": True}, "REFUTED"),
    ("G3", "allowfail", {"status": "success"}, "REFUTED"),
    ("G4", "lint", {"reference_tag": False}, "REFUTED"),
    ("G4", "lint", {"include_key": True}, "REFUTED"),
    ("G4", "lint", {"extends_key": False}, "REFUTED"),
    ("G4", "lint", {"default_key": False}, "REFUTED"),
    ("G5", "mr_pipelines", {"sources": ["merge_request_event"]}, "REFUTED"),
    ("G5", "mr_pipelines", {"sources": ["merge_request_event"], "branch_pipeline_exists": False}, "OPEN"),
    ("G5", "mr_pipelines", {"all_have_sha": False}, "OPEN"),
    ("G6", "skipci", {"ran": 1}, "REFUTED"),
    ("G7", "crossref", {"system_note": False}, "OPEN"),
    ("G10", "pipeline_user", {"push": 3}, "REFUTED"),
    ("G10", "pipeline_user", {"mr": None}, "REFUTED"),
    ("G10", "pipeline_user", {"me": None, "push": None, "mr": None}, "REFUTED"),
])
def test_an_observation_that_differs_is_not_confirmed(g, key, change, want):
    obs = full_obs()
    obs[key].update(change)
    assert verdicts(obs)[g] == want


@pytest.mark.parametrize("g,keys", [("G1", ["retry"]), ("G2", ["manual"]), ("G2", ["skipped"]), ("G2", ["rules"]),
                                    ("G3", ["allowfail"]), ("G4", ["lint"]), ("G5", ["mr_pipelines"]), ("G6", ["skipci"]),
                                    ("G7", ["crossref"]), ("G8", ["merge_status"]), ("G10", ["pipeline_user"])])
def test_a_missing_observation_leaves_its_assumption_open(g, keys):
    obs = full_obs()
    for k in keys:
        del obs[k]
    assert verdicts(obs)[g] == pp.OPEN


def go_lint_out(**change):
    lint = {"sha": SHA, "valid": True, "merged_yaml_bytes": 9,
            "includes": [{"type": "local", "location": "ci/probe-common.yml", "context_sha": SHA}]}
    lint.update(change)
    return {"evidence": {"head_lint": lint}}


def test_go_lint_observation_reads_named_fields():
    assert pp.go_lint_observation(go_lint_out(), SHA) == {"valid": True, "includes": 1, "context_ok": True}
    # An include resolved at ANOTHER commit (what a dry run does), one that is
    # not local, and no include at all: none of them is "context ok".
    other = [{"type": "local", "location": "ci/probe-common.yml", "context_sha": "b" * 40}]
    assert pp.go_lint_observation(go_lint_out(includes=other), SHA)["context_ok"] is False
    remote = [{"type": "remote", "location": "https://example.invalid/ci.yml", "context_sha": SHA}]
    assert pp.go_lint_observation(go_lint_out(includes=remote), SHA)["context_ok"] is False
    assert pp.go_lint_observation(go_lint_out(includes=[]), SHA) == {"valid": True, "includes": 0, "context_ok": False}
    assert pp.go_lint_observation(go_lint_out(valid=False), SHA)["valid"] is False
    # Not a lint of that commit, or not a lint result at all: no observation.
    assert pp.go_lint_observation(go_lint_out(sha="b" * 40), SHA) is None
    assert pp.go_lint_observation(go_lint_out(valid=None), SHA) is None
    assert pp.go_lint_observation({"evidence": {}}, SHA) is None
    assert pp.go_lint_observation(None, SHA) is None
    # The G4 row says what was seen -- and says so when nothing was.
    obs = full_obs()
    assert "NOT observed" in dict((g, t) for g, _, t in pp.settle_assumptions(obs))["G4"]
    obs["go_lint"] = pp.go_lint_observation(go_lint_out(), SHA)
    text = dict((g, t) for g, _, t in pp.settle_assumptions(obs))["G4"]
    assert "glab.CILintStatic" in text and "valid=True, 1 include(s), every one local with context_sha == that commit: True" in text


def test_the_g_table_names_the_fields():
    table = pp.render_g(pp.settle_assumptions(full_obs()))
    assert table.count("\n") == 12
    for needle in ("include_retried=true lists 2 objects", "status='manual'", "ABSENT from", "allow_failure=True",
                   "context_sha == sha: True", "sources ['merge_request_event', 'push']", "0 of them ran a job",
                   "mentioned in merge request !9", "detailed_merge_status", "user.id"):
        assert needle in table, needle


STATIC_YAML = '''---
".probe-job":
  tags:
  - cluster
  before_script:
  - echo "probe job $CI_JOB_NAME"
stages:
- ".pre"
- check
- report
- ".post"
default:
  interruptible: true
probe-lint:
  tags:
  - cluster
  extends: ".probe-job"
  variables:
    PROBE_KIND: synthetic
probe-status:
  tags:
  - cluster
  before_script:
  - echo "probe job $CI_JOB_NAME"
  extends: ".probe-job"
probe-extra:
  script:
  - - echo "probe job $CI_JOB_NAME"
'''


def test_lint_observation_reads_named_things_from_the_merged_yaml():
    static = {"valid": True, "merged_yaml": STATIC_YAML, "includes": [{"type": "local", "context_sha": SHA}]}
    dry = {"valid": False, "errors": ["Local file `ci/probe-common.yml` does not exist!"]}
    o = pp.lint_observation(static, dry, SHA)
    assert o == {"include_key": False, "included_text": True, "context_ok": True, "extends_key": True,
                 "extends_merged": True, "default_key": True, "reference_tag": False, "reference_resolved": True,
                 "anchor_resolved": True, "pre_post": True, "dry_valid": False,
                 "dry_errors": ["Local file `ci/probe-common.yml` does not exist!"]}
    assert pp.lint_observation(dict(static, includes=[{"type": "local", "context_sha": "b" * 40}]), dry, SHA)["context_ok"] is False
    kept = dict(static, merged_yaml="include:\n- local: x\n" + STATIC_YAML.replace("- - echo", "- !reference [.probe-job, before_script]"))
    o2 = pp.lint_observation(kept, dry, SHA)
    assert o2["include_key"] is True and o2["reference_tag"] is True and o2["reference_resolved"] is False
    # An error object, or an invalid lint, is no observation at all.
    assert pp.lint_observation(dict(static, includes=[]), dry, SHA)["context_ok"] is False
    for not_a_lint in ({"message": "404 Commit Not Found"}, {"valid": False, "errors": ["x"]}, {"valid": True}, None, [],
                       {"valid": False, "errors": ["x"], "merged_yaml": STATIC_YAML}):
        assert pp.lint_observation(not_a_lint, dry, SHA) is None
