"""Offline fail-closed tests for the default-branch PR triage policy.

These tests mock every GitHub call. They never read credentials, contact the
network, change labels or enroll any real pull request in auto-merge.
"""
import importlib.util
import os
from pathlib import Path
import unittest
from unittest import mock


SCRIPT = Path(__file__).with_name("pr_backlog_triage.py")
TEST_REPO = "MulattoTech/agent-orchestrator"


def load_triage():
    with mock.patch.dict(os.environ, {
        "GITHUB_REPOSITORY": TEST_REPO,
        "GITHUB_TOKEN": "synthetic-never-a-real-token",
    }):
        spec = importlib.util.spec_from_file_location("triage_isolated_test", SCRIPT)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module


def success(name):
    return {"name": name, "status": "completed", "conclusion": "success"}


def completed(name, conclusion):
    return {"name": name, "status": "completed", "conclusion": conclusion}


def suite():
    return [success("ci-ok"), success("scan"), success("check"),
            completed("go", "skipped"), completed("frontend", "skipped")]


def pr(labels=(), *, draft=False, base="main", author="MulattoTech",
       head_repo=TEST_REPO, state="clean"):
    return {
        "number": 17, "node_id": "SYNTHETIC_PullRequest_17",
        "draft": draft, "mergeable_state": state,
        "user": {"login": author},
        "head": {"sha": "synthetic-commit", "repo": {"full_name": head_repo}},
        "base": {"ref": base},
        "labels": [{"name": x} for x in labels],
        "updated_at": "2026-11-20T00:00:00Z",
        "auto_merge": None,
        "changed_files": 1, "additions": 2, "deletions": 1,
    }


