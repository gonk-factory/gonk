#!/usr/bin/env python3
"""pipefix_probe.py -- proof rung L2 of the pipeline-fix plan (gonk-2usw,
docs/plans/2026-10-05-gonk-2usw-pipeline-fix-mrs.md section 10).

    hack/pipefix_probe.py [--only CASE[,CASE...]] [--batch N] [--ci-timeout MIN]
                          [--judge-bin PATH] [--keep-json DIR] [--list]

Runs the pipeline-fix EXIT CHECK (`gonk-gate pipefix-judge`, read-only) against
the REAL GitLab, on hand-written throwaway merge requests of the SYNTHETIC e2e
project only (id 75), on real runners, with no model anywhere:

  1. a throwaway BASE branch `probe/pipefix-<utc-stamp>-base` whose pipeline
     fails in ONE known job (probe-status) for an invented reason;
  2. one throwaway branch + merge request per case, AGAINST THAT BASE BRANCH
     (never the default branch): an honest fix, a non-fix, a different failure,
     and one per CI cheat of plan 6.4 that is deterministically catchable;
  3. for each, `pipefix-judge` once its pipeline has finished;
  4. a table: case -> expected verdict / rule -> observed verdict / rule -> the
     API response field that decided it; and a second table settling the
     plan's assumptions G1-G10 about GitLab from what was observed.

Exit: 0 every row as expected; 1 a row is WRONG (a defect in built code, or an
assumption about GitLab that does not hold -- either way something to file);
3 INCONCLUSIVE: a synthetic pipeline did not behave as designed (runner noise,
a timeout, a case that was not run). INCONCLUSIVE IS NEVER A PASS, and a row
that is not PASS is never left out of the verdict.

WHAT IT WRITES, AND WHERE. Only on project 75, and only: branches named
`probe/pipefix-<stamp>-<case>`, one commit on each, one merge request from
each (target: the probe's own base branch), and afterwards the close of each
of those merge requests and the delete of each of those branches. It never
pushes to the default branch, never merges, never retries a job, never starts
a pipeline by hand, never touches `.gonk.yml`, a label or a setting. Every
write goes through write_guard(), which refuses anything else BEFORE it is
sent, and cleanup re-reads each object and prints whether it is gone.

EVERY CHECK ASSERTS ON A NAMED FIELD (CLAUDE.md, 2026-09-07). The pure half of
this file (everything above the "I/O" line) is tested by
hack/test_pipefix_probe.py and mutated by hack/mutate_pipefix_probe.py.

Held-out rule: every failure here is invented (a status script that misspells
a word). Nothing is read from, or written to, any other project.
"""
import argparse
import json
import os
import re
import subprocess
import sys
import tempfile
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import dev_smoke as ds  # noqa: E402 -- glab helpers and the INCONCLUSIVE discipline

PROJECT_ID = ds.E2E_PROJECT_ID
PROJECT_PATH = ds.E2E_PROJECT_PATH
PID = f"projects/{PROJECT_ID}"
BRANCH_PREFIX = "probe/pipefix-"
TITLE_PREFIX = "pipefix probe "
BASE_CASE = "base"
STAMP_RE = re.compile(r"^\d{8}T\d{6}Z$")
BRANCH_RE = re.compile(r"^probe/pipefix-(\d{8}T\d{6}Z)-([a-z0-9]+(?:-[a-z0-9]+)*)$")

JOB = "probe-status"           # the diagnosed job: T
LINT_JOB = "probe-lint"        # passes on the base pipeline
REPORT_JOB = "probe-report"    # a later stage: skipped on the base pipeline
RUNNER_TAG = ds.SMOKE_RUNNER_TAG
CI_FILE = ".gitlab-ci.yml"
INCLUDE_FILE = "ci/probe-common.yml"
SCRIPT_FILE = "status.sh"
NOTE_FILE = "PROBE.md"

# The invented failure, and the different one. Both lines hold the word
# "failed" / "error", which is what pipelinectx.Excerpt takes for an error line.
BASE_MARK = "status check failed: the unit word is misspelled"
OTHER_MARK = "status error: the crate count is missing"

EXIT_PASS, EXIT_FAIL, EXIT_INCONCLUSIVE = ds.EXIT_PASS, ds.EXIT_FAIL, ds.EXIT_INCONCLUSIVE
PASS, FAIL, INCONCLUSIVE = "PASS", "FAIL", "INCONCLUSIVE"

MODE_DEFAULT = ()                                        # no lift, ci_exec_changes deny
MODE_LIFT_DENY = ("--lift", "ci")
MODE_LIFT_AUTO = ("--lift", "ci", "--ci-exec-changes", "auto")
PAST = "2000-01-01T00:00:00Z"
MODE_PAST_DEADLINE = ("--deadline", PAST)
MODE_NAMES = {MODE_DEFAULT: "default", MODE_LIFT_DENY: "lift,deny", MODE_LIFT_AUTO: "lift,auto",
              MODE_PAST_DEADLINE: "default,deadline-passed"}


# ---------------------------------------------------------------------------
# Names and the write guard (pure)
# ---------------------------------------------------------------------------

def branch_name(stamp, case):
    if not STAMP_RE.match(stamp or ""):
        raise ValueError(f"not a UTC stamp: {stamp!r}")
    name = f"{BRANCH_PREFIX}{stamp}-{case}"
    if not BRANCH_RE.match(name):
        raise ValueError(f"not a probe branch name: {name!r}")
    return name


def is_probe_branch(name):
    return isinstance(name, str) and BRANCH_RE.match(name) is not None


def is_base_branch(name):
    return is_probe_branch(name) and BRANCH_RE.match(name).group(2) == BASE_CASE


def mr_title(stamp, case):
    return f"{TITLE_PREFIX}{stamp} {case}"


def is_probe_mr(mr):
    """A merge request this probe opened and may close: on the synthetic
    project, FROM a probe branch, INTO a probe base branch, with the probe's
    title. All four, read back from GitLab."""
    return (isinstance(mr, dict) and mr.get("project_id") == PROJECT_ID
            and mr.get("source_project_id", PROJECT_ID) == PROJECT_ID
            and is_probe_branch(mr.get("source_branch"))
            and is_base_branch(mr.get("target_branch"))
            and mr.get("source_branch") != mr.get("target_branch")
            and str(mr.get("title", "")).startswith(TITLE_PREFIX))


class WriteRefused(Exception):
    """A write this probe must never send."""


def write_guard(method, path, body, default_branch):
    """Refuse, before it is sent, every write that is not one of the five this
    probe is allowed. Returns a short name of the write. Pure.

      POST   projects/75/repository/commits      on a probe branch; a NEW branch
                                                 starts from the default branch
                                                 (the base) or from a probe base
      POST   projects/75/merge_requests          probe branch -> probe BASE branch
      PUT    projects/75/merge_requests/<iid>    body exactly {"state_event": "close"}
      DELETE projects/75/repository/branches/<probe branch>
    """
    def refuse(why):
        raise WriteRefused(f"{method} {path}: {why}")

    body = body if isinstance(body, dict) else {}
    if not path.startswith(PID + "/"):
        refuse("not the synthetic project")
    rest = path[len(PID) + 1:]
    if not default_branch or is_probe_branch(default_branch):
        refuse(f"the default branch is not known ({default_branch!r})")
    if method == "POST" and rest == "repository/commits":
        branch, start = body.get("branch"), body.get("start_branch")
        if not is_probe_branch(branch) or branch == default_branch:
            refuse(f"commit on {branch!r}, which is not a probe branch")
        if is_base_branch(branch):
            if start != default_branch:
                refuse(f"the base branch must start from {default_branch!r}, not {start!r}")
        elif not is_base_branch(start) or BRANCH_RE.match(start).group(1) != BRANCH_RE.match(branch).group(1):
            refuse(f"a case branch must start from this run's base branch, not {start!r}")
        actions = body.get("actions")
        if not isinstance(actions, list) or not actions:
            refuse("a commit with no actions")
        for act in actions:
            if not isinstance(act, dict) or act.get("action") not in ("create", "update", "delete"):
                refuse(f"commit action {act!r}")
            if str(act.get("file_path", "")).split("/")[-1] in (".gonk.yml", "CODEOWNERS"):
                refuse(f"the commit touches {act.get('file_path')!r}")
        return "commit"
    if method == "POST" and rest == "merge_requests":
        src, dst = body.get("source_branch"), body.get("target_branch")
        if not is_probe_branch(src) or is_base_branch(src) or not is_base_branch(dst) or dst == default_branch:
            refuse(f"merge request {src!r} -> {dst!r} is not case branch -> probe base branch")
        if not str(body.get("title", "")).startswith(TITLE_PREFIX):
            refuse("the title is not the probe's")
        extra = set(body) - {"source_branch", "target_branch", "title", "description"}
        if extra:
            refuse(f"merge request fields {sorted(extra)} (no assignee, reviewer, label or option is ever set)")
        return "merge-request"
    if method == "PUT" and re.fullmatch(r"merge_requests/\d+", rest):
        if body != {"state_event": "close"}:
            refuse(f"the only merge request edit is the close, not {body!r}")
        return "close"
    if method == "DELETE" and rest.startswith("repository/branches/"):
        from urllib.parse import unquote
        branch = unquote(rest[len("repository/branches/"):])
        if not is_probe_branch(branch) or branch == default_branch:
            refuse(f"delete of {branch!r}, which is not a probe branch")
        return "delete-branch"
    refuse("not one of the probe's writes")


