import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]
SCRIPT = Path(__file__).with_name("check_version.py")
VERSION_FILES = (
    Path("main.go"),
    Path("module/module.prop"),
    Path("webui/package.json"),
)


class CheckVersionTests(unittest.TestCase):
    def make_fixture(self):
        temp_dir = tempfile.TemporaryDirectory()
        self.addCleanup(temp_dir.cleanup)
        root = Path(temp_dir.name)
        for relative_path in VERSION_FILES:
            destination = root / relative_path
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(REPO_ROOT / relative_path, destination)
        return root

    def run_script(self, tag, root, github_output=None):
        env = os.environ.copy()
        env.pop("GITHUB_OUTPUT", None)
        if github_output is not None:
            env["GITHUB_OUTPUT"] = str(github_output)

        return subprocess.run(
            [
                sys.executable,
                str(SCRIPT),
                "--tag",
                tag,
                "--root",
                str(root),
            ],
            capture_output=True,
            check=False,
            env=env,
            text=True,
        )

    def assert_outputs(self, output, expected):
        actual = {}
        for line in output.splitlines():
            key, separator, value = line.partition("=")
            if separator:
                actual[key] = value
        self.assertEqual(actual, expected)

    def test_valid_release_tag_and_versions(self):
        root = self.make_fixture()

        result = self.run_script("v2.0.0", root)

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assert_outputs(
            result.stdout,
            {
                "base_version": "2.0.0",
                "version_code": "20000",
                "prerelease": "false",
            },
        )

    def test_version_mismatches_report_the_specific_field(self):
        cases = (
            (
                Path("main.go"),
                'version    = "2.0.0"',
                'version    = "9.9.9"',
                "main.go: field version",
            ),
            (
                Path("module/module.prop"),
                "version=2.0.0",
                "version=9.9.9",
                "module/module.prop: field version",
            ),
            (
                Path("webui/package.json"),
                '"version": "2.0.0"',
                '"version": "9.9.9"',
                "webui/package.json: field version",
            ),
            (
                Path("module/module.prop"),
                "versionCode=20000",
                "versionCode=99999",
                "module/module.prop: field versionCode",
            ),
        )

        for relative_path, original, replacement, field in cases:
            with self.subTest(field=field):
                root = self.make_fixture()
                path = root / relative_path
                path.write_text(
                    path.read_text(encoding="utf-8").replace(
                        original, replacement, 1
                    ),
                    encoding="utf-8",
                )

                result = self.run_script("v2.0.0", root)
                output = result.stdout + result.stderr

                self.assertEqual(result.returncode, 1, output)
                self.assertIn(field, output)
                self.assertIn("actual", output)
                self.assertIn("expected", output)

    def test_prerelease_tag_uses_base_version(self):
        root = self.make_fixture()

        result = self.run_script("v2.0.0-rc1", root)

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assert_outputs(
            result.stdout,
            {
                "base_version": "2.0.0",
                "version_code": "20000",
                "prerelease": "true",
            },
        )

    def test_invalid_tag_is_a_usage_error(self):
        root = self.make_fixture()

        for tag in ("2.0.0", "v2.0", "v2.0.0.1", "vX.Y.Z"):
            with self.subTest(tag=tag):
                result = self.run_script(tag, root)
                output = result.stdout + result.stderr

                self.assertEqual(result.returncode, 2, output)
                self.assertIn("invalid tag", output)

    def test_outputs_are_written_to_github_output_when_configured(self):
        root = self.make_fixture()
        github_output = root / "github-output.txt"

        result = self.run_script(
            "v2.0.0", root, github_output=github_output
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "")
        self.assert_outputs(
            github_output.read_text(encoding="utf-8"),
            {
                "base_version": "2.0.0",
                "version_code": "20000",
                "prerelease": "false",
            },
        )


if __name__ == "__main__":
    unittest.main()
