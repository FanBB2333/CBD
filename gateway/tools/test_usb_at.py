import errno
import json
import os
from pathlib import Path
import select
import subprocess
import sys
import time
from types import SimpleNamespace
import unittest

import usb_at


def device(vid=0x2CA3, pid=0x4006, bus=1, ports=(2,)):
    return SimpleNamespace(idVendor=vid, idProduct=pid, bus=bus, port_numbers=ports)


class SelectionTests(unittest.TestCase):
    def test_selection_pins_port_and_rejects_ambiguity(self):
        target = device()
        self.assertIs(usb_at.choose_device([device(bus=2), target], "1-2"), target)
        for devices in ([], [device(pid=1)], [target, device()]):
            with self.assertRaises(RuntimeError):
                usb_at.choose_device(devices, "1-2")
        for path in ("../1-2", "auto", "1-2/../../../", "0-0"):
            with self.assertRaises(ValueError):
                usb_at.valid_usb_path(path)

    def test_wrong_endpoint_or_interface_is_rejected(self):
        class Interface(list):
            bInterfaceNumber = 2
            bInterfaceClass = 0xFF
        interface = Interface([SimpleNamespace(bEndpointAddress=ep, bmAttributes=2) for ep in (0x84, 0x03)])
        usb_at.validate_interface(interface)
        interface[0].bEndpointAddress = 0x81
        with self.assertRaises(RuntimeError):
            usb_at.validate_interface(interface)

    def test_probe_rejects_mutations_and_handles_fragmented_response(self):
        class Fake:
            def __init__(self):
                self.writes = []
                self.chunks = [b"AT+CGMM\r\r\nQDC", b"507\r\nO", b"K\r\n"]
            def write(self, data): self.writes.append(data)
            def read(self, _): return self.chunks.pop(0)
        fake = Fake()
        for command in ("ATD10086;", "ATH", "AT+CFUN=1,1", 'AT+QCFG="usbcfg",1'):
            with self.assertRaises(ValueError):
                usb_at.query(fake, command)
        self.assertEqual(fake.writes, [])
        result = usb_at.query(fake, "AT+CGMM")
        self.assertEqual(result["lines"], ["QDC507", "OK"])
        self.assertTrue(result["ok"])


class BridgeTests(unittest.TestCase):
    def test_real_pty_roundtrip_disconnect_no_replay_and_shutdown(self):
        code = r'''
import errno, sys, time
sys.path.insert(0, sys.argv[1])
from usb_at import bridge
class Fake:
    def __init__(self, path): self.pending = []
    def flush(self): pass
    def close(self): pass
    def read(self, timeout):
        if self.pending: return self.pending.pop(0)
        time.sleep(0.01)
        return b""
    def write(self, data):
        print("write=" + repr(data), file=sys.stderr, flush=True)
        if b"DISCONNECT" in data: raise OSError(errno.ENODEV, "unplugged")
        self.pending.append(b"\r\nOK\r\n")
bridge("1-2", Fake)
'''
        child = subprocess.Popen([sys.executable, "-u", "-c", code, str(Path(__file__).parent)], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        tty_fd = None
        def cleanup():
            if tty_fd is not None: os.close(tty_fd)
            if child.poll() is None: child.terminate()
            try: child.wait(timeout=3)
            except subprocess.TimeoutExpired: child.kill(); child.wait()
            child.stdout.close()
            child.stderr.close()
        self.addCleanup(cleanup)
        self.assertTrue(select.select([child.stdout], [], [], 3)[0], "no PTY readiness")
        ready = json.loads(child.stdout.readline())
        self.assertEqual(ready["usb_path"], "1-2")
        tty_fd = os.open(ready["tty"], os.O_RDWR | os.O_NONBLOCK | os.O_NOCTTY)
        def response():
            self.assertTrue(select.select([tty_fd], [], [], 2)[0], "missing AT response")
            return os.read(tty_fd, 4096)
        os.write(tty_fd, b"AT\r")
        self.assertEqual(response(), b"\r\nOK\r\n")
        # Raw PTY must not echo OK back to USB and create a feedback loop.
        self.assertFalse(select.select([tty_fd], [], [], 0.15)[0])
        os.write(tty_fd, b"AT+DISCONNECT\r")
        log = b""
        deadline = time.monotonic() + 3
        while b"disconnected;" not in log and time.monotonic() < deadline:
            if select.select([child.stderr], [], [], 0.2)[0]: log += os.read(child.stderr.fileno(), 4096)
        self.assertIn(b"disconnected;", log)
        os.write(tty_fd, b"AT+MUST_NOT_REPLAY\r")
        while b"reconnected at" not in log and time.monotonic() < deadline:
            if select.select([child.stderr], [], [], 0.2)[0]: log += os.read(child.stderr.fileno(), 4096)
        self.assertIn(b"reconnected at", log)
        os.write(tty_fd, b"AT\r")
        self.assertEqual(response(), b"\r\nOK\r\n")
        child.terminate()
        self.assertEqual(child.wait(timeout=3), 0)
        log += child.stderr.read()
        self.assertNotIn(b"MUST_NOT_REPLAY", log)


if __name__ == "__main__":
    unittest.main()