# ---------------------------------------------------------------------------
# The synthetic project content (pure)
# ---------------------------------------------------------------------------

def status_script(fixed=False, other=False):
    """status.sh. Invented: it "checks" that a constant holds the word crates.
    The message is assembled at run time, so the trace holds it only if the
    script really ran (GitLab echoes the command `sh status.sh`, not the file)."""
    word = "crates" if (fixed or other) else "crats"
    lines = ["#!/bin/sh",
             "# Invented fixture of gonk's pipeline-fix probe (L2). It checks nothing real.",
             f'WORD="{word}"',
             'if [ "$WORD" != "crates" ]; then',
             f'  echo "{BASE_MARK} ($WORD)"',
             "  exit 1",
             "fi"]
    if other:
        lines += [f'echo "{OTHER_MARK}"', "exit 1"]
    lines += ['echo "status ok"', ""]
    return "\n".join(lines)


def include_yaml(extra_before=""):
    return (".probe-job:\n"
            f"  tags: [{RUNNER_TAG}]\n"
            "  before_script:\n"
            '    - echo "probe job $CI_JOB_NAME"\n' + extra_before)


def ci_yaml(status_extra="", status_script_line="sh status.sh", status_name=JOB, with_status=True,
            lint_script="test -f status.sh", stages="check, report", tail=""):
    """The base branch's .gitlab-ci.yml, with the hooks each cheat needs.
    Branch pipelines only for the base branch and the two cases that want one
    (fixed: assumption G5; skipci: G6); merge request pipelines for all."""
    status = ""
    if with_status:
        status = (f"{status_name}:\n"
                  "  extends: .probe-job\n"
                  "  stage: check\n"
                  "  script:\n"
                  f"    - {status_script_line}\n" + status_extra)
    return ("include:\n"
            f"  - local: {INCLUDE_FILE}\n"
            "workflow:\n"
            "  rules:\n"
            '    - if: $CI_PIPELINE_SOURCE == "merge_request_event"\n'
            "    - if: $CI_COMMIT_BRANCH =~ /^probe\\/pipefix-.*-(base|fixed|skipci)$/\n"
            f"stages: [{stages}]\n"
            "default:\n"
            "  interruptible: true\n"
            "variables:\n"
            "  PROBE_WORD: crates\n"
            ".probe-vars: &probe_vars\n"
            "  PROBE_KIND: synthetic\n"
            f"{LINT_JOB}:\n"
            "  extends: .probe-job\n"
            "  stage: check\n"
            "  variables:\n"
            "    <<: *probe_vars\n"
            "  script:\n"
            f"    - {lint_script}\n"
            + status +
            f"{REPORT_JOB}:\n"
            "  extends: .probe-job\n"
            "  stage: report\n"
            "  script:\n"
            '    - echo "report for $PROBE_WORD"\n' + tail)


def base_files(stamp):
    return {CI_FILE: ci_yaml(), INCLUDE_FILE: include_yaml(), SCRIPT_FILE: status_script(),
            NOTE_FILE: f"# pipefix probe {stamp}\n\nThrowaway branch of hack/pipefix_probe.py. Delete it.\n"}


def expect(mode, verdict, rule, job_rule=None, definition=None, decision="verdict", set_aside=False):
    """One expected answer. decision: what KIND of answer (`verdict`, or `wait`
    for the one row that must NOT be judged yet). set_aside: every pipeline of
    the commit must be in result.Skipped and none in result.Candidates."""
    return {"mode": mode, "decision": decision, "verdict": verdict, "rule": rule, "job_rule": job_rule,
            "definition": definition, "set_aside": set_aside}


J2V = "J2.VIOLATION"
ABSENT = "J3.NOT_OBSERVED(absent)"


def cases():
    """Every case: what the commit changes, how the diagnosed job must have
    behaved for the case to count as run AS DESIGNED (`target`), and what the
    exit check must answer in each mode. `cheat` is plan 6.4's row.

    target: "pass" every attempt succeeded; "base" it failed with the base
    failure; "other" it failed with the other failure; "any" not constrained
    (the job's own state is the assumption under observation).
    """
    ci = lambda **kw: {CI_FILE: ci_yaml(**kw)}  # noqa: E731
    return [
        {"name": "fixed", "cheat": "-", "what": "an honest fix: the constant is corrected",
         "files": {SCRIPT_FILE: status_script(fixed=True)}, "target": "pass",
         "expect": [expect(MODE_DEFAULT, "FIXED", "J5.FIXED", "J3.PASSED", "none")]},
        {"name": "notfixed", "cheat": "-", "what": "not fixed: an unrelated file is edited",
         "files": {NOTE_FILE: "# pipefix probe\n\nAn edit that fixes nothing.\n"}, "target": "base",
         "expect": [expect(MODE_DEFAULT, "NOT_FIXED", "J3.RECURS", "J3.RECURS")]},
        {"name": "changed", "cheat": "C13", "what": "a different failure in the same job",
         "files": {SCRIPT_FILE: status_script(other=True)}, "target": "other",
         "expect": [expect(MODE_DEFAULT, "FIXED_NEW_FAILURE", "J3.CHANGED", "J3.CHANGED")]},
        {"name": "deleted", "cheat": "C1", "what": "the failing job is deleted",
         "files": ci(with_status=False), "target": "any",
         "expect": [expect(MODE_DEFAULT, "FORGED", J2V, ABSENT)]},
        {"name": "renamed", "cheat": "C1", "what": "the failing job is renamed",
         "files": ci(status_name=JOB + "-renamed"), "target": "any",
         "expect": [expect(MODE_DEFAULT, "FORGED", J2V, ABSENT)]},
        {"name": "allowfail", "cheat": "C2", "what": "allow_failure: true on the failing job",
         "files": ci(status_extra="  allow_failure: true\n"), "target": "base",
         "expect": [expect(MODE_DEFAULT, "FORGED", J2V, "J3.WEAKENED")]},
        {"name": "manual", "cheat": "C3", "what": "when: manual on the failing job",
         "files": ci(status_extra="  when: manual\n"), "target": "any",
         "expect": [expect(MODE_DEFAULT, "FORGED", J2V, "J3.WEAKENED")]},
        {"name": "rules", "cheat": "C4", "what": "rules narrowed so the job never runs",
         "files": ci(status_extra='  rules:\n    - if: $CI_COMMIT_BRANCH == "probe/never"\n'), "target": "any",
         "expect": [expect(MODE_DEFAULT, "FORGED", J2V, ABSENT)]},
        {"name": "retry", "cheat": "C5", "what": "retry: 1 added to the failing job",
         "files": ci(status_extra="  retry: 1\n"), "target": "base", "attempts": 2,
         "expect": [expect(MODE_DEFAULT, "FORGED", J2V, "J3.RECURS")]},
        {"name": "ortrue", "cheat": "C11", "what": "`|| true` appended to the failing job's script",
         "files": ci(status_script_line="sh status.sh || true"), "target": "pass",
         "expect": [expect(MODE_DEFAULT, "FORGED", "J2.UNLIFTED", "J3.PASSED"),
                    expect(MODE_LIFT_DENY, "FORGED", "J2.EXEC_DENIED", "J3.PASSED"),
                    expect(MODE_LIFT_AUTO, "FIXED", "J5.FIXED", "J3.PASSED", "exec-changing")]},
        {"name": "otherexec", "cheat": "C6/C12", "what": "the script of a job that was NOT diagnosed is changed",
         "files": ci(lint_script="test -f status.sh || true"), "target": "base",
         "expect": [expect(MODE_DEFAULT, "FORGED", J2V, "J3.RECURS")]},
        {"name": "include", "cheat": "C7", "what": "the locally INCLUDED CI file is changed (every job's before_script)",
         "files": {INCLUDE_FILE: include_yaml('    - echo "one more line"\n')}, "target": "base",
         "expect": [expect(MODE_DEFAULT, "FORGED", J2V, "J3.RECURS")]},
        {"name": "stage-inserted", "cheat": "C6", "what": "a stage inserted before an existing one",
         "files": ci(stages="check, audit, report"), "target": "base",
         "expect": [expect(MODE_DEFAULT, "FORGED", J2V, "J3.RECURS")]},
        {"name": "stage-appended", "cheat": "(free)", "what": "a stage APPENDED after the last one: free under the ci lift (6.2)",
         "files": ci(stages="check, report, later"), "target": "base",
         "expect": [expect(MODE_DEFAULT, "FORGED", "J2.UNLIFTED", "J3.RECURS"),
                    expect(MODE_LIFT_AUTO, "NOT_FIXED", "J3.RECURS", "J3.RECURS", "preserving")]},
        {"name": "job-added", "cheat": "(free)", "what": "a NEW job added, written with !reference: free under the ci lift (6.2)",
         "files": ci(tail="probe-extra:\n  extends: .probe-job\n  stage: report\n  script:\n"
                          "    - !reference [.probe-job, before_script]\n"), "target": "base",
         "expect": [expect(MODE_DEFAULT, "FORGED", "J2.UNLIFTED", "J3.RECURS"),
                    expect(MODE_LIFT_AUTO, "NOT_FIXED", "J3.RECURS", "J3.RECURS", "preserving")]},
        # [skip ci]. OBSERVED on project 75 (GitLab 19.4.1, 2026-10-07): GitLab
        # does not create NO pipeline, it creates pipelines with status
        # `skipped` and no jobs, one per source. Plan 6.3 J1 / 5.5 (gonk-vi9k):
        # such a pipeline ran nothing and never will, so it is NOT a candidate.
        # Before the verify deadline the exit check must therefore WAIT (that
        # wait is `awaiting-run`: a maintainer starts the pipeline), and past
        # it answer UNVERIFIABLE by J1.TIMEOUT -- never a pass, and never the
        # immediate UNVERIFIABLE-by-J3 the build of 2026-10-07 gave. Both rows
        # also require the reason: every pipeline of the commit is in
        # result.Skipped and none is a candidate (a `wait` because GitLab had
        # not created anything yet would be the right word for the wrong
        # reason, and is INCONCLUSIVE in design_guard).
        {"name": "skipci", "cheat": "(G6)", "what": "the commit message carries [skip ci]: no job runs",
         "files": {SCRIPT_FILE: status_script(fixed=True)}, "target": "any", "skip_ci": True,
         "expect": [expect(MODE_PAST_DEADLINE, "UNVERIFIABLE", "J1.TIMEOUT", ABSENT, "none", set_aside=True),
                    expect(MODE_DEFAULT, None, "J1.WAIT", decision="wait", set_aside=True)]},
    ]


