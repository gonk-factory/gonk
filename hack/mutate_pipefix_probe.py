#!/usr/bin/env python3
"""Break each decision in hack/pipefix_probe.py and watch
hack/test_pipefix_probe.py go red (CLAUDE.md: a test counts only once it has
been seen RED against a deliberately broken decision).

    hack/mutate_pipefix_probe.py [path/to/pytest] [--show]

Each mutation is applied alone, the tests are run, the failing test names are
recorded, and the file is RESTORED (also on error). Exits non-zero if the
unmutated baseline is red, if a mutation's text does not occur exactly once
(the code moved), or if any mutation leaves every test green.

The break the task names is marked [NAMED]: INCONCLUSIVE reported as PASS.
"""
import os
import re
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
P = os.path.join(HERE, "pipefix_probe.py")
T = os.path.join(HERE, "test_pipefix_probe.py")
args = [a for a in sys.argv[1:] if a != "--show"]
SHOW = "--show" in sys.argv[1:]
PYTEST = args[0] if args else "pytest"

M = [
 # ---- the verdict of a row
 ("[NAMED] INCONCLUSIVE reported as PASS: a row starts out as PASS", '"expected": exp, "status": INCONCLUSIVE, "why": ""', '"expected": exp, "status": PASS, "why": ""'),
 ("[NAMED] INCONCLUSIVE reported as PASS: the design guard is not consulted", "    ok, why = design_guard(case, out, commit_sha)\n    if not ok:\n", "    ok, why = design_guard(case, out, commit_sha)\n    if False:\n"),
 ("[NAMED] INCONCLUSIVE reported as PASS: a result that is not a verdict is compared anyway", '    if res.get("Decision") != want_decision:\n', "    if False:\n"),
 # ---- gonk-vi9k: the [skip ci] rows (wait before the deadline, J1.TIMEOUT after)
 ("skipci: every row expects a verdict (the wait row can only ever be noise)", '    want_decision = exp.get("decision") or "verdict"\n', '    want_decision = "verdict"\n'),
 ("skipci: a verdict where a wait is expected is INCONCLUSIVE (the defect hidden as noise)", '        if res.get("Decision") == "verdict":\n', "        if False:\n"),
 ("skipci: a verdict where a wait is expected is a PASS", "            row[\"status\"] = FAIL\n            row[\"why\"] = (f\"decision 'verdict'", "            row[\"status\"] = PASS\n            row[\"why\"] = (f\"decision 'verdict'"),
 ("skipci: a verdict carried on a wait answer is fine", '    if want_decision != "verdict" and res.get("Verdict"):\n', "    if False:\n"),
 ("skipci: WHY it waits is not checked (result.Skipped / Candidates)", '    if exp.get("set_aside"):\n', "    if False:\n"),
 ("skipci: result.Skipped need only be a SUBSET of the commit's pipelines (empty passes)", "        if sorted(aside) != ids:", "        if not set(aside) <= set(ids):"),
 ("skipci: result.Skipped need only COVER the commit's pipelines", "        if sorted(aside) != ids:", "        if not set(ids) <= set(aside):"),
 ("skipci: a skipped pipeline that is still a candidate is fine", '        if res.get("Candidates"):\n', "        if False:\n"),
 ("skipci guard: no pipeline of the commit at all is as designed (a `wait` for the wrong reason passes)", '        if not cands:\n            return False, ("[skip ci] leaves', '        if False:\n            return False, ("[skip ci] leaves'),
 ("skipci guard: a pipeline that is not skipped is as designed", '            if p.get("Status") != "skipped":', "            if False:"),
 ("skipci guard: unread jobs of a skipped pipeline are no jobs", "            if jobs_read is not True:", "            if False:"),
 ("keep_looking: a wait is looked at again even where a wait is the answer (the row never ends)", '    if decision != (exp.get("decision") or "verdict") and decision in ("wait", "cannot-judge", None):', '    if decision in ("wait", "cannot-judge", None):'),
 ("keep_looking: a wait is judged at once where a verdict is expected", '    if decision != (exp.get("decision") or "verdict") and decision in ("wait", "cannot-judge", None):', "    if False:"),
 ("keep_looking: `stopped` is looked at again", 'and decision in ("wait", "cannot-judge", None):\n        return True\n    return bool(case', 'and decision in ("wait", "cannot-judge", "stopped", None):\n        return True\n    return bool(case'),
 ("keep_looking: a [skip ci] commit is judged before GitLab lists its pipelines", '    return bool(case.get("skip_ci")) and isinstance(out, dict) and not candidates(out)', "    return False"),
 ("keep_looking: every case waits for a pipeline listing", '    return bool(case.get("skip_ci")) and isinstance(out, dict) and not candidates(out)', "    return isinstance(out, dict) and not candidates(out)"),
 ("table: a decision that is not a verdict is shown as nothing", "        return f\"({d['decision']})\" if d.get(\"decision\") and d.get(\"decision\") != \"verdict\" else \"-\"", '        return "-"'),
 ("decided_by: a J1 row does not say what was set aside", "        parts.append(f\"result.Skipped={res.get('Skipped') or []} result.Candidates={res.get('Candidates') or []}\")\n", ""),
 ("cases: the [skip ci] row before the deadline expects the old immediate UNVERIFIABLE", '                    expect(MODE_DEFAULT, None, "J1.WAIT", decision="wait", set_aside=True)]},', '                    expect(MODE_DEFAULT, "UNVERIFIABLE", ABSENT, ABSENT)]},'),
 ("cases: the [skip ci] row past the deadline expects J3's absent job, not J1.TIMEOUT", '"expect": [expect(MODE_PAST_DEADLINE, "UNVERIFIABLE", "J1.TIMEOUT", ABSENT, "none", set_aside=True),', '"expect": [expect(MODE_PAST_DEADLINE, "UNVERIFIABLE", ABSENT, ABSENT, "none", set_aside=True),'),
 ("[NAMED] INCONCLUSIVE reported as PASS: no output is a row with nothing wrong", '        row["why"] = problem or "pipefix-judge printed no result"\n        return row', '        row["status"] = PASS\n        return row'),
 ("a wrong answer is a PASS", '        row["status"], row["why"] = FAIL, "; ".join(wrong)', '        row["status"], row["why"] = PASS, "; ".join(wrong)'),
 ("a wrong answer is INCONCLUSIVE (a defect hidden as noise)", '        row["status"], row["why"] = FAIL, "; ".join(wrong)', '        row["status"], row["why"] = INCONCLUSIVE, "; ".join(wrong)'),
 ("the verdict is not compared", 'for key, label in (("verdict", "verdict"), ("rule", "rule"),', 'for key, label in (("rule", "rule"),'),
 ("the deciding rule is not compared", '(("verdict", "verdict"), ("rule", "rule"), ("job_rule", "job rule")', '(("verdict", "verdict"), ("job_rule", "job rule")'),
 ("the diagnosed job's rule is not compared", '("rule", "rule"), ("job_rule", "job rule"), ("definition", "definition")):', '("rule", "rule"), ("definition", "definition")):'),
 ("the definition delta is not compared", '("job_rule", "job rule"), ("definition", "definition")):', '("job_rule", "job rule")):'),
 ("an expected None is compared (every row without a definition fails)", "        if want is not None and row[\"observed\"].get(key) != want:", "        if row[\"observed\"].get(key) != want:"),
 # ---- the verdict of the run
 ("[NAMED] INCONCLUSIVE reported as PASS: an inconclusive row does not change the exit code", "    if incon or unknown or missing or not rows:", "    if unknown or missing or not rows:"),
 ("[NAMED] INCONCLUSIVE reported as PASS: exit 3 is exit 0", "        return EXIT_INCONCLUSIVE, summary\n    return EXIT_PASS, summary", "        return EXIT_PASS, summary\n    return EXIT_PASS, summary"),
 ("a planned row that never ran is not missed", "    if incon or unknown or missing or not rows:", "    if incon or unknown or not rows:"),
 ("an empty run passes", "    if incon or unknown or missing or not rows:", "    if incon or unknown or missing:"),
 ("a status nobody defined passes", "    if incon or unknown or missing or not rows:", "    if incon or missing or not rows:"),
 ("a FAIL does not fail the run", "    if failed:\n        return EXIT_FAIL, summary", "    if False:\n        return EXIT_FAIL, summary"),
 ("an INCONCLUSIVE outranks a FAIL", "    if failed:\n        return EXIT_FAIL, summary\n    if incon or unknown or missing or not rows:\n        return EXIT_INCONCLUSIVE, summary",
  "    if incon or unknown or missing or not rows:\n        return EXIT_INCONCLUSIVE, summary\n    if failed:\n        return EXIT_FAIL, summary"),
 ("missing rows are matched by case only", "    have = {(r[\"case\"], r[\"mode\"]) for r in rows}\n    missing = [f\"{c}[{m}]\" for c, m in planned if (c, m) not in have]",
  "    have = {r[\"case\"] for r in rows}\n    missing = [f\"{c}[{m}]\" for c, m in planned if c not in have]"),
 ("the summary counts every row as a pass", "    passed = [r for r in rows if r[\"status\"] == PASS]", "    passed = rows"),
 # ---- the design guard
 ("guard: an unread merge request is fine", '    if _dig(out, "facts", "MR", "Read") is not True:', "    if False:"),
 ("guard: the head need not be the probe's commit", '    if _dig(out, "facts", "MR", "SHA") != commit_sha or _dig(out, "facts", "Fix", "FixCommitSHA") != commit_sha:', "    if False:"),
 ("guard: a closed merge request is fine", '    if _dig(out, "facts", "MR", "State") != "opened":', "    if False:"),
 ("guard: no pipeline is fine", "    if not cands:\n        return False,", "    if False:\n        return False,"),
 ("guard: any pipeline source is fine", '        if p.get("Source") not in ("merge_request_event", "push"):', "        if False:"),
 ("guard: unread jobs are fine", '        if p.get("JobsRead") is not True:', "        if False:"),
 ("guard: a pipeline with no jobs is fine", '        if not p.get("Jobs"):', "        if False:"),
 ("guard: a runner failure is the designed failure", '            if a.get("Status") == "failed" and a.get("FailureReason") != "script_failure":', "            if False:"),
 ("guard: a canceled or running job is fine", '            if a.get("Status") in ("canceled", "created", "pending", "running", "preparing", "waiting_for_resource"):', "            if False:"),
 ("guard: a job designed to pass may fail", "        if not att or any(a.get(\"Status\") != \"success\" for a in att):", "        if not att:"),
 ("guard: a job designed to pass may be absent", "        if not att or any(a.get(\"Status\") != \"success\" for a in att):", "        if any(a.get(\"Status\") != \"success\" for a in att):"),
 ("guard: a job designed to fail may pass", "        if not failed or len(failed) != len(att):", "        if False:"),
 ("guard: a job designed to fail may fail with any line", "            if _dig(a, \"Trace\", \"Read\") is not True or mark not in str(_dig(a, \"Trace\", \"FirstError\")):", "            if _dig(a, \"Trace\", \"Read\") is not True:"),
 ("guard: an unread trace is the designed line", "            if _dig(a, \"Trace\", \"Read\") is not True or mark not in str(_dig(a, \"Trace\", \"FirstError\")):", "            if mark not in str(_dig(a, \"Trace\", \"FirstError\")) and _dig(a, \"Trace\", \"Read\") is True:"),
 ("guard: the other failure's mark is the base's", "        mark = BASE_MARK if target == \"base\" else OTHER_MARK", "        mark = BASE_MARK"),
 ("guard: the designed number of attempts is not checked", "        if \"attempts\" in case and len(att) != case[\"attempts\"]:", "        if False:"),
 ("guard: an unknown target is as designed", "    elif target != \"any\":\n        return False,", "    elif False:\n        return False,"),
 ("guard: the skip-ci case skips the head check too", "    if _dig(out, \"facts\", \"MR\", \"SHA\") != commit_sha or _dig(out, \"facts\", \"Fix\", \"FixCommitSHA\") != commit_sha:", "    if not case.get(\"skip_ci\") and (_dig(out, \"facts\", \"MR\", \"SHA\") != commit_sha or _dig(out, \"facts\", \"Fix\", \"FixCommitSHA\") != commit_sha):"),
 ("guard: a [skip ci] commit that ran a job is as designed", "        if ran:\n            return False, f\"[skip ci] was designed", "        if False:\n            return False, f\"[skip ci] was designed"),
 ("candidates: pipelines of any commit count", "            if isinstance(p, dict) and p.get(\"SHA\") == fix and p.get(\"Source\") != \"parent_pipeline\"]", "            if isinstance(p, dict) and p.get(\"Source\") != \"parent_pipeline\"]"),
 # ---- the base guard
 ("base: an unreadable pipeline is fine", "    if not isinstance(pipeline, dict) or not isinstance(pipeline.get(\"id\"), int):", "    if not isinstance(pipeline, dict):"),
 ("base: any pipeline status is fine", "    if pipeline.get(\"status\") != \"failed\":", "    if False:"),
 ("base: job statuses are not checked", "        if len(got) != 1 or got[0].get(\"status\") != status:", "        if len(got) != 1:"),
 ("base: a retried job is fine", "        if len(got) != 1 or got[0].get(\"status\") != status:", "        if not got or got[0].get(\"status\") != status:"),
 ("base: extra jobs are fine", "    if set(by) != set(want):", "    if False:"),
 ("base: any failure_reason is the designed one", "    if job.get(\"failure_reason\") != \"script_failure\" or job.get(\"allow_failure\") is not False:", "    if job.get(\"allow_failure\") is not False:"),
 ("base: an allowed failure is the designed one", "    if job.get(\"failure_reason\") != \"script_failure\" or job.get(\"allow_failure\") is not False:", "    if job.get(\"failure_reason\") != \"script_failure\":"),
 ("base: the trace need not hold the invented line", "    if not isinstance(trace, str) or BASE_MARK not in trace:", "    if not isinstance(trace, str):"),
 # ---- the write guard
 ("write: any project", "    if not path.startswith(PID + \"/\"):", "    if not path.startswith(\"projects/\"):"),
 ("write: an unknown default branch is fine", "    if not default_branch or is_probe_branch(default_branch):", "    if False:"),
 ("write: a commit on any branch", "        if not is_probe_branch(branch) or branch == default_branch:\n            refuse(f\"commit on", "        if False:\n            refuse(f\"commit on"),
 ("write: the base may start anywhere", "            if start != default_branch:", "            if False:"),
 ("write: a case branch may start from the default branch", "        elif not is_base_branch(start) or BRANCH_RE.match(start).group(1) != BRANCH_RE.match(branch).group(1):", "        elif False:"),
 ("write: a case branch may start from another run's base", "        elif not is_base_branch(start) or BRANCH_RE.match(start).group(1) != BRANCH_RE.match(branch).group(1):", "        elif not is_base_branch(start):"),
 ("write: any commit action", "            if not isinstance(act, dict) or act.get(\"action\") not in (\"create\", \"update\", \"delete\"):", "            if not isinstance(act, dict):"),
 ("write: .gonk.yml may be committed", "in (\".gonk.yml\", \"CODEOWNERS\"):", "in (\"CODEOWNERS\",):"),
 ("write: CODEOWNERS may be committed", "in (\".gonk.yml\", \"CODEOWNERS\"):", "in (\".gonk.yml\",):"),
 ("write: an empty commit", "        if not isinstance(actions, list) or not actions:\n            refuse(\"a commit with no actions\")", "        if not isinstance(actions, list):\n            refuse(\"a commit with no actions\")"),
 ("write: an MR into any branch", "        if not is_probe_branch(src) or is_base_branch(src) or not is_base_branch(dst) or dst == default_branch:", "        if not is_probe_branch(src) or is_base_branch(src):"),
 ("write: an MR from any branch", "        if not is_probe_branch(src) or is_base_branch(src) or not is_base_branch(dst) or dst == default_branch:", "        if not is_base_branch(dst) or dst == default_branch:"),
 ("write: an MR from the base branch", "        if not is_probe_branch(src) or is_base_branch(src) or not is_base_branch(dst) or dst == default_branch:", "        if not is_probe_branch(src) or not is_base_branch(dst) or dst == default_branch:"),
 ("write: any MR title", "        if not str(body.get(\"title\", \"\")).startswith(TITLE_PREFIX):\n            refuse(\"the title is not the probe's\")", "        if False:\n            refuse(\"the title is not the probe's\")"),
 ("write: extra MR fields (assignee, labels, auto-merge)", "        if extra:\n            refuse(", "        if False:\n            refuse("),
 ("write: THE MERGE passes as an MR edit", "    if method == \"PUT\" and re.fullmatch(r\"merge_requests/\\d+\", rest):", "    if method == \"PUT\" and re.match(r\"merge_requests/\\d+\", rest):\n        return \"close\"\n    if False:"),
 ("write: any MR edit", "        if body != {\"state_event\": \"close\"}:", "        if body.get(\"state_event\") != \"close\":"),
 ("write: any state event", "        if body != {\"state_event\": \"close\"}:", "        if \"state_event\" not in body or len(body) != 1:"),
 ("write: any branch may be deleted", "        if not is_probe_branch(branch) or branch == default_branch:\n            refuse(f\"delete of", "        if False:\n            refuse(f\"delete of"),
 ("write: anything else passes", "    refuse(\"not one of the probe's writes\")", "    return \"other\""),
 # ---- names
 ("names: any branch under probe/ is the probe's", "BRANCH_RE = re.compile(r\"^probe/pipefix-(\\d{8}T\\d{6}Z)-([a-z0-9]+(?:-[a-z0-9]+)*)$\")", "BRANCH_RE = re.compile(r\"^probe/pipefix-(.*)-([a-z0-9]+(?:-[a-z0-9]+)*)$\")"),
 ("names: the branch pattern is not anchored at its end", "([a-z0-9]+(?:-[a-z0-9]+)*)$\")", "([a-z0-9]+(?:-[a-z0-9]+)*)\")"),
 ("names: every probe branch is a base branch", "    return is_probe_branch(name) and BRANCH_RE.match(name).group(2) == BASE_CASE", "    return is_probe_branch(name)"),
 ("mr: another project's MR is the probe's", "    return (isinstance(mr, dict) and mr.get(\"project_id\") == PROJECT_ID\n", "    return (isinstance(mr, dict)\n"),
 ("mr: a fork's MR is the probe's", "            and mr.get(\"source_project_id\", PROJECT_ID) == PROJECT_ID\n", ""),
 ("mr: any source branch", "            and is_probe_branch(mr.get(\"source_branch\"))\n", ""),
 ("mr: any target branch", "            and is_base_branch(mr.get(\"target_branch\"))\n", ""),
 ("mr: any title", "            and str(mr.get(\"title\", \"\")).startswith(TITLE_PREFIX))", "            )"),
 ("payload: the case branch starts from the default branch", "    start = default_branch if case == BASE_CASE else base_branch", "    start = default_branch or base_branch"),
 ("payload: [skip ci] on every commit", "+ (\" [skip ci]\" if skip_ci else \"\")", "+ \" [skip ci]\""),
 ("payload: every file is created (an update of an existing file would 400)", "\"update\" if path in existing else \"create\"", "\"create\""),
 ("fixture: the fixed script is still wrong", "    word = \"crates\" if (fixed or other) else \"crats\"", "    word = \"crats\""),
 # ---- assumptions
 ("G: nothing observed confirms G1", "    if not r:\n        add(\"G1\", OPEN,", "    if False:\n        add(\"G1\", OPEN,"),
 ("G1: one listed attempt is every attempt", "        ok = all_n == 2 and latest_n == 1 and len(set(r.get(\"ids\") or [])) == 2", "        ok = latest_n == 1 and len(set(r.get(\"ids\") or [])) == 2"),
 ("G1: the default listing is not looked at", "        ok = all_n == 2 and latest_n == 1 and len(set(r.get(\"ids\") or [])) == 2", "        ok = all_n == 2 and len(set(r.get(\"ids\") or [])) == 2"),
 ("G1: the same object twice is two attempts", "        ok = all_n == 2 and latest_n == 1 and len(set(r.get(\"ids\") or [])) == 2", "        ok = all_n == 2 and latest_n == 1"),
 ("G2: two of three observations confirm", "    if not (m and s and x):", "    if not (m or s or x):"),
 ("G2: the manual status is not checked", "        ok = m.get(\"status\") == \"manual\" and s.get(\"status\") == \"skipped\" and x.get(\"present\") is False", "        ok = s.get(\"status\") == \"skipped\" and x.get(\"present\") is False"),
 ("G2: the skipped status is not checked", "        ok = m.get(\"status\") == \"manual\" and s.get(\"status\") == \"skipped\" and x.get(\"present\") is False", "        ok = m.get(\"status\") == \"manual\" and x.get(\"present\") is False"),
 ("G2: a present job is absent", "        ok = m.get(\"status\") == \"manual\" and s.get(\"status\") == \"skipped\" and x.get(\"present\") is False", "        ok = m.get(\"status\") == \"manual\" and s.get(\"status\") == \"skipped\""),
 ("G3: allow_failure false confirms", "        ok = a.get(\"allow_failure\") is True and a.get(\"status\") == \"failed\" and a.get(\"base_allow_failure\") is False", "        ok = a.get(\"status\") == \"failed\" and a.get(\"base_allow_failure\") is False"),
 ("G3: a job that did not fail confirms", "        ok = a.get(\"allow_failure\") is True and a.get(\"status\") == \"failed\" and a.get(\"base_allow_failure\") is False", "        ok = a.get(\"allow_failure\") is True and a.get(\"base_allow_failure\") is False"),
 ("G3: the base value is not compared", "        ok = a.get(\"allow_failure\") is True and a.get(\"status\") == \"failed\" and a.get(\"base_allow_failure\") is False", "        ok = a.get(\"allow_failure\") is True and a.get(\"status\") == \"failed\""),
 ("G4: a resolved !reference still confirms", "                      and d.get(\"default_key\") is True and d.get(\"reference_tag\") is True)", "                      and d.get(\"default_key\") is True)"),
 ("G4: a kept include still confirms", "        as_assumed = (d.get(\"include_key\") is False and d.get(\"extends_key\") is True", "        as_assumed = (d.get(\"extends_key\") is True"),
 ("G4: a dropped extends still confirms", "        as_assumed = (d.get(\"include_key\") is False and d.get(\"extends_key\") is True", "        as_assumed = (d.get(\"include_key\") is False"),
 ("G4: a resolved default still confirms", "                      and d.get(\"default_key\") is True and d.get(\"reference_tag\") is True)", "                      and d.get(\"reference_tag\") is True)"),
 ("G5: MR pipelines alone confirm", "        ok = \"merge_request_event\" in srcs and \"push\" in srcs and p.get(\"all_have_sha\") is True", "        ok = \"merge_request_event\" in srcs and p.get(\"all_have_sha\") is True"),
 ("G5: entries without sha confirm", "        ok = \"merge_request_event\" in srcs and \"push\" in srcs and p.get(\"all_have_sha\") is True", "        ok = \"merge_request_event\" in srcs and \"push\" in srcs"),
 ("G5: a missing branch pipeline refutes even when none exists", "        refuted = p.get(\"branch_pipeline_exists\") is True and \"push\" not in srcs", "        refuted = \"push\" not in srcs"),
 ("G6: a pipeline that ran is suppression", "        suppressed = k.get(\"ran\") == 0", "        suppressed = True"),
 ("G6: suppression alone CONFIRMS (the manual start was not observed)", "        add(\"G6\", PARTIAL if suppressed else REFUTED,", "        add(\"G6\", CONFIRMED if suppressed else REFUTED,"),
 ("G7: no system note is still partial", "        add(\"G7\", PARTIAL if x7.get(\"system_note\") else OPEN,", "        add(\"G7\", PARTIAL,"),
 ("G8: field values alone CONFIRM", "        add(\"G8\", PARTIAL,\n", "        add(\"G8\", CONFIRMED,\n"),
 ("G10: a different user confirms", "        ok = u.get(\"me\") is not None and u.get(\"push\") == u.get(\"me\") and u.get(\"mr\") == u.get(\"me\")", "        ok = u.get(\"me\") is not None and u.get(\"mr\") == u.get(\"me\")"),
 ("G10: the MR pipeline's user is not looked at", "        ok = u.get(\"me\") is not None and u.get(\"push\") == u.get(\"me\") and u.get(\"mr\") == u.get(\"me\")", "        ok = u.get(\"me\") is not None and u.get(\"push\") == u.get(\"me\")"),
 ("G10: three unread ids are equal", "        ok = u.get(\"me\") is not None and u.get(\"push\") == u.get(\"me\") and u.get(\"mr\") == u.get(\"me\")", "        ok = u.get(\"push\") == u.get(\"me\") and u.get(\"mr\") == u.get(\"me\")"),
 ("G10: the human pusher CONFIRMS the bot assumption", "        add(\"G10\", PARTIAL if ok else REFUTED,", "        add(\"G10\", CONFIRMED if ok else REFUTED,"),
 ("G11: claimed", "    add(\"G11\", OPEN,", "    add(\"G11\", CONFIRMED,"),
 ("G9: claimed", "    add(\"G9\", OPEN,", "    add(\"G9\", CONFIRMED,"),
 ("lint: an error object is an observation", "    if not isinstance(static, dict) or static.get(\"valid\") is not True or not isinstance(static.get(\"merged_yaml\"), str):", "    if not isinstance(static, dict):"),
 ("lint: an invalid lint is an observation", "static.get(\"valid\") is not True or not isinstance(static.get(\"merged_yaml\"), str):", "not isinstance(static.get(\"merged_yaml\"), str):"),
 ("lint: context_sha is not compared with the commit", "i.get(\"context_sha\") == sha for i in incs)", "i.get(\"context_sha\") for i in incs)"),
 ("go lint: a lint of another commit is an observation", '    if not isinstance(lint, dict) or lint.get("sha") != sha or not isinstance(lint.get("valid"), bool):', '    if not isinstance(lint, dict) or not isinstance(lint.get("valid"), bool):'),
 ("go lint: an object with no `valid` is an observation", '    if not isinstance(lint, dict) or lint.get("sha") != sha or not isinstance(lint.get("valid"), bool):', '    if not isinstance(lint, dict) or lint.get("sha") != sha:'),
 ("go lint: context_sha is not compared with the commit", 'i.get("type") == "local" and i.get("context_sha") == sha\n', 'i.get("type") == "local" and i.get("context_sha")\n'),
 ("go lint: an include that is not local is `context ok`", 'isinstance(i, dict) and i.get("type") == "local" and i.get("context_sha") == sha\n', 'isinstance(i, dict) and i.get("context_sha") == sha\n'),
 ("go lint: no includes at all is `context ok`", '            "context_ok": bool(incs) and all(isinstance(i, dict) and i.get("type")', '            "context_ok": all(isinstance(i, dict) and i.get("type")'),
 ("go lint: nothing observed reads as observed", "    if not g:\n        return (\"gonk's own client", "    if False:\n        return (\"gonk's own client"),
 ("lint: no includes at all is `context ok`", "        \"context_ok\": bool(incs) and all(isinstance(i, dict) and i.get(\"context_sha\")", "        \"context_ok\": all(isinstance(i, dict) and i.get(\"context_sha\")"),
]

