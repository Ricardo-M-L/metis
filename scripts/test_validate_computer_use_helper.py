#!/usr/bin/env python3

import json
from pathlib import Path
import stat
import tempfile
import unittest

import importlib.util


SCRIPT = Path(__file__).with_name("validate-computer-use-helper.py")
SPEC = importlib.util.spec_from_file_location("validate_computer_use_helper", SCRIPT)
VALIDATOR = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(VALIDATOR)


class ValidateComputerUseHelperTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.binary = Path(self.temp.name) / "metis-cu"

    def write_helper(self, descriptor, version_output=None):
        payload = json.dumps(descriptor)
        version_output = version_output or f"metis-cu {descriptor['version']}\n"
        payload = payload.replace("'", "'\\''")
        version_output = version_output.replace("'", "'\\''")
        script = f"""#!/bin/sh
if [ "$1" = "--describe" ] && [ "$2" = "--json" ]; then
  printf '%s\\n' '{payload}'
  exit 0
fi
if [ "$1" = "--version" ]; then
  printf '%s' '{version_output}'
  exit 0
fi
exit 2
"""
        self.binary.write_text(script, encoding="utf-8")
        self.binary.chmod(self.binary.stat().st_mode | stat.S_IXUSR)

    def descriptor(self):
        return {
            "name": "metis-cu",
            "version": "0.0.1-dev",
            "protocolVersion": 1,
            "platform": "darwin",
            "arch": "arm64",
            "capabilities": sorted(VALIDATOR.REQUIRED_CAPABILITIES),
            "permissions": {},
        }

    def test_accepts_managed_helper(self):
        self.write_helper(self.descriptor())
        result = VALIDATOR.validate(self.binary, "darwin-arm64", "0.0.1-dev")
        self.assertEqual(result["name"], "metis-cu")

    def test_rejects_plain_mcp_helper_without_descriptor(self):
        self.binary.write_text("#!/bin/sh\nprintf 'flag provided but not defined\\n' >&2\nexit 2\n", encoding="utf-8")
        self.binary.chmod(self.binary.stat().st_mode | stat.S_IXUSR)
        with self.assertRaisesRegex(VALIDATOR.HelperValidationError, "metadata command failed"):
            VALIDATOR.validate(self.binary, "darwin-arm64")

    def test_rejects_missing_lifecycle_capability(self):
        descriptor = self.descriptor()
        descriptor["capabilities"].remove("stop")
        self.write_helper(descriptor)
        with self.assertRaisesRegex(VALIDATOR.HelperValidationError, "missing required capabilities: stop"):
            VALIDATOR.validate(self.binary, "darwin-arm64")

    def test_rejects_target_and_version_mismatch(self):
        descriptor = self.descriptor()
        descriptor["arch"] = "amd64"
        self.write_helper(descriptor)
        with self.assertRaisesRegex(VALIDATOR.HelperValidationError, "does not match"):
            VALIDATOR.validate(self.binary, "darwin-arm64")

    def test_rejects_trailing_metadata(self):
        descriptor = self.descriptor()
        self.write_helper(descriptor)
        script = self.binary.read_text(encoding="utf-8").replace(
            "printf '%s\\n'", "printf '%s\\nextra\\n'"
        )
        self.binary.write_text(script, encoding="utf-8")
        with self.assertRaisesRegex(VALIDATOR.HelperValidationError, "trailing data"):
            VALIDATOR.validate(self.binary, "darwin-arm64")


if __name__ == "__main__":
    unittest.main()