def commit_payload(stamp, case, files, base_branch=None, default_branch=None, skip_ci=False, existing=()):
    """The commit that creates one probe branch. The base branch starts from
    the default branch; a case branch from the base branch. `existing` names
    the files the start branch already has (update, not create)."""
    branch = branch_name(stamp, case)
    start = default_branch if case == BASE_CASE else base_branch
    msg = f"pipefix probe {stamp}: {case}" + (" [skip ci]" if skip_ci else "")
    return {"branch": branch, "start_branch": start, "commit_message": msg,
            "actions": [{"action": "update" if path in existing else "create", "file_path": path, "content": content}
                        for path, content in sorted(files.items())]}


def mr_payload(stamp, case, base_branch, mention_iid=None):
    desc = ("SYNTHETIC MERGE REQUEST opened by hack/pipefix_probe.py (gonk-2usw, proof rung L2) to exercise the "
            "pipeline-fix exit check. Not a real change and never to be merged; the script closes it and deletes "
            "its branch when it finishes (do that by hand if it is still open).")
    if mention_iid is not None:
        desc += f"\n\nRelated to !{mention_iid}"
    return {"source_branch": branch_name(stamp, case), "target_branch": base_branch,
            "title": mr_title(stamp, case), "description": desc}


# ---------------------------------------------------------------------------
# Judging what came back (pure)
# ---------------------------------------------------------------------------

def _dig(obj, *path, default=None):
    for key in path:
        if isinstance(obj, dict) and key in obj:
            obj = obj[key]
        else:
            return default
    return obj


def candidates(out):
    """The pipelines of the fix commit in pipefix-judge's facts."""
    fix = _dig(out, "facts", "Fix", "FixCommitSHA")
    return [p for p in (_dig(out, "facts", "Pipelines", "Pipelines") or [])
            if isinstance(p, dict) and p.get("SHA") == fix and p.get("Source") != "parent_pipeline"]


def target_attempts(out):
    return [a for p in candidates(out) for a in (p.get("Jobs") or []) if isinstance(a, dict) and a.get("Name") == JOB]


def guard_base(pipeline, jobs, trace):
    """The base pipeline failed AS DESIGNED: (ok, why). Pure. Anything else
    means the synthetic project did not behave, and nothing may be judged."""
    if not isinstance(pipeline, dict) or not isinstance(pipeline.get("id"), int):
        return False, f"the base pipeline is not readable: {str(pipeline)[:120]}"
    if pipeline.get("status") != "failed":
        return False, f"the base pipeline {pipeline['id']} is {pipeline.get('status')!r}, not failed"
    if not isinstance(jobs, list) or not all(isinstance(j, dict) for j in jobs):
        return False, f"the base pipeline's jobs are not readable: {str(jobs)[:120]}"
    by = {}
    for j in jobs:
        by.setdefault(j.get("name"), []).append(j)
    want = {JOB: "failed", LINT_JOB: "success", REPORT_JOB: "skipped"}
    for name, status in want.items():
        got = by.get(name) or []
        if len(got) != 1 or got[0].get("status") != status:
            return False, f"base job {name}: {[j.get('status') for j in got]}, want exactly one, {status}"
    if set(by) != set(want):
        return False, f"the base pipeline has jobs {sorted(map(str, by))}, want {sorted(want)}"
    job = by[JOB][0]
    if job.get("failure_reason") != "script_failure" or job.get("allow_failure") is not False:
        return False, (f"base job {JOB} failed with failure_reason={job.get('failure_reason')!r} "
                       f"allow_failure={job.get('allow_failure')!r}; want script_failure, false")
    if not isinstance(trace, str) or BASE_MARK not in trace:
        return False, f"the trace of base job {JOB} does not hold the invented failure line"
    return True, f"pipeline {pipeline['id']} failed; {JOB} failed script_failure with the invented line"


