#!/usr/bin/python3
"""Tests for the test server's setup text and job manager. Usage: python3 platform/runner/test_runner.py"""

import os
import re
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(os.path.dirname(HERE))
sys.path.insert(0, HERE)
import build  # noqa: E402
import manager  # noqa: E402


class SetupText(unittest.TestCase):
    def test_valid_and_small_enough(self):
        text, files = build.build()
        build.server.validate(text, files)
        self.assertTrue(text.startswith("#cloud-config\n"))

    def test_no_secrets(self):
        text, _ = build.build()
        self.assertNotRegex(text, r"gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}")

    def test_nothing_comes_in(self):
        self.assertNotIn("tcp dport", build.NFTABLES)
        text, _ = build.build()
        self.assertIn("ssh.service", text)

    def test_slot_users_match_firewall_rule(self):
        # The metadata rule covers uids from 2000; slot users start at 2001.
        text, _ = build.build()
        uids = [int(u) for u in re.findall(r"--uid, '(\d+)'", text)]
        self.assertEqual(len(uids), build.SLOTS)
        self.assertTrue(all(u > 2000 for u in uids))

    def test_playwright_version_matches_pipeline(self):
        with open(os.path.join(ROOT, ".github", "workflows", "pipeline.yml")) as f:
            m = re.search(r"PLAYWRIGHT_VERSION:\s*([0-9.]+)", f.read())
        self.assertEqual(m.group(1), build.PLAYWRIGHT_VERSION)