def run_tests():
    p = subprocess.run([PYTEST, T, "-q", "-x", "-p", "no:cacheprovider", "--color=no"], capture_output=True, text=True)
    failed = sorted(set(re.findall(r"^FAILED \S+::(\S+)", p.stdout, re.M)))
    return p.returncode, failed, p.stdout


def main():
    src = open(P).read()
    rc, _, out = run_tests()
    if rc != 0:
        print("the UNMUTATED tests are red; nothing was mutated:\n" + out)
        return 2
    bad = 0
    try:
        for i, (name, old, new) in enumerate(M):
            if src.count(old) != 1 or old == new:
                print(f"VACUOUS  [{i}] {name} (text occurs {src.count(old)} times)")
                bad += 1
                continue
            open(P, "w").write(src.replace(old, new, 1))
            rc, failed, out = run_tests()
            if rc == 0:
                print(f"SURVIVED [{i}] {name}")
                bad += 1
            elif not failed:
                print(f"BROKEN   [{i}] {name} (pytest exit {rc} with no failed test: does not import?)\n{out[-400:]}")
                bad += 1
            else:
                print(f"KILLED   [{i}] {name}\n           {failed[0]}")
                if SHOW:
                    print(out)
    finally:
        open(P, "w").write(src)
    print(f"killed={len(M) - bad} not-killed={bad}")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