class GuardrailTests(unittest.TestCase):
    def setUp(self):
        self.triage = load_triage()
        # A forgotten mock must never initiate HTTP; no real token exists.
        self.network = mock.patch.object(
            self.triage.urllib.request, "urlopen",
            side_effect=AssertionError("test attempted live GitHub HTTP"),
        )
        self.network.start()
        self.addCleanup(self.network.stop)

    def check_result(self, runs, *, statuses=(), total=None):
        def get(path):
            if path.endswith("/check-runs?per_page=100"):
                response = {"check_runs": runs}
                if total is not None:
                    response["total_count"] = total
                return response
            if path.endswith("/status"):
                return {"statuses": list(statuses)}
            raise AssertionError("unexpected GitHub path: " + path)
        with mock.patch.object(self.triage, "get", side_effect=get):
            return self.triage.checks("synthetic-commit")

    def simulate_main(self, proposal, *, runs=None, age=0,
                      repo_auto_merge=True):
        calls = []
        runs = suite() if runs is None else runs

        def get(path):
            if path == "":
                return {"archived": False, "disabled": False,
                        "allow_auto_merge": repo_auto_merge,
                        "default_branch": "main"}
            if path == "/pulls/17":
                return proposal
            if path == "/commits/synthetic-commit/check-runs?per_page=100":
                return {"check_runs": runs, "total_count": len(runs)}
            if path == "/commits/synthetic-commit/status":
                return {"statuses": []}
            raise AssertionError("unapproved metadata read: " + path)

        def api(method, path, payload=None, **kwargs):
            calls.append((method, path, payload, kwargs))
            return {"data": {"enablePullRequestAutoMerge": {}}}

        with mock.patch.object(self.triage, "get", side_effect=get), \
             mock.patch.object(self.triage, "prs", return_value=[proposal]), \
             mock.patch.object(self.triage, "code_age_days", return_value=age), \
             mock.patch.object(self.triage, "set_label",
                               return_value={x["name"] for x in proposal["labels"]}), \
             mock.patch.object(self.triage, "api", side_effect=api):
            with mock.patch.dict(os.environ, {"GITHUB_EVENT_NAME": "schedule"}):
                self.triage.main()
        return calls

    def test_valid_named_successes_with_optional_skipped_jobs(self):
        self.assertEqual(self.check_result(suite()), (True, False))

    def test_all_skipped_neutral_and_legacy_success_never_qualify(self):
        self.assertEqual(self.check_result(
            [completed("ci-ok", "skipped"),
             completed("scan", "skipped"), completed("check", "neutral")],
            statuses=[{"state": "success"}]), (False, False))

    def test_absent_named_success_fails_closed(self):
        for absent in ("ci-ok", "check", "scan"):
            with self.subTest(absent=absent):
                self.assertEqual(self.check_result([
                    x for x in suite() if x["name"] != absent]), (False, False))

    def test_required_skipped_or_neutral_fails_closed(self):
        for named in ("ci-ok", "scan", "check"):
            for bad in ("neutral", "skipped"):
                with self.subTest(named=named, status=bad):
                    changed = [completed(x["name"], bad)
                               if x["name"] == named else x for x in suite()]
                    self.assertEqual(self.check_result(changed), (False, False))

    def test_duplicate_named_success_and_skipped_is_not_ready(self):
        self.assertEqual(self.check_result(
            suite() + [completed("ci-ok", "skipped")]), (False, False))

    def test_pending_and_failed_checks_preserve_original_status(self):
        pending = suite() + [{"name": "other", "status": "queued", "conclusion": None}]
        self.assertEqual(self.check_result(pending), (False, False))
        failing = suite() + [completed("other", "failure")]
        self.assertEqual(self.check_result(failing), (False, True))
        failing_legacy = [{"state": "failure"}]
        self.assertEqual(self.check_result(suite(), statuses=failing_legacy), (False, True))

    def test_truncated_paginated_check_runs_fail_closed(self):
        self.assertEqual(self.check_result(suite(), total=101), (False, False))

    def test_no_check_runs_even_with_legacy_success_is_not_ready(self):
        self.assertEqual(self.check_result([], statuses=[{"state": "success"}]),
                         (False, False))

    def test_metadata_only_documentation_does_not_auto_enroll(self):
        proposal = pr()
        calls = self.simulate_main(proposal)
        self.assertFalse(any("/graphql" in path for _, path, _, _ in calls))
        self.assertFalse(any("/files" in path for _, path, _, _ in calls))

    def test_explicit_optin_can_arm_with_all_named_successes(self):
        calls = self.simulate_main(pr(["agent:automerge"]))
        graphql = [x for x in calls if x[1] == "/graphql"]
        self.assertEqual(len(graphql), 1)
        self.assertEqual(graphql[0][0], "POST")
        self.assertEqual(graphql[0][2]["variables"]["id"],
                         "SYNTHETIC_PullRequest_17")

    def test_absent_security_check_never_arms_even_with_optin(self):
        calls = self.simulate_main(pr(["agent:automerge"]), runs=[
            x for x in suite() if x["name"] != "scan"])
        self.assertFalse(any(x[1] == "/graphql" for x in calls))

    def test_draft_hold_untrusted_and_nondefault_base_rejected(self):
        proposals = (
            pr(["agent:automerge"], draft=True),
            pr(["agent:automerge", "agent:hold"]),
            pr(["agent:automerge"], base="development"),
            pr(["agent:automerge"], author="other-user"),
            pr(["agent:automerge"], head_repo="other/repository"),
        )
        for proposal in proposals:
            with self.subTest(proposal=proposal):
                calls = self.simulate_main(proposal)
                self.assertFalse(any(x[1] == "/graphql" for x in calls))
                self.assertFalse(any(x[0] == "PUT" for x in calls))

    def test_auto_merge_disabled_rejected(self):
        calls = self.simulate_main(pr(["agent:automerge"]), repo_auto_merge=False)
        self.assertFalse(any(x[1] == "/graphql" for x in calls))

    def test_aged_proposal_rejected(self):
        calls = self.simulate_main(pr(["agent:automerge"]), age=32)
        self.assertFalse(any(x[1] == "/graphql" for x in calls))

    def test_opted_in_behind_proposal_only_requests_update(self):
        calls = self.simulate_main(pr(["agent:automerge"], state="behind"))
        self.assertEqual(len([x for x in calls if x[0] == "PUT"]), 1)
        self.assertFalse(any(x[1] == "/graphql" for x in calls))


if __name__ == "__main__":
    unittest.main()