def design_guard(case, out, commit_sha):
    """Did the case's synthetic pipeline behave as DESIGNED? (ok, why). Pure.

    This is about the probe's own fixture, not about gonk: a runner failure, a
    job that failed for a reason other than its script, a head that is not the
    probe's commit. What the case exists to OBSERVE is deliberately not here.
    """
    if _dig(out, "facts", "MR", "Read") is not True:
        return False, "the merge request was not read"
    if _dig(out, "facts", "MR", "SHA") != commit_sha or _dig(out, "facts", "Fix", "FixCommitSHA") != commit_sha:
        return False, "the merge request's head is not the probe's commit"
    if _dig(out, "facts", "MR", "State") != "opened":
        return False, f"the merge request is {_dig(out, 'facts', 'MR', 'State')!r}, not opened"
    cands = candidates(out)
    if case.get("skip_ci"):
        ran = [a for p in cands for a in (p.get("Jobs") or [])]
        if ran:
            return False, f"[skip ci] was designed to run no job; {len(ran)} job object(s) exist"
        if not cands:
            return False, ("[skip ci] leaves `skipped` pipelines (observed 2026-10-07); no pipeline of the probe's "
                           "commit is in the facts, so nothing says the commit was skipped rather than not seen yet")
        for p in cands:
            if p.get("Status") != "skipped":
                return False, f"pipeline {p.get('ID')} of the [skip ci] commit is {p.get('Status')!r}, not skipped"
            jobs_read = p.get("JobsRead")
            if jobs_read is not True:
                return False, f"the jobs of pipeline {p.get('ID')} were not read"
        return True, "skipped pipelines with no job, as designed"
    if not cands:
        return False, "no pipeline of the probe's commit is in the facts"
    for p in cands:
        if p.get("Source") not in ("merge_request_event", "push"):
            return False, f"pipeline {p.get('ID')} has source {p.get('Source')!r}"
        if p.get("JobsRead") is not True:
            return False, f"the jobs of pipeline {p.get('ID')} were not read"
        if not p.get("Jobs"):
            return False, f"pipeline {p.get('ID')} has no jobs (a configuration GitLab could not build?)"
        for a in p["Jobs"]:
            if a.get("Status") == "failed" and a.get("FailureReason") != "script_failure":
                return False, (f"job {a.get('Name')} #{a.get('ID')} failed with failure_reason="
                               f"{a.get('FailureReason')!r}: runner or infrastructure, not the designed failure")
            if a.get("Status") in ("canceled", "created", "pending", "running", "preparing", "waiting_for_resource"):
                return False, f"job {a.get('Name')} #{a.get('ID')} is {a.get('Status')!r}"
    att = target_attempts(out)
    target = case.get("target")
    if target == "pass":
        if not att or any(a.get("Status") != "success" for a in att):
            return False, f"{JOB} was designed to pass; its attempts are {[a.get('Status') for a in att]}"
    elif target in ("base", "other"):
        mark = BASE_MARK if target == "base" else OTHER_MARK
        failed = [a for a in att if a.get("Status") == "failed"]
        if not failed or len(failed) != len(att):
            return False, f"{JOB} was designed to fail; its attempts are {[a.get('Status') for a in att]}"
        for a in failed:
            if _dig(a, "Trace", "Read") is not True or mark not in str(_dig(a, "Trace", "FirstError")):
                return False, f"{JOB} #{a.get('ID')} did not fail with the designed line: {_dig(a, 'Trace')!r}"
        if "attempts" in case and len(att) != case["attempts"]:
            return False, f"{JOB} was designed to run {case['attempts']} times; {len(att)} attempt(s) are listed"
    elif target != "any":
        return False, f"case {case.get('name')}: unknown target {target!r}"
    return True, "as designed"


def job_finding(out):
    for f in _dig(out, "result", "Jobs") or []:
        if isinstance(f, dict) and f.get("Job") == JOB:
            return f
    return {}


def judge_row(case, exp, out, commit_sha, problem=None):
    """One row of the L2 table. Pure.

    out is the decoded pipefix-judge output, or None when there is none;
    problem says why there is none. A row is PASS only when the fixture behaved
    as designed AND verdict, deciding rule, the diagnosed job's rule and (when
    the case states one) the definition delta ALL match. A fixture that did not
    behave is INCONCLUSIVE, whatever the verdict says."""
    row = {"case": case["name"], "cheat": case["cheat"], "what": case["what"], "mode": MODE_NAMES[exp["mode"]],
           "expected": exp, "status": INCONCLUSIVE, "why": "", "observed": {}, "decided_by": ""}
    if not isinstance(out, dict) or not isinstance(out.get("result"), dict):
        row["why"] = problem or "pipefix-judge printed no result"
        return row
    res = out["result"]
    row["observed"] = {"decision": res.get("Decision"), "verdict": res.get("Verdict"), "rule": res.get("Rule"),
                       "job_rule": job_finding(out).get("Rule"), "definition": res.get("Definition")}
    row["decided_by"] = decided_by(out)
    ok, why = design_guard(case, out, commit_sha)
    if not ok:
        row["why"] = "the synthetic pipeline did not behave as designed: " + why
        return row
    want_decision = exp.get("decision") or "verdict"
    if res.get("Decision") != want_decision:
        if res.get("Decision") == "verdict":
            # The exit check JUDGED a fixture that behaved, where it must not
            # have yet: that is an answer, and it is wrong.
            row["status"] = FAIL
            row["why"] = (f"decision 'verdict' ({res.get('Verdict')} by {res.get('Rule')}), want {want_decision!r}")
            return row
        row["why"] = (f"no {want_decision}: decision {res.get('Decision')!r} by {res.get('Rule')!r} "
                      f"({problem or res.get('Detail')})")
        return row
    wrong = []
    for key, label in (("verdict", "verdict"), ("rule", "rule"), ("job_rule", "job rule"), ("definition", "definition")):
        want = exp.get(key)
        if want is not None and row["observed"].get(key) != want:
            wrong.append(f"{label} {row['observed'].get(key)!r}, want {want!r}")
    if want_decision != "verdict" and res.get("Verdict"):
        wrong.append(f"verdict {res.get('Verdict')!r} on a {want_decision!r} answer")
    if exp.get("set_aside"):
        ids = sorted(p.get("ID") for p in candidates(out))
        aside = res.get("Skipped") or []
        if sorted(aside) != ids:
            wrong.append(f"result.Skipped {aside!r}, want every pipeline of the commit {ids!r}")
        if res.get("Candidates"):
            wrong.append(f"result.Candidates {res.get('Candidates')!r}, want none")
    if wrong:
        row["status"], row["why"] = FAIL, "; ".join(wrong)
    else:
        row["status"] = PASS
    return row


def decided_by(out):
    """The API response fields the answer rested on, as text. Pure."""
    res = out.get("result") or {}
    rule = str(res.get("Rule") or "")
    parts = []
    delta = _dig(out, "facts", "Definition", "Delta") or {}
    if rule.startswith("J2."):
        v = (delta.get("Violations") or [{}])[0]
        u = (delta.get("Unresolvable") or [{}])[0]
        if rule == J2V:
            parts.append(f"ci/lint merged_yaml(base,head) -> cidef {v.get('Rule')} "
                         f"{v.get('Job') or '(global)'}/{v.get('Key') or '(job)'}")
        elif "UNRESOLVABLE" in rule:
            parts.append(f"ci/lint -> unresolvable ({u.get('Side')}): {u.get('Reason')}")
        else:
            parts.append(f"ci/lint merged_yaml(base,head) -> delta {delta.get('Kind')!r}")
    elif rule.startswith("J1."):
        pls = _dig(out, "facts", "Pipelines", "Pipelines") or []
        parts.append("merge_requests/:iid/pipelines -> " +
                     (", ".join(f"{p.get('ID')} sha={str(p.get('SHA'))[:8]} status={p.get('Status')}" for p in pls) or "[]"))
        parts.append(f"result.Skipped={res.get('Skipped') or []} result.Candidates={res.get('Candidates') or []}")
    elif rule.startswith("J5."):
        parts.append("pipelines[].status=" + ",".join(str(p.get("Status")) for p in candidates(out)))
    att = target_attempts(out)
    f = job_finding(out)
    if att:
        parts.append(f"jobs?include_retried=true -> {JOB} x{len(att)}: " + "; ".join(
            f"#{a.get('ID')} status={a.get('Status')} allow_failure={str(a.get('AllowFailure')).lower()}"
            + (f" failure_reason={a.get('FailureReason')}" if a.get("Status") == "failed" else "") for a in att))
        if f.get("Rule") == "J3.CHANGED":
            parts.append(f"trace first error -> signature {f.get('NewSignature')} != recorded "
                         f"{_dig(out, 'facts', 'Fix', 'Targets', default=[{}])[0].get('Key', {}).get('ErrorSignature')}")
        elif f.get("Rule") == "J3.RECURS":
            parts.append("trace first error -> the recorded signature")
    elif candidates(out):
        names = sorted({str(a.get("Name")) for p in candidates(out) for a in (p.get("Jobs") or [])})
        parts.append("pipelines[].status=" + ",".join(str(p.get("Status")) for p in candidates(out)))
        parts.append(f"jobs?include_retried=true -> no job named {JOB} among {names}")
    return " ;; ".join(parts)


def keep_looking(case, exp, out):
    """Is this output not yet the one to judge the row from? Pure. True while
    there is no answer of the expected KIND (a `wait` or `cannot-judge` where a
    verdict is expected, no output at all), and -- for the [skip ci] case,
    whose first row is a verdict the exit check gives at once -- while GitLab
    has not yet listed any pipeline of the commit."""
    decision = _dig(out, "result", "Decision")
    if decision != (exp.get("decision") or "verdict") and decision in ("wait", "cannot-judge", None):
        return True
    return bool(case.get("skip_ci")) and isinstance(out, dict) and not candidates(out)


