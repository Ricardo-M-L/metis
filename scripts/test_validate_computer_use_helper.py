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

    def write_helper(self, descriptor, version_output=None, *, list_error=False,
                     status_text=None, list_resources=None, pause_list=False,
                     flood_list=False, log_requests=False):
        def shell_quote(value):
            return "'" + value.replace("'", "'\\''") + "'"

        payload = json.dumps(descriptor)
        version_output = version_output or f"metis-cu {descriptor['version']}\n"
        if status_text is None:
            status_text = json.dumps({
                "name": "metis-cu", "version": descriptor["version"],
                "protocolVersion": 1, "permissions": descriptor["permissions"],
                "lifecycle": {"state": "idle"},
            })
        uri = "metis-cu://status?fixture=listed"
        resources = list_resources if list_resources is not None else [
            {"uri": uri, "name": "Computer use status", "mimeType": "application/json"}
        ]
        initialize = json.dumps({
            "jsonrpc": "2.0", "id": 1, "result": {
                "protocolVersion": "2024-11-05",
                "capabilities": {"resources": {}},
                "serverInfo": {"name": "metis-cu", "version": descriptor["version"]},
            },
        })
        listing = json.dumps({
            "jsonrpc": "2.0", "id": 2,
            **({"error": {"code": -32601, "message": "Method not found"}}
               if list_error else {"result": {"resources": resources}}),
        })
        reading = json.dumps({
            "jsonrpc": "2.0", "id": 3, "result": {"contents": [
                {"uri": uri, "mimeType": "application/json", "text": status_text}
            ]},
        })
        log_line = ""
        if log_requests:
            log_line = f"printf '%s\\n' \"$line\" >> {shell_quote(str(self.binary) + '.requests')}"
        list_action = (
            "sleep 2" if pause_list else
            "i=0; while [ \"$i\" -lt 70 ]; do printf '%1024s' x; i=$((i+1)); done" if flood_list else
            f"printf '%s\\n' {shell_quote(listing)}"
        )
        script = f"""#!/bin/sh
if [ "$1" = "--describe" ] && [ "$2" = "--json" ]; then
  printf '%s\\n' {shell_quote(payload)}
  exit 0
fi
if [ "$1" = "--version" ]; then
  printf '%s' {shell_quote(version_output)}
  exit 0
fi
while IFS= read -r line; do
  {log_line}
  case "$line" in
    *'"method":"initialize"'*) printf '%s\\n' {shell_quote(initialize)} ;;
    *'"method":"notifications/initialized"'*) ;;
    *'"method":"resources/list"'*) {list_action} ;;
    *'"method":"resources/read"'*)
      case "$line" in
        *'"uri":"{uri}"'*) printf '%s\\n' {shell_quote(reading)} ;;
        *) exit 4 ;;
      esac ;;
    *) exit 2 ;;
  esac
done
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
            "permissions": {"accessibility": "notGranted", "screenRecording": "notGranted"},
        }

    def test_accepts_managed_helper(self):
        self.write_helper(self.descriptor(), log_requests=True)
        result = VALIDATOR.validate(self.binary, "darwin-arm64", "0.0.1-dev")
        self.assertEqual(result["name"], "metis-cu")
        requests = [json.loads(line)["method"] for line in
                    Path(str(self.binary) + ".requests").read_text().splitlines()]
        self.assertEqual(requests, [
            "initialize", "notifications/initialized", "resources/list", "resources/read",
        ])

    def test_accepts_other_release_runner_architecture(self):
        descriptor = self.descriptor()
        descriptor["arch"] = "amd64"
        self.write_helper(descriptor)
        self.assertEqual(VALIDATOR.validate(self.binary, "darwin-amd64")["arch"], "amd64")

    def test_rejects_0_0_2_like_missing_resource_method(self):
        descriptor = self.descriptor()
        descriptor["version"] = "0.0.2"
        self.write_helper(descriptor, list_error=True)
        with self.assertRaisesRegex(VALIDATOR.HelperValidationError, "resources/list failed with error -32601"):
            VALIDATOR.validate(self.binary, "darwin-arm64", "0.0.2")

    def test_rejects_missing_or_inconsistent_status_resource(self):
        self.write_helper(self.descriptor(), list_resources=[])
        with self.assertRaisesRegex(VALIDATOR.HelperValidationError, "no Computer use status resource"):
            VALIDATOR.validate(self.binary, "darwin-arm64")
        self.write_helper(self.descriptor(), status_text=json.dumps({
            "name": "metis-cu", "version": "wrong", "protocolVersion": 1,
            "permissions": self.descriptor()["permissions"],
            "lifecycle": {"state": "idle"},
        }))
        with self.assertRaisesRegex(VALIDATOR.HelperValidationError, "disagrees with descriptor"):
            VALIDATOR.validate(self.binary, "darwin-arm64")

    def test_rejects_missing_permissions_and_unsupported_status(self):
        descriptor = self.descriptor()
        descriptor["permissions"].pop("accessibility")
        self.write_helper(descriptor)
        with self.assertRaisesRegex(VALIDATOR.HelperValidationError, "descriptor is missing required permission statuses"):
            VALIDATOR.validate(self.binary, "darwin-arm64")

        descriptor = self.descriptor()
        self.write_helper(descriptor, status_text=json.dumps({
            "name": "metis-cu", "version": descriptor["version"],
            "protocolVersion": 1, "permissions": {"accessibility": "granted"},
            "lifecycle": {"state": "idle"},
        }))
        with self.assertRaisesRegex(VALIDATOR.HelperValidationError, "status resource disagrees"):
            VALIDATOR.validate(self.binary, "darwin-arm64")

        self.write_helper(descriptor, status_text=json.dumps({
            "name": "metis-cu", "version": descriptor["version"],
            "protocolVersion": 1, "permissions": descriptor["permissions"],
            "lifecycle": {"state": "sleeping"},
        }))
        with self.assertRaisesRegex(VALIDATOR.HelperValidationError, "status resource disagrees"):
            VALIDATOR.validate(self.binary, "darwin-arm64")

    def test_mcp_probe_times_out_and_bounds_output(self):
        descriptor = self.descriptor()
        self.write_helper(descriptor, pause_list=True)
        with self.assertRaisesRegex(VALIDATOR.HelperValidationError, "resources/list timed out"):
            VALIDATOR.probe_status_resource(self.binary, descriptor, timeout=1.0)
        self.write_helper(descriptor, flood_list=True)
        with self.assertRaisesRegex(VALIDATOR.HelperValidationError, "exceeded output limit"):
            VALIDATOR.probe_status_resource(self.binary, descriptor)

    def test_metadata_probe_times_out_and_bounds_output(self):
        self.binary.write_text("#!/bin/sh\nsleep 2\n", encoding="utf-8")
        self.binary.chmod(self.binary.stat().st_mode | stat.S_IXUSR)
        with self.assertRaisesRegex(VALIDATOR.HelperValidationError, "metadata command timed out"):
            VALIDATOR.run_helper(self.binary, "--describe", "--json", timeout=0.2)
        self.binary.write_text("#!/bin/sh\nprintf '%70000s' x\n", encoding="utf-8")
        with self.assertRaisesRegex(VALIDATOR.HelperValidationError, "metadata command exceeded output limit"):
            VALIDATOR.run_helper(self.binary, "--describe", "--json")

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
