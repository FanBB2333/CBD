import contextlib
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

import init_usb


class InitTests(unittest.TestCase):
    @unittest.skipUnless(sys.platform == "linux", "production libcrypt is Linux")
    def test_unlock_matches_independent_openssl_vector(self):
        # openssl passwd -1 -salt 12345678 SH_adb_quectel
        self.assertEqual(init_usb.derive_key("12345678"), "0jXKXQwSwMxYoegx")
        with self.assertRaises(ValueError):
            init_usb.derive_key("1234567;reboot")

    def run_case(self, apply=False, config=None, mismatch=False, existing_backup=False):
        class Fake:
            def flush(self): pass
        commands = []
        config = config or '+QCFG: "usbcfg",0x2CA3,0x4006,1,1,1,1,1,0,0'
        target = 'AT+QCFG="usbcfg",0x2CA3,0x4006,1,1,1,1,1,1,1'
        with tempfile.TemporaryDirectory() as root:
            backup = Path(root) / "original.json"
            if existing_backup:
                backup.write_text("untouched")
            def exchange(dev, command, label):
                commands.append(command)
                if command == "AT+CGMM": return ["QDC507"]
                if command == "AT+CGMR": return ["QDC507GLEFM21"]
                if command == "AT+QADBKEY?": return ["+QADBKEY: 12345678"]
                if command == 'AT+QCFG="usbcfg"':
                    if target in commands and not mismatch:
                        return [target.replace('AT+QCFG=', '+QCFG: ')]
                    return [config]
                # Every mutation must follow a persisted baseline.
                self.assertTrue(backup.is_file())
                return []
            with patch.object(init_usb, "exchange", exchange), patch.object(init_usb, "derive_key", return_value="fixture"), contextlib.redirect_stdout(io.StringIO()):
                try:
                    init_usb.initialize(Fake(), "1-2", apply, str(backup))
                except (RuntimeError, FileExistsError) as error:
                    return commands, error
            if apply:
                saved = json.loads(backup.read_text())
                self.assertEqual(saved["original"], 'AT+QCFG="usbcfg",0x2CA3,0x4006,1,1,1,1,1,0,0')
                self.assertEqual(saved["target"], target)
            return commands, None

    def test_check_only_has_no_mutations(self):
        commands, error = self.run_case()
        self.assertIsNone(error)
        self.assertFalse(any(c.startswith('AT+QADBKEY=') or c.startswith('AT+CFUN=') for c in commands))
        self.assertEqual(commands.count('AT+QCFG="usbcfg"'), 1)

    def test_apply_preserves_identity_and_reboots_once(self):
        commands, error = self.run_case(apply=True)
        self.assertIsNone(error)
        self.assertEqual(commands[-1], "AT+CFUN=1,1")
        self.assertEqual(commands.count("AT+CFUN=1,1"), 1)
        self.assertIn('AT+QCFG="usbcfg",0x2CA3,0x4006,1,1,1,1,1,1,1', commands)

    def test_unknown_baseline_cannot_be_written(self):
        commands, error = self.run_case(apply=True, config='+QCFG: "usbcfg",0x2CA3,0x4006,1,1,1,1,0,0,0')
        self.assertIsInstance(error, RuntimeError)
        self.assertEqual(len(commands), 3)

    def test_readback_mismatch_does_not_reboot(self):
        commands, error = self.run_case(apply=True, mismatch=True)
        self.assertIsInstance(error, RuntimeError)
        self.assertNotIn("AT+CFUN=1,1", commands)

    def test_existing_backup_prevents_replaying_initialization(self):
        commands, error = self.run_case(apply=True, existing_backup=True)
        self.assertIsInstance(error, FileExistsError)
        self.assertFalse(any(c.startswith('AT+QADBKEY=') for c in commands))


if __name__ == "__main__":
    unittest.main()
