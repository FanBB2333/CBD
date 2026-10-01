#!/usr/bin/env python3
"""Pinned QDC507 USB AT access for Linux containers (probe or USB-to-PTY bridge).

Protocol/interface selection adapted from dontmove-kit at_pty_bridge.py,
Copyright (c) 2026 zxd, MIT; see ThirdParty/dontmove-kit.LICENSE.
No driver detachment, USB reset, configuration writes or command replay.
"""
import argparse
import errno
import json
import os
import pty
import re
import select
import signal
import sys
import time
import tty

SUPPORTED_IDS = {(0x2CA3, 0x4006), (0x2C7C, 0x0125)}
INTERFACE, EP_IN, EP_OUT = 2, 0x84, 0x03
READ_ONLY_COMMANDS = (
    "AT", "AT+CGMI", "AT+CGMM", "AT+CGMR", "AT+CPIN?", "AT+CEREG?",
    "AT+CSQ", 'AT+QCFG="usbcfg"', 'AT+QCFG="ims"',
)


def valid_usb_path(value):
    if not re.fullmatch(r"[1-9][0-9]*-[1-9][0-9]*(?:\.[1-9][0-9]*)*", value):
        raise ValueError("USB path must identify a physical port, e.g. 1-2 or 1-2.3")
    return value


def device_path(device):
    ports = device.port_numbers or ()
    return f"{device.bus}-" + ".".join(str(port) for port in ports)


def choose_device(devices, path):
    valid_usb_path(path)
    matches = [dev for dev in devices if (dev.idVendor, dev.idProduct) in SUPPORTED_IDS and device_path(dev) == path]
    if len(matches) != 1:
        raise RuntimeError(f"expected one supported module at USB {path}; found {len(matches)}")
    return matches[0]


def validate_interface(interface):
    if interface.bInterfaceNumber != INTERFACE or interface.bInterfaceClass != 0xFF:
        raise RuntimeError("USB interface 2 is not the expected vendor AT interface")
    bulk = {ep.bEndpointAddress for ep in interface if ep.bmAttributes & 3 == 2}
    if bulk != {EP_IN, EP_OUT}:
        raise RuntimeError(f"unexpected AT bulk endpoints: {sorted(bulk)}")


class USBAT:
    def __init__(self, path):
        import usb.core
        import usb.util
        self.core, self.util = usb.core, usb.util
        self.dev = choose_device(usb.core.find(find_all=True), path)
        self.claimed = False
        try:
            validate_interface(self.dev.get_active_configuration()[(INTERFACE, 0)])
            if self.dev.is_kernel_driver_active(INTERFACE):
                raise RuntimeError("AT interface already has a kernel driver; refusing to detach it")
            usb.util.claim_interface(self.dev, INTERFACE)
            self.claimed = True
        except Exception:
            self.close()
            raise

    def read(self, timeout=100):
        try:
            return bytes(self.dev.read(EP_IN, 512, timeout=timeout))
        except self.core.USBTimeoutError:
            return b""

    def write(self, data):
        count = self.dev.write(EP_OUT, data, timeout=2000)
        if count != len(data):
            raise RuntimeError("short USB AT write; refusing to replay a possibly accepted command")

    def flush(self):
        deadline = time.monotonic() + 1.0
        while time.monotonic() < deadline:
            if not self.read(100):
                return
        raise RuntimeError("AT input did not become quiet during bounded startup drain")

    def close(self):
        try:
            if self.claimed:
                self.claimed = False
                self.util.release_interface(self.dev, INTERFACE)
        finally:
            self.util.dispose_resources(self.dev)


def query(device, command, timeout=4):
    if command not in READ_ONLY_COMMANDS:
        raise ValueError("probe accepts read-only commands only")
    device.write((command + "\r").encode("ascii"))
    deadline = time.monotonic() + timeout
    buffer = b""
    lines = []
    while time.monotonic() < deadline:
        buffer += device.read(100)
        if len(buffer) > 16384:
            raise RuntimeError("AT response exceeded probe limit")
        while b"\n" in buffer:
            line, buffer = buffer.split(b"\n", 1)
            value = line.decode("ascii", errors="replace").strip()
            if not value or value == command:
                continue
            lines.append(value)
            if value == "OK":
                return {"command": command, "ok": True, "lines": lines}
            if value == "ERROR" or value.startswith(("+CME ERROR:", "+CMS ERROR:")):
                return {"command": command, "ok": False, "lines": lines}
    # Stop the entire probe on timeout: a late OK must not satisfy the next query.
    raise TimeoutError(f"no final response to {command}")


def probe(path):
    dev = USBAT(path)
    try:
        dev.flush()
        result = [query(dev, command) for command in READ_ONLY_COMMANDS]
        print(json.dumps({"usb_path": path, "queries": result}, ensure_ascii=False), flush=True)
        return 0 if result[0]["ok"] else 1
    finally:
        dev.close()


def drain_pty(master):
    """Discard pending bytes while disconnected. Never replay commands on reconnect."""
    while True:
        try:
            if not os.read(master, 4096):
                return
        except BlockingIOError:
            return


def write_pending(fd, pending):
    if pending:
        try:
            count = os.write(fd, pending)
            del pending[:count]
        except BlockingIOError:
            pass


def bridge(path, factory=USBAT):
    master, slave = pty.openpty()
    # Keep the slave open for the whole bridge lifetime. Raw mode prevents the
    # modem's responses echoing back as commands before the gateway opens it.
    tty.setraw(slave)
    os.set_blocking(master, False)
    dev = None
    pending = bytearray()
    stopped = False
    def stop(*_):
        nonlocal stopped
        stopped = True
    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    try:
        dev = factory(path)
        dev.flush()
        print(json.dumps({"tty": os.ttyname(slave), "usb_path": path}), flush=True)
        retry_at = 0.0
        while not stopped:
            if dev is None:
                drain_pty(master)
                if time.monotonic() < retry_at:
                    time.sleep(0.05)
                    continue
                retry_at = time.monotonic() + 1.0
                try:
                    dev = factory(path)
                    dev.flush()
                    drain_pty(master)
                    print("USB AT reconnected at pinned port", file=sys.stderr, flush=True)
                except Exception:
                    if dev is not None:
                        dev.close()
                    dev = None
                    continue
            try:
                readable, _, _ = select.select([master], [], [], 0)
                if readable:
                    data = os.read(master, 4096)
                    if data:
                        dev.write(data)
                data = dev.read(100)
                pending.extend(data)
                if len(pending) > 65536:
                    raise RuntimeError("PTY consumer stalled; refusing to lose AT framing")
                write_pending(master, pending)
            except OSError as error:
                if error.errno not in (errno.ENODEV, errno.ENXIO, errno.ESHUTDOWN):
                    raise
                print("USB AT disconnected; queued commands will not be replayed", file=sys.stderr, flush=True)
                try:
                    dev.close()
                except OSError:
                    pass
                dev = None
                pending.clear()
                retry_at = time.monotonic() + 1.0
        return 0
    finally:
        if dev is not None:
            dev.close()
        os.close(master)
        os.close(slave)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--usb-path", required=True, type=valid_usb_path)
    parser.add_argument("--mode", choices=("probe", "bridge"), default="probe")
    args = parser.parse_args()
    try:
        return probe(args.usb_path) if args.mode == "probe" else bridge(args.usb_path)
    except Exception as error:
        print(f"USB AT: {error}", file=sys.stderr, flush=True)
        return 1


if __name__ == "__main__":
    sys.exit(main())