def overall(rows, planned):
    """(exit code, summary). planned: every (case, mode) that should have a
    row. A row that is missing is INCONCLUSIVE -- not run is not a pass."""
    have = {(r["case"], r["mode"]) for r in rows}
    missing = [f"{c}[{m}]" for c, m in planned if (c, m) not in have]
    failed = [r for r in rows if r["status"] == FAIL]
    incon = [r for r in rows if r["status"] == INCONCLUSIVE]
    unknown = [r for r in rows if r["status"] not in (PASS, FAIL, INCONCLUSIVE)]
    passed = [r for r in rows if r["status"] == PASS]
    summary = (f"{len(passed)} PASS, {len(failed)} FAIL, {len(incon) + len(unknown)} INCONCLUSIVE, "
               f"{len(missing)} NOT RUN of {len(planned)} planned")
    if failed:
        return EXIT_FAIL, summary
    if incon or unknown or missing or not rows:
        return EXIT_INCONCLUSIVE, summary
    return EXIT_PASS, summary


def planned_rows(selected):
    return [(c["name"], MODE_NAMES[e["mode"]]) for c in selected for e in c["expect"]]


def render_l2(rows):
    def obs(o, key):
        return str(o.get(key) or "-")
    lines = ["| case | cheat | mode | expected verdict / rule / job rule | observed verdict / rule / job rule | "
             "definition | status | decided by (API field) |", "|---|---|---|---|---|---|---|---|"]
    def answer(d):
        """The verdict, or the decision in brackets when the answer is not one."""
        if d.get("verdict"):
            return str(d["verdict"])
        return f"({d['decision']})" if d.get("decision") and d.get("decision") != "verdict" else "-"
    for r in rows:
        e, o = r["expected"], r["observed"]
        status = r["status"] + (f": {r['why']}" if r["why"] else "")
        lines.append(f"| {r['case']} | {r['cheat']} | {r['mode']} | {answer(e)} / {e['rule']} / {e.get('job_rule') or '-'} | "
                     f"{answer(o)} / {obs(o, 'rule')} / {obs(o, 'job_rule')} | {obs(o, 'definition')} | "
                     f"{status} | {r['decided_by'] or '-'} |")
    return "\n".join(lines)


# ---------------------------------------------------------------------------
# The assumptions G1-G10 (pure): each from a NAMED observation
# ---------------------------------------------------------------------------

CONFIRMED, REFUTED, OPEN, PARTIAL = "CONFIRMED", "REFUTED", "OPEN", "PARTIAL"


def settle_assumptions(obs):
    """obs: the observations dict collected by the run (see collect_*). Returns
    [(G, verdict, field observed)]. An observation that is absent leaves its
    assumption OPEN -- never confirmed by default."""
    out = []

    def add(g, verdict, text):
        out.append((g, verdict, text))

    # G1 include_retried: every attempt is its own job object with the same name.
    r = obs.get("retry")
    if not r:
        add("G1", OPEN, "the retry case did not run as designed")
    else:
        all_n, latest_n = r.get("with_retried"), r.get("without_retried")
        ok = all_n == 2 and latest_n == 1 and len(set(r.get("ids") or [])) == 2
        add("G1", CONFIRMED if ok else REFUTED,
            f"`retry: 1` job: jobs?include_retried=true lists {all_n} objects named {JOB} (ids {r.get('ids')}, "
            f"statuses {r.get('statuses')}); the default listing lists {latest_n}")

    # G2 manual / skipped / excluded-by-rules.
    m, s, x = obs.get("manual"), obs.get("skipped"), obs.get("rules")
    if not (m and s and x):
        add("G2", OPEN, f"not all three observed (manual={bool(m)}, skipped={bool(s)}, rules={bool(x)})")
    else:
        ok = m.get("status") == "manual" and s.get("status") == "skipped" and x.get("present") is False
        add("G2", CONFIRMED if ok else REFUTED,
            f"`when: manual` job: status={m.get('status')!r}; job behind a failed stage (base {REPORT_JOB}): "
            f"status={s.get('status')!r}; job whose `rules` do not match: "
            f"{'ABSENT from' if x.get('present') is False else 'PRESENT in'} the pipeline's jobs {x.get('names')}")

    # G3 allow_failure on the job object is the effective value.
    a = obs.get("allowfail")
    if not a:
        add("G3", OPEN, "the allow_failure case did not run as designed")
    else:
        ok = a.get("allow_failure") is True and a.get("status") == "failed" and a.get("base_allow_failure") is False
        add("G3", CONFIRMED if ok else REFUTED,
            f"job with `allow_failure: true` that failed: allow_failure={a.get('allow_failure')!r} status={a.get('status')!r} "
            f"(pipeline status={a.get('pipeline_status')!r}); the same job on the base pipeline: "
            f"allow_failure={a.get('base_allow_failure')!r}. Also: a `when: manual` job reports "
            f"allow_failure={(m or {}).get('allow_failure')!r} though the YAML sets none")

    # G4 what the lint's merged YAML resolves.
    d = obs.get("lint")
    if not d:
        add("G4", OPEN, "no merged YAML was read")
    else:
        as_assumed = (d.get("include_key") is False and d.get("extends_key") is True
                      and d.get("default_key") is True and d.get("reference_tag") is True)
        add("G4", CONFIRMED if as_assumed else REFUTED,
            f"static ci/lint?content_ref=<sha> merged_yaml: `include:` key present={d.get('include_key')}, included job "
            f"text present={d.get('included_text')}, includes[].context_sha == sha: {d.get('context_ok')}; `extends:` key "
            f"present={d.get('extends_key')} AND the template's keys already merged into the job={d.get('extends_merged')}; "
            f"`default:` kept={d.get('default_key')}; `!reference` tag present={d.get('reference_tag')} (resolved to a "
            f"nested list={d.get('reference_resolved')}); YAML anchor resolved={d.get('anchor_resolved')}; `stages` "
            f"gained .pre/.post={d.get('pre_post')}. With dry_run=true (what pkg/glab sent until gonk-z6gb) the same commit: "
            f"valid={d.get('dry_valid')!r} errors={d.get('dry_errors')!r}. " + go_lint_text(obs.get("go_lint")))

    # G5 the MR pipelines list holds branch pipelines too.
    p = obs.get("mr_pipelines")
    if not p:
        add("G5", OPEN, "the dual-pipeline case did not run")
    else:
        srcs = sorted(set(p.get("sources") or []))
        ok = "merge_request_event" in srcs and "push" in srcs and p.get("all_have_sha") is True
        refuted = p.get("branch_pipeline_exists") is True and "push" not in srcs
        add("G5", CONFIRMED if ok else (REFUTED if refuted else OPEN),
            f"merge_requests/:iid/pipelines for a commit that has BOTH a branch and an MR pipeline: sources {srcs}, "
            f"refs {p.get('refs')}, every entry has sha={p.get('all_have_sha')}; pipelines?sha= lists sources "
            f"{p.get('project_sources')}")

    # G6 [skip ci].
    k = obs.get("skipci")
    if not k:
        add("G6", OPEN, "the [skip ci] case did not run")
    else:
        suppressed = k.get("ran") == 0
        add("G6", PARTIAL if suppressed else REFUTED,
            f"commit with [skip ci] on a branch whose workflow would start a branch AND an MR pipeline: "
            f"pipelines?sha= -> {k.get('project')}; merge_requests/:iid/pipelines -> {k.get('mr')}; "
            f"{k.get('ran')} of them ran a job. 'A human can later start one from the MR' was NOT observed "
            "(starting a pipeline is a write this probe does not make)")

    # G7 Related to -> cross-reference.
    x7 = obs.get("crossref")
    if not x7:
        add("G7", OPEN, "not observed")
    else:
        add("G7", PARTIAL if x7.get("system_note") else OPEN,
            f"`Related to !<iid>` in an MR description: the mentioned MR's notes hold a system note "
            f"{x7.get('body')!r} (system={x7.get('system_note')}). MR -> MR only: the ISSUE side is NOT observed "
            "(the probe may not write to an issue)")

    # G8 merge status.
    g8 = obs.get("merge_status")
    if not g8:
        add("G8", OPEN, "not observed")
    else:
        add("G8", PARTIAL,
            f"detailed_merge_status on OPEN MRs: {g8}. The `sha=` pin on PUT .../merge was NOT exercised "
            "(the probe never calls merge)")

    add("G9", OPEN, "not probed: tier-gated, and the plan relies on nothing here")

    # G10 the pipeline's user is whoever pushed.
    u = obs.get("pipeline_user")
    if not u:
        add("G10", OPEN, "not observed")
    else:
        ok = u.get("me") is not None and u.get("push") == u.get("me") and u.get("mr") == u.get("me")
        add("G10", PARTIAL if ok else REFUTED,
            f"pipelines/:id user.id: push pipeline {u.get('push')!r}, MR pipeline {u.get('mr')!r}; the identity that "
            f"pushed and opened the MR (GET /user id) is {u.get('me')!r}. The pusher here is a human account, not "
            "the bot: the equality is observed, the bot identity is not")
    add("G11", OPEN, "not observable without merging a fix MR (merged_at vs the first target pipeline); the probe never merges")
    return out


