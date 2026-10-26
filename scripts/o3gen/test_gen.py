#!/usr/bin/env python3
"""Tests for gen.py's pinned-design gate (hk 2055).

Run: python3 scripts/o3gen/test_gen.py
"""
import hashlib
import os
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(HERE))
GEN = os.path.join(HERE, "gen.py")
FIXTURE = os.path.join(HERE, "testdata", "b1873a-sol-r4.md")
PACK = os.path.join(REPO, "testdata", "o3")
PINNED = "ce4f6efbe39514bb0bced2d0a66510e646058a66d0d28bbb417af180f375c19d"
DOC_KEY = "designs/hk1873/b1873a-sol-r4"


def run_gen(*args):
    return subprocess.run([sys.executable, GEN, *args],
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                          text=True)


def run_gen_reversed_walk(*args):
    """Run gen.py with os.walk descending directories in reverse order."""
    code = (
        "import os, sys\n"
        "walk = os.walk\n"
        "def rev(top, *a, **k):\n"
        "    for root, dirs, names in walk(top, *a, **k):\n"
        "        dirs.reverse()\n"
        "        yield root, dirs, names\n"
        "os.walk = rev\n"
        "sys.argv = ['gen.py'] + sys.argv[1:]\n"
        "src = open(%r).read()\n"
        "exec(compile(src, %r, 'exec'), {'__name__': '__main__',\n"
        "                              '__file__': %r})\n" % (GEN, GEN, GEN))
    return subprocess.run([sys.executable, "-c", code, *args],
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                          text=True)


def combined_lines(p):
    return (p.stdout + p.stderr).splitlines()


def tree_files(root):
    out = {}
    for base, _, names in os.walk(root):
        for n in names:
            p = os.path.join(base, n)
            out[os.path.relpath(p, root)] = p
    return out


class DesignGateTest(unittest.TestCase):
    def need_fixture(self):
        if not os.path.exists(FIXTURE):
            if os.environ.get("CI"):
                self.fail("pinned design fixture missing: " + FIXTURE)
            self.skipTest("pinned design fixture missing: " + FIXTURE)

    def test_missing_design_one_line_nothing_written(self):
        with tempfile.TemporaryDirectory() as td:
            missing = os.path.join(td, "no-such-design.md")
            out = os.path.join(td, "out")
            p = run_gen("--design", missing, "--out", out)
            self.assertNotEqual(p.returncode, 0)
            lines = combined_lines(p)
            self.assertEqual(1, len(lines), lines)
            line = lines[0]
            self.assertIn(missing, line)
            self.assertIn("hk:doc " + DOC_KEY, line)
            self.assertIn("--design", line)
            self.assertNotIn("Traceback", p.stderr)
            self.assertFalse(os.path.exists(out))

    def test_wrong_body_sha_line_nothing_written(self):
        self.need_fixture()
        with open(FIXTURE, "rb") as f:
            body = bytearray(f.read())
        body[0] ^= 0xFF  # one byte changed
        wrong_sha = hashlib.sha256(bytes(body)).hexdigest()
        self.assertNotEqual(PINNED, wrong_sha)
        with tempfile.TemporaryDirectory() as td:
            design = os.path.join(td, "design.md")
            with open(design, "wb") as f:
                f.write(body)
            out = os.path.join(td, "out")
            p = run_gen("--design", design, "--out", out)
            self.assertNotEqual(p.returncode, 0)
            lines = combined_lines(p)
            self.assertEqual(1, len(lines), lines)
            self.assertIn(PINNED, lines[0])
            self.assertIn(wrong_sha, lines[0])
            self.assertNotIn("Traceback", p.stderr)
            self.assertFalse(os.path.exists(out))

    def test_pinned_body_regenerates_pack_byte_exact(self):
        self.need_fixture()
        with tempfile.TemporaryDirectory() as td:
            out = os.path.join(td, "o3")
            p = run_gen("--design", FIXTURE, "--out", out)
            self.assertEqual(0, p.returncode, p.stderr)
            want = tree_files(PACK)
            got = tree_files(out)
            self.assertEqual(sorted(want), sorted(got))
            for rel, wp in sorted(want.items()):
                with open(wp, "rb") as f:
                    wb = f.read()
                with open(got[rel], "rb") as f:
                    gb = f.read()
                self.assertEqual(wb, gb, rel)

    def test_output_identical_under_reversed_readdir(self):
        self.need_fixture()
        with tempfile.TemporaryDirectory() as td:
            a = os.path.join(td, "a")
            b = os.path.join(td, "b")
            p = run_gen("--design", FIXTURE, "--out", a)
            self.assertEqual(0, p.returncode, p.stderr)
            q = run_gen_reversed_walk("--design", FIXTURE, "--out", b)
            self.assertEqual(0, q.returncode, q.stderr)
            fa, fb = tree_files(a), tree_files(b)
            self.assertEqual(sorted(fa), sorted(fb))
            for rel, ap in sorted(fa.items()):
                with open(ap, "rb") as f:
                    ab = f.read()
                with open(fb[rel], "rb") as f:
                    bb = f.read()
                self.assertEqual(ab, bb, rel)


if __name__ == "__main__":
    unittest.main()
