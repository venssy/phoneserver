#!/usr/bin/env python3
import argparse
import json
import os
import re
import sys
from pathlib import Path


TAG_PATTERN = re.compile(
    r"^v(\d+)\.(\d+)\.(\d+)(?:-([A-Za-z0-9.-]+))?$"
)


def parse_tag(tag):
    match = TAG_PATTERN.fullmatch(tag)
    if match is None:
        return None

    major, minor, patch, label = match.groups()
    base_version = f"{major}.{minor}.{patch}"
    version_code = int(major) * 10000 + int(minor) * 100 + int(patch)
    return base_version, version_code, label is not None


def read_text_field(path, pattern):
    try:
        text = path.read_text(encoding="utf-8")
    except OSError as exc:
        return None, f"could not read file: {exc}"

    match = re.search(pattern, text, re.MULTILINE)
    if match is None:
        return None, "field not found"
    return match.group(1).strip(), None


def read_package_version(path):
    try:
        package = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        return None, f"could not read JSON field: {exc}"

    if not isinstance(package, dict) or "version" not in package:
        return None, "field not found"

    value = package["version"]
    if not isinstance(value, str):
        return None, f"field is not a string: {value!r}"
    return value, None


def compare_field(problems, label, actual, expected, read_error=None):
    if read_error is not None:
        problems.append(
            f"ERROR {label}: actual unavailable ({read_error}); "
            f"expected {expected!r}"
        )
    elif actual != expected:
        problems.append(
            f"ERROR {label}: actual {actual!r}; expected {expected!r}"
        )


def write_outputs(base_version, version_code, prerelease):
    lines = (
        f"base_version={base_version}\n"
        f"version_code={version_code}\n"
        f"prerelease={'true' if prerelease else 'false'}\n"
    )
    github_output = os.environ.get("GITHUB_OUTPUT")

    if github_output:
        try:
            with open(github_output, "a", encoding="utf-8") as output_file:
                output_file.write(lines)
            return
        except OSError as exc:
            print(
                f"WARNING: cannot write GITHUB_OUTPUT ({exc}); "
                "writing outputs to stdout",
                file=sys.stderr,
            )

    sys.stdout.write(lines)


def main(argv=None):
    parser = argparse.ArgumentParser(
        description="Check repository version files against a release tag."
    )
    parser.add_argument("--tag", required=True, help="Git tag, for example v2.0.0")
    parser.add_argument("--root", required=True, type=Path, help="repository root")
    args = parser.parse_args(argv)

    parsed_tag = parse_tag(args.tag)
    if parsed_tag is None:
        parser.error(
            f"invalid tag {args.tag!r}; expected "
            "vMAJOR.MINOR.PATCH or vMAJOR.MINOR.PATCH-LABEL"
        )

    base_version, version_code, prerelease = parsed_tag
    root = args.root
    problems = []

    main_version, error = read_text_field(
        root / "main.go", r'^\s*version\s*=\s*"([^"]+)"'
    )
    compare_field(
        problems,
        "main.go: field version",
        main_version,
        base_version,
        error,
    )

    module_version, error = read_text_field(
        root / "module/module.prop", r"^version=(.*)$"
    )
    compare_field(
        problems,
        "module/module.prop: field version",
        module_version,
        base_version,
        error,
    )

    package_version, error = read_package_version(root / "webui/package.json")
    compare_field(
        problems,
        "webui/package.json: field version",
        package_version,
        base_version,
        error,
    )

    module_version_code, error = read_text_field(
        root / "module/module.prop", r"^versionCode=(.*)$"
    )
    compare_field(
        problems,
        "module/module.prop: field versionCode",
        module_version_code,
        str(version_code),
        error,
    )

    if problems:
        for problem in problems:
            print(problem, file=sys.stderr)
        return 1

    write_outputs(base_version, version_code, prerelease)
    return 0


if __name__ == "__main__":
    sys.exit(main())
