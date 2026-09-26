import json
import contextlib
import io
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from fork_release import check_versions, next_tag, prepare, select_base, stamp, validate_pr


class ForkReleaseTests(unittest.TestCase):
    def test_revision_is_numeric_and_scoped_to_base(self):
        self.assertEqual(next_tag("v0.1.6", [
            "v0.1.6-fork.2", "v0.1.6-fork.10", "v0.1.7-fork.99",
            "v0.1.6-fork.bad", "v0.1.6-fork.1-extra",
        ]), "v0.1.6-fork.11")
        self.assertEqual(next_tag("v0.1.7", []), "v0.1.7-fork.1")

    def test_only_reachable_stable_releases_are_bases(self):
        releases = [
            dict(tag_name=tag, published_at=date, draft=False, prerelease=prerelease)
            for tag, date, prerelease in [
                ("v0.1.5", "2026-09-24", False),
                ("v0.1.6", "2026-09-25", False),
                ("v0.1.7", "2026-09-26", False),
                ("v0.1.8", "2026-09-27", True),
            ]
        ]
        self.assertEqual(select_base(releases, lambda tag: tag != "v0.1.7"), "v0.1.6")
        with self.assertRaises(ValueError):
            select_base(releases, lambda tag: False)

    def test_pr_must_be_merged_into_this_fork(self):
        pr = {"merged": True, "base": {"ref": "custom/main",
              "repo": {"full_name": "gchust/ClinePassBridge"}},
              "merge_commit_sha": "a" * 40}
        self.assertEqual(validate_pr(pr, "gchust/ClinePassBridge"), "a" * 40)
        with self.assertRaises(ValueError):
            validate_pr(pr, "somebody/else")
        pr["merged"] = False
        with self.assertRaises(ValueError):
            validate_pr(pr, "gchust/ClinePassBridge")
        pr["merged"] = True
        pr["base"]["ref"] = "main"
        with self.assertRaises(ValueError):
            validate_pr(pr, "gchust/ClinePassBridge")

    def test_stamp_preserves_plugin_identity_and_matches_registry(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "marketplace").mkdir()
            (root / "internal/bridge").mkdir(parents=True)
            registry = {"schema_version": 1, "plugins": [{
                "id": "clinepassbridge", "version": "0.1.6",
                "author": "xiao-qiu-qiu", "install": {"type": "github-release"},
            }]}
            (root / "marketplace/registry.json").write_text(json.dumps(registry))
            source = root / "internal/bridge/types.go"
            source.write_text('package bridge\n\nconst Version = "0.1.6"\n')
            stamp(root, "v0.1.6-fork.1", "gchust/ClinePassBridge")
            result, code = check_versions(root)
            self.assertIn('const Version = "0.1.6-fork.1"', code)
            plugin = result["plugins"][0]
            self.assertEqual(plugin["author"], "xiao-qiu-qiu")
            self.assertEqual(plugin["repository"], "https://github.com/gchust/ClinePassBridge")
            self.assertIn("v0.1.6-fork.1/logo.png", plugin["logo"])
            with self.assertRaises(ValueError):
                stamp(root, "v0.1.6", "gchust/ClinePassBridge")
            source.write_text('package bridge\nconst Version = "wrong"\n')
            with self.assertRaises(ValueError):
                check_versions(root)

    def prepare_fixture(self):
        pr = {"merged": True, "base": {"ref": "custom/main",
              "repo": {"full_name": "gchust/ClinePassBridge"}},
              "merge_commit_sha": "a" * 40}
        releases = [[dict(tag_name="v0.1.6", published_at="2026-09-25",
                          draft=False, prerelease=False)]]
        return pr, releases

    def test_rerun_reuses_tag_and_skips_published_release(self):
        pr, releases = self.prepare_fixture()
        with patch("fork_release.git"), patch("fork_release.command") as run, \
                patch("fork_release.api", side_effect=[pr, releases, {"draft": False}]), \
                patch("fork_release.fork_tags", return_value={"v0.1.6-fork.4": "a" * 40}), \
                tempfile.TemporaryDirectory() as directory:
            run.return_value = subprocess.CompletedProcess([], 0, "", "")
            output = Path(directory) / "output"
            with patch.dict(os.environ, {"GITHUB_OUTPUT": str(output)}), \
                    contextlib.redirect_stdout(io.StringIO()):
                prepare("gchust/ClinePassBridge", 12)
            self.assertIn("tag=v0.1.6-fork.4\n", output.read_text())
            self.assertIn("publish=false\n", output.read_text())
            self.assertFalse(any(call.args[0] == "gh" for call in run.call_args_list))

    def test_concurrent_tag_collision_allocates_next_revision(self):
        pr, releases = self.prepare_fixture()
        attempts = []

        def run(*args, **kwargs):
            if args[0] == "gh":
                attempts.append(args)
                if len(attempts) == 1:
                    return subprocess.CompletedProcess(args, 1, "Reference already exists", "HTTP 422")
            return subprocess.CompletedProcess(args, 0, "{}", "")

        with patch("fork_release.git"), patch("fork_release.command", side_effect=run), \
                patch("fork_release.api", side_effect=[pr, releases, None]), \
                patch("fork_release.fork_tags", side_effect=[
                    {"v0.1.6-fork.1": "b" * 40},
                    {"v0.1.6-fork.1": "b" * 40, "v0.1.6-fork.2": "c" * 40},
                ]), tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "output"
            with patch.dict(os.environ, {"GITHUB_OUTPUT": str(output)}), \
                    contextlib.redirect_stdout(io.StringIO()):
                prepare("gchust/ClinePassBridge", 13)
            self.assertEqual(len(attempts), 2)
            self.assertIn("ref=refs/tags/v0.1.6-fork.2", attempts[0])
            self.assertIn("ref=refs/tags/v0.1.6-fork.3", attempts[1])
            self.assertIn("tag=v0.1.6-fork.3\n", output.read_text())
            self.assertIn("publish=true\n", output.read_text())


if __name__ == "__main__":
    unittest.main()