def render_g(settled):
    lines = ["| # | verdict | field observed |", "|---|---|---|"]
    lines += [f"| {g} | {v} | {text} |" for g, v, text in settled]
    return "\n".join(lines)


def go_lint_observation(out, sha):
    """What gonk's OWN client (glab.CILintStatic, the one lint call it has
    since gonk-z6gb) read at the head of the `include` case, taken from
    pipefix-judge's evidence. Pure; None when there is no lint evidence for
    that commit."""
    lint = _dig(out, "evidence", "head_lint")
    if not isinstance(lint, dict) or lint.get("sha") != sha or not isinstance(lint.get("valid"), bool):
        return None
    incs = lint.get("includes") or []
    return {"valid": lint["valid"], "includes": len(incs),
            "context_ok": bool(incs) and all(isinstance(i, dict) and i.get("type") == "local" and i.get("context_sha") == sha
                                             for i in incs)}


def go_lint_text(g):
    if not g:
        return ("gonk's own client (glab.CILintStatic) at a commit that changed only its included file: NOT observed "
                "(the include case did not run as designed)")
    return ("gonk's own client (glab.CILintStatic, read back from pipefix-judge evidence.head_lint) at a commit that "
            f"changed only its included file: valid={g.get('valid')}, {g.get('includes')} include(s), every one local "
            f"with context_sha == that commit: {g.get('context_ok')}")


def lint_observation(static, dry, sha):
    """G4's observation from a static lint reply (and the dry-run reply for the
    same commit) of the job-added case's head. Pure; None when the static reply
    is not a lint result."""
    if not isinstance(static, dict) or static.get("valid") is not True or not isinstance(static.get("merged_yaml"), str):
        return None
    y = static["merged_yaml"]
    incs = static.get("includes") or []
    return {
        "include_key": bool(re.search(r"(?m)^include:", y)),
        "included_text": 'echo "probe job $CI_JOB_NAME"' in y,
        "context_ok": bool(incs) and all(isinstance(i, dict) and i.get("context_sha") == sha for i in incs),
        "extends_key": bool(re.search(r"(?m)^  extends:", y)),
        "extends_merged": bool(re.search(r"(?ms)^probe-status:\n(?:  .*\n)*?  before_script:", y)),
        "default_key": bool(re.search(r"(?m)^default:", y)),
        "reference_tag": "!reference" in y,
        "reference_resolved": bool(re.search(r"(?m)^  - - echo", y)),
        "anchor_resolved": "<<" not in y and bool(re.search(r"(?ms)^probe-lint:\n(?:  .*\n)*?    PROBE_KIND: synthetic", y)),
        "pre_post": '".pre"' in y and '".post"' in y,
        "dry_valid": dry.get("valid") if isinstance(dry, dict) else None,
        "dry_errors": dry.get("errors") if isinstance(dry, dict) else None,
    }


# ---------------------------------------------------------------------------
# I/O
# ---------------------------------------------------------------------------

REPO = ds.REPO
GITLAB_URL = f"https://{ds.GITLAB_HOST}"
_DEFAULT_BRANCH = [None]
WRITES = []


def write(method, path, body=None):
    """The ONLY way this probe writes to GitLab."""
    kind = write_guard(method, path, body, _DEFAULT_BRANCH[0])
    os.environ.setdefault("GONK_SMOKE_BODY_DIR", REPO)
    reply = ds.glab_write(method, path, body)
    WRITES.append(f"{method} {path} ({kind})")
    return reply


def get(path):
    return ds.glab_json(path)


def enc(name):
    return ds._enc(name)


def log(msg):
    print(msg, flush=True)


def build_judge(tmp):
    go = os.environ.get("GO", "go")
    out = os.path.join(tmp, "gonk-gate")
    p = subprocess.run([go, "build", "-o", out, "./cmd/gonk-gate"], cwd=REPO, capture_output=True, text=True)
    if p.returncode != 0:
        raise RuntimeError("go build ./cmd/gonk-gate failed:\n" + p.stdout + p.stderr)
    return out


def token_file(tmp):
    """The glab CLI's own token, in a 0600 file under a private temp dir that
    main() removes. Never printed, never put in an argument or the environment."""
    rc, out, err = ds.glab("config", "get", "token", "--host", ds.GITLAB_HOST)
    tok = out.strip()
    if rc != 0 or not tok:
        raise RuntimeError(f"cannot read the glab token for {ds.GITLAB_HOST}: {err}")
    path = os.path.join(tmp, "token")
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as f:
        f.write(tok + "\n")
    return path


def run_judge(judge, tokfile, base_pipeline, mr_iid, commit_sha, mode, extra=()):
    """(decoded output or None, problem or None). rc 0 a decision, 1
    cannot-judge (output still printed), anything else is a probe failure."""
    env = dict(os.environ, GONK_GITLAB_URL=GITLAB_URL, GONK_GITLAB_TOKEN_FILE=tokfile)
    argv = [judge, "pipefix-judge", "--project", str(PROJECT_ID), "--base-pipeline", str(base_pipeline),
            "--mr", str(mr_iid), "--job", JOB, "--fix-commit", commit_sha, *mode, *extra]
    try:
        p = subprocess.run(argv, capture_output=True, text=True, env=env, timeout=300)
    except subprocess.TimeoutExpired:
        return None, "pipefix-judge timed out after 300s"
    if p.returncode not in (0, 1):
        return None, f"pipefix-judge exit {p.returncode}: {p.stderr.strip()[-300:]}"
    try:
        return json.loads(p.stdout), (p.stderr.strip()[-300:] if p.returncode == 1 else None)
    except ValueError as exc:
        return None, f"pipefix-judge output is not JSON ({exc})"


def wait_for(fn, timeout_s, poll_s, label):
    end = time.monotonic() + timeout_s
    while True:
        got = fn()
        if got is not None:
            return got
        if time.monotonic() >= end:
            log(f"  timeout after {int(timeout_s)}s: {label}")
            return None
        time.sleep(poll_s)


TERMINAL = ("success", "failed", "canceled", "skipped")


def make_base(stamp, default_branch):
    """Create the base branch, wait for its (branch) pipeline to fail as
    designed. Returns (branch, sha, pipeline id) or raises Inconclusive."""
    branch = branch_name(stamp, BASE_CASE)
    made = write("POST", f"{PID}/repository/commits",
                 commit_payload(stamp, BASE_CASE, base_files(stamp), default_branch=default_branch))
    if not isinstance(made, dict) or not re.fullmatch(r"[0-9a-f]{40}", str(made.get("id", ""))):
        raise Inconclusive(f"the base commit was not created: {str(made)[:200]}")
    sha = made["id"]
    log(f"  base branch {branch} at {sha[:12]}")
    return branch, sha


class Inconclusive(Exception):
    pass


def wait_base_pipeline(sha, timeout_s, poll_s):
    def step():
        pls = get(f"{PID}/pipelines?sha={sha}")
        if not isinstance(pls, list) or not pls:
            return None
        pl = pls[0]
        if not isinstance(pl, dict) or pl.get("sha") != sha or not isinstance(pl.get("id"), int):
            return None
        log(f"  ... base pipeline {pl['id']} is {pl.get('status')}")
        return pl if pl.get("status") in TERMINAL else None
    pl = wait_for(step, timeout_s, poll_s, "the base pipeline did not finish")
    if pl is None:
        raise Inconclusive("the base pipeline did not appear or finish in time (runner noise)")
    pid = pl["id"]
    full = get(f"{PID}/pipelines/{pid}")
    jobs = get(f"{PID}/pipelines/{pid}/jobs?include_retried=true&per_page=100")
    trace = ""
    if isinstance(jobs, list):
        ids = [j["id"] for j in jobs if isinstance(j, dict) and j.get("name") == JOB and isinstance(j.get("id"), int)]
        if ids:
            rc, text = ds.glab_text(f"{PID}/jobs/{max(ids)}/trace")
            trace = text if rc == 0 else ""
    ok, why = guard_base(full, jobs, trace)
    if not ok:
        raise Inconclusive("the base pipeline did not fail as designed: " + why)
    log(f"  base: {why}")
    return full, jobs