class Runner(unittest.TestCase):
    RELEASE = {
        "assets": [
            {"name": "actions-runner-osx-x64-2.330.0.tar.gz", "browser_download_url": "https://x/osx"},
            {"name": "actions-runner-linux-x64-2.330.0.tar.gz", "browser_download_url": "https://x/linux"},
        ],
        "body": "- Linux x64 <!-- BEGIN SHA linux-x64 -->" + "a" * 64 + "<!-- END SHA linux-x64 -->\n"
                "- OSX x64 <!-- BEGIN SHA osx-x64 -->" + "b" * 64 + "<!-- END SHA osx-x64 -->",
    }

    def test_asset_and_hash(self):
        self.assertEqual(manager.runner_asset(self.RELEASE), ("2.330.0", "https://x/linux", "a" * 64))

    def test_no_hash_no_download(self):
        with self.assertRaises(ValueError):
            manager.runner_asset({"assets": self.RELEASE["assets"], "body": "nothing"})

    def test_job_command_isolates(self):
        cmd = manager.job_command(2, "labr2", 2)
        self.assertIn("--uid=labr2", cmd)
        self.assertIn("--property=PrivateTmp=yes", cmd)
        self.assertIn("--property=NoNewPrivileges=yes", cmd)
        self.assertIn("--property=OOMPolicy=continue", cmd)
        self.assertIn("--property=ReadWritePaths=/home/labr2", cmd)
        self.assertIn("--property=MemoryMax=45%", cmd)
        # The registration is never on a command line the unit shows.
        self.assertFalse(any("jitconfig " in c and "$(cat" not in c for c in cmd))

    def test_placeholder_is_no_token(self):
        import tempfile
        with tempfile.NamedTemporaryFile("w", delete=False) as f:
            f.write("PASTE_TOKEN_HERE\n")
        old, manager.TOKEN = manager.TOKEN, f.name
        try:
            self.assertIsNone(manager.read_token())
            with open(f.name, "w") as g:
                g.write(" github_pat_abc\n")
            self.assertEqual(manager.read_token(), "github_pat_abc")
        finally:
            manager.TOKEN = old
            os.remove(f.name)

    def test_jobs_cannot_read_setup_text(self):
        # The token can come with the setup text, which the metadata service serves.
        self.assertIn("meta skuid >= 2000 ip daddr 169.254.169.254 drop", build.NFTABLES)

    GOOD = "github_pat_11ABCDEFG0" + "a1B2_c3D4e" * 7 + "XY"

    def test_good_token_shape(self):
        self.assertEqual(len(self.GOOD), 93)
        self.assertIsNone(manager.shape_problem(self.GOOD))

    def test_wrong_kinds_are_named(self):
        self.assertIn("registration token", manager.shape_problem("AACXZXTBPCD6FACWEONRCH3K5W47U"))
        self.assertIn("classic token", manager.shape_problem("ghp_" + "a" * 36))
        self.assertIn("does not look like", manager.shape_problem("hello"))
        self.assertIn("too short", manager.shape_problem(self.GOOD[:40]))
        self.assertIn("never has (- ?)", manager.shape_problem(self.GOOD[:30] + "?-" + self.GOOD[32:]))

    def test_paste_leftovers_are_removed(self):
        self.assertEqual(manager.clean('  "%s"\r\n' % self.GOOD), self.GOOD)

    def test_description_hides_the_token(self):
        d = manager.describe(self.GOOD)
        self.assertEqual(d, "93 characters, github_pat_..." + self.GOOD[-4:])
        self.assertNotIn(self.GOOD[11:-4], d)

    def test_check_token_explains_github_answers(self):
        import io
        import urllib.error
        old = manager.api
        try:
            def refuse(code):
                def f(*a, **k):
                    raise urllib.error.HTTPError("u", code, "x", {}, io.BytesIO(b""))
                return f
            manager.api = refuse(401)
            self.assertIn("does not know this token", manager.check_token("OWNER/REPO", self.GOOD))
            manager.api = refuse(403)
            self.assertIn("Administration", manager.check_token("OWNER/REPO", self.GOOD))
            manager.api = lambda *a, **k: {"total_count": 0}
            self.assertIsNone(manager.check_token("OWNER/REPO", self.GOOD))
            # A token of the wrong kind never even reaches GitHub.
            manager.api = refuse(500)
            self.assertIn("registration", manager.check_token("OWNER/REPO", "AACXZXTBPCD6FACWEONRCH3K5W47U"))
        finally:
            manager.api = old

    def test_token_from_the_setup_texts_last_line(self):
        text, _ = build.build()
        self.assertTrue(text.rstrip("\n").endswith("# token: PASTE_TOKEN_HERE"))
        self.assertIsNone(manager.token_from_setup(text))
        for line in ("# token: %s", "#token:%s", "# token:  '%s'  ", "# Token %s", "token: %s"):
            pasted = text.replace("# token: PASTE_TOKEN_HERE", line % self.GOOD)
            self.assertEqual(manager.token_from_setup(pasted), self.GOOD, line)
            # Whatever the spacing, the setup text stays valid.
            build.server.validate(pasted, build.build()[1])
        # The explanation above it mentions 'token:' but is not the token line.
        self.assertEqual(len(re.findall(r"(?im)^#\s*token\b", text)), 1)

    def test_slots_follow_memory(self):
        gb = 2**30
        self.assertEqual(manager.slots_for_memory(int(0.9 * gb)), 1)
        self.assertEqual(manager.slots_for_memory(int(3.7 * gb)), 1)  # CX23
        self.assertEqual(manager.slots_for_memory(int(7.6 * gb)), 2)  # CX33
        self.assertEqual(manager.slots_for_memory(0), 1)

    def test_playwright_version_from_pipeline(self):
        with open(os.path.join(ROOT, ".github", "workflows", "pipeline.yml")) as f:
            self.assertEqual(manager.playwright_version(f.read()), build.PLAYWRIGHT_VERSION)
        self.assertIsNone(manager.playwright_version("env: {}"))

    def test_selftest_accepts_this_manager_and_refuses_a_broken_one(self):
        import json
        import tempfile
        with tempfile.TemporaryDirectory() as d:
            cfg = os.path.join(d, "config.json")
            with open(cfg, "w") as f:
                json.dump({"repo": "OWNER/REPO", "slots": 2}, f)
            with open(os.path.join(HERE, "manager.py"), "rb") as f:
                good = f.read()
            self.assertTrue(manager.selftest(good, cfg))
            self.assertFalse(manager.selftest(b"def broken(:\n", cfg))
            self.assertFalse(manager.selftest(good.replace(b"def main():", b"def main():\n    raise SystemExit(3)"), cfg))

    def test_update_installs_changed_files_from_main_only(self):
        import tempfile
        with tempfile.TemporaryDirectory() as d:
            files = {"platform/runner/manager.py": b"new manager", "platform/runner/lab-runner-token": b"same",
                     "platform/runner/install-browser-libs": b"new libs",
                     ".github/workflows/pipeline.yml": b"env:\n  PLAYWRIGHT_VERSION: 1.56.0\n"}
            asked = []

            def raw(path, token):
                asked.append(path)
                return files[path.split("/contents/")[1].split("?")[0]]
            dests = {p: os.path.join(d, os.path.basename(p)) for p in files}
            for p in dests:
                with open(dests[p], "wb") as f:
                    f.write(b"same" if "token" in p else b"old")
            saved = (manager.api_raw, manager.UPDATED_FILES, manager.SELF, manager.LIBS_STATE, manager.selftest)
            try:
                manager.api_raw = raw
                manager.SELF = dests["platform/runner/manager.py"]
                manager.UPDATED_FILES = [(p, dests[p]) for p in files if not p.endswith(".yml")]
                manager.LIBS_STATE = os.path.join(d, "libs")
                with open(manager.LIBS_STATE, "w") as f:
                    f.write("1.56.0\n")
                manager.selftest = lambda code, config=None: True
                self.assertTrue(manager.update_once("OWNER/REPO", "t"))
                with open(manager.SELF, "rb") as f:
                    self.assertEqual(f.read(), b"new manager")
                self.assertTrue(all(p.endswith("?ref=main") for p in asked))
                # Nothing changed the second time: no restart.
                self.assertFalse(manager.update_once("OWNER/REPO", "t"))
                # A manager that does not start is never installed.
                files["platform/runner/manager.py"] = b"newer but broken"
                manager.selftest = lambda code, config=None: False
                self.assertFalse(manager.update_once("OWNER/REPO", "t"))
                with open(manager.SELF, "rb") as f:
                    self.assertEqual(f.read(), b"new manager")
            finally:
                (manager.api_raw, manager.UPDATED_FILES, manager.SELF, manager.LIBS_STATE, manager.selftest) = saved

    def test_runner_env_marks_self_hosted(self):
        self.assertIn("LAB_RUNNER=1", manager.runner_env())


if __name__ == "__main__":
    unittest.main()
