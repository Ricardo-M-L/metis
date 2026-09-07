"""Offline regression tests for the CLI-preview/stable release boundary."""

import copy
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock


SCRIPT = Path(__file__).with_name("release_contract.py")
spec = importlib.util.spec_from_file_location("release_contract", SCRIPT)
contract = importlib.util.module_from_spec(spec)
spec.loader.exec_module(contract)


class ReleaseContractTests(unittest.TestCase):
    def setUp(self):
        self.registry = {
            "schema_version": 1,
            "releases": {
                "v0.4.49": {
                    "channel": "cli-only-prerelease",
                    "prerelease": True,
                    "make_latest": False,
                    "reason": "Independent long-run CLI evaluation; no Desktop release.",
                }
            },
        }

    def metadata(self, tag="v0.4.49", *, draft=False):
        plan = contract.release_plan(self.registry, tag)
        return {
            "tag_name": tag,
            "draft": draft,
            "prerelease": plan["prerelease"],
            "assets": [
                {"name": name, "state": "uploaded", "size": 42}
                for name in plan["assets"]
            ],
        }

    def verify(self, meta=None, latest="v0.4.47", phase="published", **kwargs):
        contract.verify_release(
            self.registry, "v0.4.49", meta or self.metadata(),
            {"tag_name": latest}, phase=phase, **kwargs,
        )

    def test_registered_cli_preview_has_exactly_twelve_assets(self):
        plan = contract.release_plan(self.registry, "v0.4.49")
        self.assertEqual(plan["channel"], "cli-only-prerelease")
        self.assertEqual(len(plan["assets"]), 12)
        self.assertTrue(plan["prerelease"])
        self.assertFalse(plan["make_latest"])
        self.assertFalse(any("desktop" in name for name in plan["assets"]))
        self.verify()

    def test_unregistered_versions_keep_twenty_asset_stable_contract(self):
        plan = contract.release_plan(self.registry, "v0.4.50")
        self.assertEqual(plan["channel"], "stable")
        self.assertEqual(len(plan["assets"]), 20)
        self.assertFalse(plan["prerelease"])
        contract.verify_release(self.registry, "v0.4.50", self.metadata("v0.4.50"),
                                {"tag_name": "v0.4.50"}, phase="published")

    def test_registered_cli_stable_has_twelve_assets_and_is_not_prerelease(self):
        self.registry["releases"]["v0.4.49"].update(channel="cli-only-stable", prerelease=False)
        plan = contract.release_plan(self.registry, "v0.4.49")
        self.assertEqual(plan["channel"], "cli-only-stable")
        self.assertEqual(len(plan["assets"]), 12)
        self.assertIs(plan["prerelease"], False)
        self.assertIs(plan["make_latest"], False)
        self.verify()

    def test_cli_stable_rejects_prerelease_metadata_or_latest_promotion(self):
        self.registry["releases"]["v0.4.49"].update(channel="cli-only-stable", prerelease=False)
        meta = self.metadata()
        meta["prerelease"] = True
        with self.assertRaisesRegex(ValueError, "prerelease"):
            self.verify(meta)
        with self.assertRaisesRegex(ValueError, "latest"):
            self.verify(latest="v0.4.49")

    def test_cli_stable_registry_rejects_prerelease_or_latest_flags(self):
        for field in ("prerelease", "make_latest"):
            registry = copy.deepcopy(self.registry)
            entry = registry["releases"]["v0.4.49"]
            entry.update(channel="cli-only-stable", prerelease=False)
            entry[field] = True
            with self.assertRaises(ValueError):
                contract.release_plan(registry, "v0.4.49")

    def test_repository_registers_v0449_as_cli_stable_only(self):
        registry = contract.load_json(SCRIPT.parent.parent / ".github/cli-only-releases.json")
        plan = contract.release_plan(registry, "v0.4.49")
        self.assertEqual(plan["channel"], "cli-only-stable")
        self.assertIs(plan["prerelease"], False)
        self.assertIs(plan["make_latest"], False)
        self.assertEqual(len(plan["assets"]), 12)

    def test_repository_registers_v0450_as_cli_stable(self):
        registry = contract.load_json(SCRIPT.parent.parent / ".github/cli-only-releases.json")
        plan = contract.release_plan(registry, "v0.4.50")
        self.assertEqual(plan["channel"], "cli-only-stable")
        self.assertIs(plan["prerelease"], False)
        self.assertIs(plan["make_latest"], False)
        self.assertEqual(len(plan["assets"]), 12)

    def test_repository_registers_v0451_as_cli_stable(self):
        registry = contract.load_json(SCRIPT.parent.parent / ".github/cli-only-releases.json")
        plan = contract.release_plan(registry, "v0.4.51")
        self.assertEqual(plan["channel"], "cli-only-stable")
        self.assertIs(plan["prerelease"], False)
        self.assertIs(plan["make_latest"], False)
        self.assertEqual(len(plan["assets"]), 12)

    def test_graphql_lookup_fields_use_gh_arguments_without_shell_or_rest_pagination(self):
        result = mock.Mock(returncode=0, stdout='{"data": {}}')
        with mock.patch.object(contract.subprocess, "run", return_value=result) as run:
            self.assertEqual(contract.gh_api_json("graphql", fields={"query": "$query", "tag": "v0.4.51"}), {"data": {}})
        run.assert_called_once_with(["gh", "api", "graphql", "-f", "query=$query", "-f", "tag=v0.4.51"],
                                    capture_output=True, text=True, timeout=60)

    def test_graphql_http_failure_cannot_be_reported_as_release_absence(self):
        with mock.patch.object(contract.subprocess, "run", return_value=mock.Mock(returncode=1, stdout="")):
            with self.assertRaises(ValueError) as raised:
                contract.lookup_release("owner/metis", "v0.4.51")
        self.assertNotIsInstance(raised.exception, contract.ReleaseNotFound)

    def graphql_release(self, release):
        return {"data": {"repository": {"nameWithOwner": "owner/metis", "release": release}}}

    def test_draft_lookup_uses_graphql_when_actions_token_rest_list_omits_draft(self):
        draft = {"id": 383893785, "tag_name": "v0.4.49", "draft": True, "prerelease": False}
        requests = []
        def request(route, *, paginate=False, fields=None):
            requests.append(route)
            if route == "repos/owner/metis/releases?per_page=100":
                return [[{"id": 41, "tag_name": "v0.4.47"}]]
            if route == "graphql":
                self.assertEqual(fields["owner"], "owner")
                self.assertEqual(fields["name"], "metis")
                self.assertEqual(fields["tag"], "v0.4.49")
                return self.graphql_release({"databaseId": 383893785, "tagName": "v0.4.49"})
            if route == "repos/owner/metis/releases/383893785":
                return draft
            raise ValueError("HTTP 404 tag endpoint")
        with mock.patch.object(contract, "gh_api_json", side_effect=request):
            self.assertEqual(contract.lookup_release("owner/metis", "v0.4.49"), draft)
        self.assertEqual(requests, ["graphql", "repos/owner/metis/releases/383893785"])

    def test_draft_lookup_absence_is_distinct_from_transport_failure(self):
        with mock.patch.object(contract, "gh_api_json", return_value=self.graphql_release(None)):
            with self.assertRaises(contract.ReleaseNotFound):
                contract.lookup_release("owner/metis", "v0.4.50")
        with mock.patch.object(contract, "gh_api_json", side_effect=ValueError("HTTP 403")):
            with self.assertRaises(ValueError) as raised:
                contract.lookup_release("owner/metis", "v0.4.50")
            self.assertNotIsInstance(raised.exception, contract.ReleaseNotFound)

    def test_draft_lookup_rejects_graphql_errors_missing_fields_and_ambiguous_release(self):
        draft = {"databaseId": 10, "tagName": "v0.4.50"}
        for response in [[], {}, {"data": {"repository": None}},
                         {"data": {"repository": {"nameWithOwner": "owner/metis"}}},
                         {**self.graphql_release(None), "errors": [{"message": "FORBIDDEN"}]},
                         self.graphql_release([draft, draft]),
                         self.graphql_release({"databaseId": True, "tagName": "v0.4.50"}),
                         self.graphql_release({"databaseId": 10, "tagName": "v0.4.49"})]:
            with mock.patch.object(contract, "gh_api_json", return_value=response):
                with self.assertRaises(ValueError) as raised:
                    contract.lookup_release("owner/metis", "v0.4.50")
                self.assertNotIsInstance(raised.exception, contract.ReleaseNotFound)

    def test_draft_lookup_rejects_changed_id_or_tag_after_graphql(self):
        draft = {"databaseId": 10, "tagName": "v0.4.50"}
        for fetched in [{"id": 11, "tag_name": "v0.4.50"}, {"id": 10, "tag_name": "v0.4.49"}]:
            with mock.patch.object(contract, "gh_api_json", side_effect=[self.graphql_release(draft), fetched]):
                with self.assertRaises(ValueError):
                    contract.lookup_release("owner/metis", "v0.4.50")

    def test_cli_cannot_be_promoted_to_stable(self):
        meta = self.metadata()
        meta["prerelease"] = False
        with self.assertRaisesRegex(ValueError, "prerelease"):
            self.verify(meta)

    def test_cli_cannot_be_latest(self):
        with self.assertRaisesRegex(ValueError, "latest"):
            self.verify(latest="v0.4.49")

    def test_published_assets_are_immutable(self):
        with self.assertRaisesRegex(ValueError, "draft"):
            self.verify(phase="draft")

    def test_draft_is_not_accepted_as_published(self):
        with self.assertRaisesRegex(ValueError, "draft"):
            self.verify(self.metadata(draft=True))

    def test_partial_draft_allows_retry_but_publish_requires_complete_inventory(self):
        meta = self.metadata(draft=True)
        meta["assets"] = meta["assets"][:1]
        self.verify(meta, phase="draft", complete=False)
        with self.assertRaisesRegex(ValueError, "inventory"):
            self.verify(meta, phase="draft")

    def test_mixed_inventory_rejected_even_for_partial_draft(self):
        meta = self.metadata(draft=True)
        meta["assets"].append({"name": "metis-desktop-linux-amd64.tar.gz", "state": "uploaded", "size": 42})
        with self.assertRaisesRegex(ValueError, "inventory"):
            self.verify(meta, phase="draft", complete=False)

    def test_duplicate_asset_rejected(self):
        meta = self.metadata()
        meta["assets"].append(copy.deepcopy(meta["assets"][0]))
        with self.assertRaisesRegex(ValueError, "duplicate"):
            self.verify(meta)

    def test_bad_asset_state_or_empty_asset_rejected(self):
        for key, value in [("state", "starter"), ("size", 0), ("size", True)]:
            with self.subTest(key=key, value=value):
                meta = self.metadata()
                meta["assets"][0][key] = value
                with self.assertRaises(ValueError):
                    self.verify(meta)

    def test_wrong_tag_rejected(self):
        meta = self.metadata()
        meta["tag_name"] = "v0.4.48"
        with self.assertRaisesRegex(ValueError, "tag"):
            self.verify(meta)

    def test_missing_or_string_booleans_rejected(self):
        for field in ["draft", "prerelease"]:
            for value in [None, "false", 0]:
                meta = self.metadata()
                meta[field] = value
                with self.assertRaises(ValueError):
                    self.verify(meta)

    def test_non_semver_and_suffix_versions_rejected(self):
        for tag in ["main", "v0.4.49-rc.1", "v0.4.49+foo", "0.4.49", "v01.4.49"]:
            with self.assertRaises(ValueError):
                contract.release_plan(self.registry, tag)

    def test_registry_cannot_disable_preview_guards(self):
        for key, value in [("prerelease", False), ("make_latest", True),
                           ("prerelease", "true"), ("make_latest", 0),
                           ("channel", "stable"), ("extra", True), ("reason", "")]:
            registry = copy.deepcopy(self.registry)
            registry["releases"]["v0.4.49"][key] = value
            with self.assertRaises(ValueError):
                contract.release_plan(registry, "v0.4.49")

    def test_registry_schema_is_closed(self):
        for registry in [{}, {**self.registry, "schema_version": 2},
                         {**self.registry, "schema_version": True},
                         {**self.registry, "unknown": 1}]:
            with self.assertRaises(ValueError):
                contract.release_plan(registry, "v0.4.49")

    def test_stable_cannot_use_preview_inventory_or_metadata(self):
        meta = self.metadata("v0.4.50")
        meta["assets"] = self.metadata()["assets"]
        with self.assertRaisesRegex(ValueError, "inventory"):
            contract.verify_release(self.registry, "v0.4.50", meta,
                                    {"tag_name": "v0.4.47"}, phase="published")
        meta = self.metadata("v0.4.50")
        meta["prerelease"] = True
        with self.assertRaisesRegex(ValueError, "prerelease"):
            contract.verify_release(self.registry, "v0.4.50", meta,
                                    {"tag_name": "v0.4.47"}, phase="published")

    def test_missing_latest_evidence_rejected(self):
        with self.assertRaisesRegex(ValueError, "latest"):
            contract.verify_release(self.registry, "v0.4.49", self.metadata(),
                                    {}, phase="published")

    def test_dist_rejects_unexpected_files_symlinks_and_corrupt_checksum(self):
        import hashlib
        with tempfile.TemporaryDirectory() as name:
            root = Path(name)
            plan = contract.release_plan(self.registry, "v0.4.49")
            for asset in plan["assets"]:
                if asset.endswith(".sha256"):
                    target = asset[:-7]
                    (root / asset).write_text(hashlib.sha256(b"asset").hexdigest() + "  " + target + "\r\n")
                else:
                    (root / asset).write_bytes(b"asset")
            contract.verify_dist(root, plan["assets"])
            (root / ".unexpected").write_text("bad")
            with self.assertRaisesRegex(ValueError, "inventory"):
                contract.verify_dist(root, plan["assets"])
            (root / ".unexpected").unlink()
            archive = next(root.glob("*.zip"))
            archive.unlink()
            archive.symlink_to("/etc/hosts")
            with self.assertRaises(ValueError):
                contract.verify_dist(root, plan["assets"])
            archive.unlink()
            archive.write_bytes(b"corrupt")
            with self.assertRaisesRegex(ValueError, "checksum"):
                contract.verify_dist(root, plan["assets"])

    def build_evidence(self):
        run = {
            "id": 101, "workflow_id": 201, "path": ".github/workflows/release.yml",
            "event": "push", "status": "completed", "conclusion": "success",
            "head_sha": "a" * 40,
            "repository": {"id": 301, "full_name": "owner/metis"},
            "head_repository": {"id": 301, "full_name": "owner/metis"},
        }
        workflow = {"id": 201, "path": ".github/workflows/release.yml", "state": "active"}
        artifacts = {"total_count": 1, "artifacts": [{
            "id": 401, "name": "metis-release-v0.4.49", "expired": False, "size_in_bytes": 42,
            "workflow_run": {"id": 101, "repository_id": 301, "head_repository_id": 301,
                             "head_sha": "a" * 40},
        }]}
        return run, workflow, artifacts

    def provenance(self, run=None, workflow=None, artifacts=None):
        defaults = self.build_evidence()
        return contract.verify_build_run(
            "v0.4.49", "owner/metis", 101, "a" * 40,
            run or defaults[0], workflow or defaults[1], artifacts or defaults[2],
        )

    def test_provenance_accepts_only_exact_successful_release_run(self):
        self.assertEqual(self.provenance()["artifact_id"], 401)

    def test_provenance_rejects_wrong_source_workflow_run_or_status(self):
        for field, value in [("id", 102), ("workflow_id", 202),
                             ("path", ".github/workflows/ci.yml"),
                             ("head_sha", "b" * 40), ("event", "pull_request"),
                             ("status", "in_progress"), ("conclusion", "failure")]:
            with self.subTest(field=field):
                run = self.build_evidence()[0]
                run[field] = value
                with self.assertRaises(ValueError):
                    self.provenance(run=run)

    def test_provenance_rejects_fork_or_wrong_repository(self):
        for field in ["repository", "head_repository"]:
            for identity in [{"id": 302, "full_name": "attacker/metis"},
                             {"id": 302, "full_name": "owner/metis"}]:
                run = self.build_evidence()[0]
                run[field] = identity
                with self.assertRaises(ValueError):
                    self.provenance(run=run)

    def test_provenance_rejects_wrong_workflow_identity(self):
        for field, value in [("id", 202), ("path", ".github/workflows/ci.yml"),
                             ("state", "disabled_manually")]:
            workflow = self.build_evidence()[1]
            workflow[field] = value
            with self.assertRaises(ValueError):
                self.provenance(workflow=workflow)

    def test_provenance_rejects_missing_duplicate_expired_or_wrong_artifact(self):
        for field, value in [("name", "metis-release-v0.4.48"), ("expired", True),
                             ("expired", "false"), ("size_in_bytes", 0), ("id", True)]:
            artifacts = self.build_evidence()[2]
            artifacts["artifacts"][0][field] = value
            with self.assertRaises(ValueError):
                self.provenance(artifacts=artifacts)
        artifacts = self.build_evidence()[2]
        artifacts["artifacts"] *= 2
        artifacts["total_count"] = 2
        with self.assertRaises(ValueError):
            self.provenance(artifacts=artifacts)

    def test_provenance_rejects_artifact_from_another_run_or_commit(self):
        for field, value in [("id", 102), ("repository_id", 302),
                             ("head_repository_id", 302), ("head_sha", "b" * 40)]:
            artifacts = self.build_evidence()[2]
            artifacts["artifacts"][0]["workflow_run"][field] = value
            with self.assertRaises(ValueError):
                self.provenance(artifacts=artifacts)

    def test_provenance_rejects_truncated_artifact_listing(self):
        artifacts = self.build_evidence()[2]
        artifacts["total_count"] = 2
        with self.assertRaises(ValueError):
            self.provenance(artifacts=artifacts)

    def test_same_version_self_consistent_draft_from_different_build_rejected(self):
        import hashlib
        with tempfile.TemporaryDirectory() as name:
            root = Path(name)
            built, draft = root / "built", root / "draft"
            built.mkdir()
            draft.mkdir()
            assets = contract.release_plan(self.registry, "v0.4.49")["assets"]
            for directory in (built, draft):
                for asset in assets:
                    if asset.endswith(".sha256"):
                        archive = asset[:-7]
                        (directory / asset).write_text(hashlib.sha256(b"same-version").hexdigest() + "  " + archive + "\n")
                    else:
                        (directory / asset).write_bytes(b"same-version")
            contract.verify_build_bytes(built, draft, assets)
            archive = "metis-darwin-arm64.tar.gz"
            (draft / archive).write_bytes(b"same-version-but-different-commit")
            (draft / (archive + ".sha256")).write_text(
                hashlib.sha256((draft / archive).read_bytes()).hexdigest() + "  " + archive + "\n")
            contract.verify_dist(draft, assets)  # Old self-consistency gate accepts it.
            with self.assertRaisesRegex(ValueError, "build artifact bytes"):
                contract.verify_build_bytes(built, draft, assets)


if __name__ == "__main__":
    unittest.main()
