import importlib.util
import os
from pathlib import Path
import unittest
from unittest.mock import patch

SPEC=importlib.util.spec_from_file_location("desktop_delivery",Path(__file__).with_name("desktop_bundled_runtime_delivery_check.py"))
DELIVERY=importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(DELIVERY)


class ProcessOwnershipTests(unittest.TestCase):
    def test_interrupt_enters_exception_cleanup(self):
        with self.assertRaisesRegex(InterruptedError, "signal 15"):
            DELIVERY.interrupted(DELIVERY.signal.SIGTERM, None)

    def test_only_exact_private_root_and_separate_process_groups_are_owned(self):
        root=Path("/private/tmp/mdb-unique")
        output=(f"12345 23456 {root}/Test install with spaces/METIS.app/Contents/MacOS/metis-desktop\n"
                f"12346 23456 {root}/source/metis-desktop/helper\n"
                f"12347 23457 {root}-unrelated/METIS.app/program\n"
                "12348 23458 /Applications/METIS.app/Contents/MacOS/metis-desktop\n"
                f"{os.getpid()} 23459 {root}/source/current-harness\n"
                f"12349 {os.getpgrp()} {root}/source/same-group\n")
        with patch.object(DELIVERY.subprocess,"check_output",return_value=output):
            self.assertEqual(DELIVERY.owned_processes(root),[(12345,23456),(12346,23456)])

    def test_cleanup_signals_only_recorded_owned_group(self):
        with patch.object(DELIVERY,"owned_processes",side_effect=[[(12345,23456)],[],[],[],[]]), \
                patch.object(DELIVERY.os,"killpg") as kill:
            self.assertEqual(DELIVERY.stop_owned_processes(Path("/private/tmp/mdb-owned")),[])
            kill.assert_called_once_with(23456,DELIVERY.signal.SIGTERM)


if __name__=="__main__":
    unittest.main()