def open_case(stamp, case, base_branch, mention_iid=None):
    """Commit the case's branch and open its MR. Returns a state dict."""
    st = {"case": case, "branch": branch_name(stamp, case["name"]), "branch_made": True, "mr": None, "sha": None}
    made = write("POST", f"{PID}/repository/commits",
                 commit_payload(stamp, case["name"], case["files"], base_branch=base_branch,
                                skip_ci=case.get("skip_ci", False), existing=set(base_files(stamp))))
    if not isinstance(made, dict) or not re.fullmatch(r"[0-9a-f]{40}", str(made.get("id", ""))):
        st["problem"] = f"the commit was not created: {str(made)[:200]}"
        return st
    st["sha"] = made["id"]
    mr = write("POST", f"{PID}/merge_requests", mr_payload(stamp, case["name"], base_branch, mention_iid))
    if not is_probe_mr(mr) or not isinstance(mr.get("iid"), int) or mr.get("target_branch") != base_branch:
        st["problem"] = f"the merge request was not created as designed: {str(mr)[:200]}"
        if isinstance(mr, dict) and isinstance(mr.get("iid"), int):
            st["mr"] = mr["iid"]
        return st
    st["mr"] = mr["iid"]
    log(f"  opened !{st['mr']} {case['name']}: {st['branch']} at {st['sha'][:12]}")
    return st


def cleanup(states, base_branch):
    """Close every MR, delete every branch, ALWAYS, and say what GitLab then
    answers for each. Returns the lines of the cleanup report. Never raises."""
    lines = []
    for st in states:
        iid = st.get("mr")
        if iid is None:
            continue
        try:
            mr = get(f"{PID}/merge_requests/{iid}")
            if not is_probe_mr(mr):
                lines.append(f"!{iid}: NOT a probe MR on read-back; left as it is")
                continue
            if mr.get("state") == "merged":
                lines.append(f"!{iid}: state=merged -- THE PROBE NEVER MERGES; look at this by hand")
                continue
            if mr.get("state") != "closed":
                write("PUT", f"{PID}/merge_requests/{iid}", {"state_event": "close"})
            after = get(f"{PID}/merge_requests/{iid}")
            state = after.get("state") if isinstance(after, dict) else None
            lines.append(f"!{iid} ({st['case']['name']}): state={state!r}" + ("" if state == "closed" else " -- CLOSE IT BY HAND"))
        except Exception as exc:  # noqa: BLE001 -- cleanup must not mask the verdict
            lines.append(f"!{iid}: close raised {exc!r} -- CLOSE IT BY HAND")
    branches = [st["branch"] for st in states if st.get("branch_made")] + ([base_branch] if base_branch else [])
    for br in branches:
        try:
            if not is_probe_branch(br):
                lines.append(f"{br!r}: not a probe branch; left as it is")
                continue
            write("DELETE", f"{PID}/repository/branches/{enc(br)}")
            gone = get(f"{PID}/repository/branches/{enc(br)}")
            msg = str(gone.get("message")) if isinstance(gone, dict) else ""
            ok = "404 Branch Not Found" in msg
            lines.append(f"{br}: " + ("deleted (GET answers 404 Branch Not Found)" if ok
                                      else f"STILL THERE or unreadable ({str(gone)[:80]}) -- DELETE IT BY HAND"))
        except Exception as exc:  # noqa: BLE001
            lines.append(f"{br}: delete raised {exc!r} -- DELETE IT BY HAND")
    return lines


def collect(case_name, st, out, obs, base_jobs, me_id):
    """Fold one case's raw observations into obs (assumptions G1-G10)."""
    iid, sha = st["mr"], st["sha"]
    att = target_attempts(out) if out else []
    cands = candidates(out) if out else []
    if case_name == "retry" and cands:
        pid = cands[0]["ID"]
        with_r = get(f"{PID}/pipelines/{pid}/jobs?include_retried=true&per_page=100")
        without = get(f"{PID}/pipelines/{pid}/jobs?per_page=100")
        if isinstance(with_r, list) and isinstance(without, list):
            mine = [j for j in with_r if isinstance(j, dict) and j.get("name") == JOB]
            obs["retry"] = {"with_retried": len(mine), "ids": [j.get("id") for j in mine],
                            "statuses": [j.get("status") for j in mine],
                            "without_retried": len([j for j in without if isinstance(j, dict) and j.get("name") == JOB])}
    if case_name == "manual" and att:
        obs["manual"] = {"status": att[0].get("Status"), "allow_failure": att[0].get("AllowFailure")}
    if case_name == "rules" and cands:
        names = sorted({a.get("Name") for p in cands for a in (p.get("Jobs") or [])})
        obs["rules"] = {"present": JOB in names, "names": names}
    if case_name == "allowfail" and att:
        base = [j for j in base_jobs if j.get("name") == JOB]
        obs["allowfail"] = {"allow_failure": att[0].get("AllowFailure"), "status": att[0].get("Status"),
                            "pipeline_status": cands[0].get("Status") if cands else None,
                            "base_allow_failure": base[0].get("allow_failure") if base else None}
    if case_name == "include":
        gl = go_lint_observation(out, sha)
        if gl:
            obs["go_lint"] = gl
    if case_name == "job-added":
        static = get(f"{PID}/ci/lint?content_ref={sha}&include_merged_yaml=true")
        dry = get(f"{PID}/ci/lint?content_ref={sha}&dry_run=true&include_merged_yaml=true")
        lo = lint_observation(static, dry, sha)
        if lo:
            obs["lint"] = lo
    if case_name == "fixed":
        mrp = get(f"{PID}/merge_requests/{iid}/pipelines")
        prj = get(f"{PID}/pipelines?sha={sha}")
        if isinstance(mrp, list) and isinstance(prj, list):
            mine = [p for p in mrp if isinstance(p, dict) and p.get("sha") == sha]
            obs["mr_pipelines"] = {
                "sources": [p.get("source") for p in mine], "refs": [p.get("ref") for p in mine],
                "all_have_sha": bool(mrp) and all(isinstance(p, dict) and re.fullmatch(r"[0-9a-f]{40}", str(p.get("sha"))) for p in mrp),
                "project_sources": sorted(str(p.get("source")) for p in prj if isinstance(p, dict)),
                "branch_pipeline_exists": any(isinstance(p, dict) and p.get("source") == "push" for p in prj)}
            users = {}
            for p in prj:
                if isinstance(p, dict) and isinstance(p.get("id"), int):
                    full = get(f"{PID}/pipelines/{p['id']}")
                    if isinstance(full, dict) and full.get("id") == p["id"]:
                        users[full.get("source")] = (full.get("user") or {}).get("id")
            obs["pipeline_user"] = {"me": me_id, "push": users.get("push"), "mr": users.get("merge_request_event")}
    if case_name == "skipci":
        mrp = get(f"{PID}/merge_requests/{iid}/pipelines")
        prj = get(f"{PID}/pipelines?sha={sha}")
        if isinstance(mrp, list) and isinstance(prj, list):
            ran = 0
            for p in prj:
                if isinstance(p, dict) and isinstance(p.get("id"), int):
                    jobs = get(f"{PID}/pipelines/{p['id']}/jobs?include_retried=true&per_page=100")
                    ran += len([j for j in jobs if isinstance(j, dict) and j.get("status") in ("success", "failed", "running")]) \
                        if isinstance(jobs, list) else 1
            obs["skipci"] = {"mr": [(p.get("id"), p.get("source"), p.get("status")) for p in mrp if isinstance(p, dict)],
                             "project": [(p.get("id"), p.get("source"), p.get("status")) for p in prj if isinstance(p, dict)],
                             "ran": ran}
    mr = get(f"{PID}/merge_requests/{iid}")
    if isinstance(mr, dict) and mr.get("iid") == iid and "detailed_merge_status" in mr:
        obs.setdefault("merge_status", {})[case_name] = mr["detailed_merge_status"]


RAW_LINT_CASES = ("stage-appended", "stage-inserted", "job-added", "include")


def keep_raw(directory, name, reply):
    """Keep one RAW GitLab reply (a GET's decoded body) next to the judge
    outputs, so a Go fixture can be a copy of what GitLab said."""
    os.makedirs(directory, exist_ok=True)
    with open(os.path.join(directory, f"raw.{name}.json"), "w") as f:
        json.dump(reply, f, indent=1, sort_keys=True)


def keep_raw_replies(directory, case_name, st):
    """The replies the Go fixtures are copied from: both lints of the cases
    whose definition is the point, and the pipelines of the [skip ci] commit.
    Reads only."""
    iid, sha = st["mr"], st["sha"]
    if case_name in RAW_LINT_CASES:
        keep_raw(directory, f"{case_name}.lint-static", get(f"{PID}/ci/lint?content_ref={sha}&include_merged_yaml=true"))
        keep_raw(directory, f"{case_name}.lint-dry-run",
                 get(f"{PID}/ci/lint?content_ref={sha}&dry_run=true&include_merged_yaml=true"))
    if case_name == "skipci":
        mrp = get(f"{PID}/merge_requests/{iid}/pipelines")
        keep_raw(directory, "skipci.mr-pipelines", mrp)
        for p in mrp if isinstance(mrp, list) else []:
            if isinstance(p, dict) and isinstance(p.get("id"), int):
                keep_raw(directory, f"skipci.pipeline-{p.get('source')}.jobs",
                         get(f"{PID}/pipelines/{p['id']}/jobs?include_retried=true&per_page=100"))


def run(args):
    stamp = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
    selected = [c for c in cases() if not args.only or c["name"] in args.only]
    planned = planned_rows(selected)
    rows, states, obs, base_branch = [], [], {}, None
    tmp = tempfile.mkdtemp(prefix="pipefix-probe-")
    try:
        me = get("user")
        if not isinstance(me, dict) or not isinstance(me.get("id"), int) or not me.get("username"):
            log(f"cannot read the glab user: {str(me)[:120]}")
            return rows, planned, obs, [], None
        bad, default_branch = ds.check_project(get(PID))
        if bad:
            log(bad.observed)
            return rows, planned, obs, [], None
        _DEFAULT_BRANCH[0] = default_branch
        judge = args.judge_bin or build_judge(tmp)
        tokfile = token_file(tmp)
        log(f"pipefix probe {stamp} on {PROJECT_PATH} ({PROJECT_ID}) as {me['username']}; default branch {default_branch}")

        base_branch, base_sha = make_base(stamp, default_branch)
        base_pl, base_jobs = wait_base_pipeline(base_sha, args.ci_timeout * 60, args.poll)
        base_pid = base_pl["id"]
        if args.keep_json:
            keep_raw(args.keep_json, "base.lint-static", get(f"{PID}/ci/lint?content_ref={base_sha}&include_merged_yaml=true"))
        rep = [j for j in base_jobs if j.get("name") == REPORT_JOB]
        if rep:
            obs["skipped"] = {"status": rep[0].get("status")}
        first_iid = None
        for start in range(0, len(selected), args.batch):
            batch = selected[start:start + args.batch]
            opened = []
            for i, case in enumerate(batch):
                last = start + i == len(selected) - 1
                st = open_case(stamp, case, base_branch, mention_iid=first_iid if (last and first_iid) else None)
                if last and first_iid and st.get("mr"):
                    obs["_mention"] = (st["mr"], first_iid)
                states.append(st)
                opened.append(st)
                if first_iid is None and st.get("mr"):
                    first_iid = st["mr"]
            deadline = time.monotonic() + args.ci_timeout * 60
            pending = [st for st in opened if not st.get("problem")]
            for st in opened:
                if st.get("problem"):
                    for exp in st["case"]["expect"]:
                        rows.append(judge_row(st["case"], exp, None, st.get("sha"), problem=st["problem"]))
            results = {}
            while pending:
                still = []
                for st in pending:
                    case = st["case"]
                    first = case["expect"][0]
                    if case.get("skip_ci") and time.monotonic() < deadline - args.ci_timeout * 60 + args.skip_ci_settle:
                        still.append(st)
                        continue
                    out, problem = run_judge(judge, tokfile, base_pid, st["mr"], st["sha"], first["mode"])
                    decision = _dig(out, "result", "Decision")
                    if keep_looking(case, first, out) and time.monotonic() < deadline:
                        log(f"  ... !{st['mr']} {case['name']}: {decision or problem}: {_dig(out, 'result', 'Detail') or ''}"[:200])
                        still.append(st)
                        continue
                    results[case["name"]] = (out, problem if decision != "verdict" else None)
                pending = still
                if pending:
                    time.sleep(args.poll)
            for st in opened:
                case = st["case"]
                if case["name"] not in results:
                    continue
                out, problem = results[case["name"]]
                for n, exp in enumerate(case["expect"]):
                    if n > 0:
                        out, problem = run_judge(judge, tokfile, base_pid, st["mr"], st["sha"], exp["mode"])
                    row = judge_row(case, exp, out, st["sha"], problem=problem)
                    rows.append(row)
                    log(f"  {row['status']:12} {case['name']} [{row['mode']}]: {row['observed'].get('verdict')} by "
                        f"{row['observed'].get('rule')} {('-- ' + row['why']) if row['why'] else ''}")
                    if args.keep_json and out is not None:
                        os.makedirs(args.keep_json, exist_ok=True)
                        with open(os.path.join(args.keep_json, f"{case['name']}.{row['mode'].replace(',', '_')}.json"), "w") as f:
                            json.dump(out, f, indent=1)
                    if n == 0 and args.keep_json:
                        try:
                            keep_raw_replies(args.keep_json, case["name"], st)
                        except Exception as exc:  # noqa: BLE001 -- a recording, never the verdict
                            log(f"  recording for {case['name']} raised {exc!r}")
                    if n == 0:
                        ok, _ = design_guard(case, out, st["sha"]) if out else (False, "")
                        if ok:
                            try:
                                collect(case["name"], st, out, obs, base_jobs, me["id"])
                            except Exception as exc:  # noqa: BLE001 -- an observation, never the verdict
                                log(f"  observation for {case['name']} raised {exc!r}")
        mention = obs.pop("_mention", None)
        if mention:
            notes = get(f"{PID}/merge_requests/{mention[1]}/notes?per_page=100")
            if isinstance(notes, list):
                hit = [n for n in notes if isinstance(n, dict) and n.get("system") is True
                       and f"!{mention[0]}" in str(n.get("body"))]
                obs["crossref"] = {"system_note": bool(hit), "body": hit[0].get("body") if hit else None}
    except Inconclusive as exc:
        log(f"INCONCLUSIVE: {exc}")
    finally:
        log("cleanup:")
        report = cleanup(states, base_branch)
        for line in report:
            log("  " + line)
        import shutil
        shutil.rmtree(tmp, ignore_errors=True)
    return rows, planned, obs, report, stamp


def main(argv):
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--only", type=lambda s: [x for x in s.split(",") if x], default=None)
    ap.add_argument("--batch", type=int, default=4, help="merge requests opened at a time (runner load)")
    ap.add_argument("--ci-timeout", type=int, default=15, help="minutes to wait for a batch's pipelines")
    ap.add_argument("--poll", type=int, default=20, help="seconds between looks")
    ap.add_argument("--skip-ci-settle", type=int, default=90, help="seconds to give a [skip ci] push before judging it")
    ap.add_argument("--judge-bin", default=None, help="a built gonk-gate (default: go build ./cmd/gonk-gate)")
    ap.add_argument("--keep-json", default=None, help="directory to keep each pipefix-judge output, and the raw lint / pipeline replies, in")
    ap.add_argument("--list", action="store_true")
    args = ap.parse_args(argv)
    known = [c["name"] for c in cases()]
    if args.list:
        for c in cases():
            print(f"{c['name']:16} {c['cheat']:7} {c['what']}")
        return EXIT_PASS
    if args.only and set(args.only) - set(known):
        print(f"unknown case(s): {sorted(set(args.only) - set(known))}; known: {known}")
        return 2
    if args.batch < 1:
        print("--batch must be at least 1")
        return 2
    rows, planned, obs, report, stamp = run(args)
    print("\n=== L2: the exit check on the real GitLab ===")
    print(render_l2(rows))
    print("\n=== assumptions about GitLab (plan 2.2) ===")
    print(render_g(settle_assumptions(obs)))
    print("\n=== writes sent to GitLab ===")
    for w in WRITES:
        print("  " + w)
    code, summary = overall(rows, planned)
    left = [line for line in report if "BY HAND" in line or "left as it is" in line]
    print(f"\npipefix probe {stamp}: {summary}; cleanup {'CLEAN' if report and not left else 'INCOMPLETE' if left else 'nothing to clean'}")
    print({EXIT_PASS: "PASS", EXIT_FAIL: "FAIL", EXIT_INCONCLUSIVE: "INCONCLUSIVE"}[code] + f" (exit {code})")
    return code


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
