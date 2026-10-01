#!/usr/bin/env python3
"""Explicit, one-shot QDC507 ADB/UAC initialization; check-only by default.

Preserves all USB identity/function values except ADB and audio. Never called
by the gateway. Unlock uses libcrypt, not a custom MD5-crypt implementation.
The standard Quectel challenge derivation is documented by EFF's Rayhunter
installer (installer/src/pinephone.rs); no credentials are logged or saved.
"""
import argparse
import errno
import json
import os
import re
import time

from usb_at import USBAT, valid_usb_path

EXPECTED = (0x2CA3, 0x4006, 1, 1, 1, 1, 1, 0, 0)


def exchange(dev, command, label, timeout=8):
    dev.write((command + "\r").encode("ascii"))
    deadline = time.monotonic() + timeout
    buffer, lines = b"", []
    while time.monotonic() < deadline:
        buffer += dev.read(100)
        if len(buffer) > 16384:
            raise RuntimeError(f"{label}: response too large")
        while b"\n" in buffer:
            line, buffer = buffer.split(b"\n", 1)
            line = line.decode("ascii", errors="replace").strip()
            if not line or line == command:
                continue
            if line == "OK":
                return lines
            if line == "ERROR" or line.startswith(("+CME ERROR:", "+CMS ERROR:")):
                raise RuntimeError(f"{label}: module rejected command; not retried")
            lines.append(line)
    raise TimeoutError(f"{label}: result unknown; not retried")


def parse_config(lines):
    values = [line.split(",")[1:] for line in lines if line.lower().startswith('+qcfg: "usbcfg",')]
    if len(values) != 1 or len(values[0]) != 9:
        raise ValueError("unexpected USB configuration format")
    fields = [value.strip() for value in values[0]]
    result = tuple(int(value, 16 if index < 2 else 10) for index, value in enumerate(fields))
    if result[:2] != EXPECTED[:2] or any(value not in (0, 1) for value in result[2:]):
        raise ValueError("unexpected USB identity or function flags")
    return result


def config_command(values):
    return f'AT+QCFG="usbcfg",0x{values[0]:04X},0x{values[1]:04X},' + ",".join(map(str, values[2:]))


def derive_key(challenge):
    if not re.fullmatch(r"[0-9]{8}", challenge):
        raise ValueError("unsupported QADBKEY challenge format")
    # Python 3.11 in the Debian bookworm container exposes system libcrypt.
    import crypt
    hashed = crypt.crypt("SH_adb_quectel", f"$1${challenge}$")
    prefix = f"$1${challenge}$"
    if not hashed or not hashed.startswith(prefix) or len(hashed[len(prefix):]) != 22:
        raise RuntimeError("libcrypt MD5-crypt unavailable")
    return hashed[len(prefix):len(prefix) + 16]


def initialize(dev, usb_path, apply=False, backup=None):
    dev.flush()
    model = exchange(dev, "AT+CGMM", "identify")
    revision = exchange(dev, "AT+CGMR", "firmware")
    if model != ["QDC507"] or revision != ["QDC507GLEFM21"]:
        raise RuntimeError("module model/firmware differs from validated target")
    current = parse_config(exchange(dev, 'AT+QCFG="usbcfg"', "read USB configuration"))
    target = current[:7] + (1, 1)
    if current == target:
        print(json.dumps({"status": "already_enabled", "usb_path": usb_path}), flush=True)
        return
    if current != EXPECTED:
        raise RuntimeError("USB configuration differs from captured baseline; inspect manually")
    key_lines = exchange(dev, "AT+QADBKEY?", "query ADB challenge")
    challenges = [line.split(":", 1)[1].strip().strip('"') for line in key_lines if line.startswith("+QADBKEY:")]
    if len(challenges) != 1:
        raise RuntimeError("expected one ADB challenge; inspect firmware before writing")
    key = derive_key(challenges[0])
    record = {"usb_path": usb_path, "model": model[0], "revision": revision[0],
              "original": config_command(current), "target": config_command(target)}
    if not apply:
        print(json.dumps({"status": "checked_only", **record}), flush=True)
        return
    if not backup:
        raise ValueError("--apply requires a new backup file")
    # Refuse overwrite: reruns must inspect the actual state before any retry.
    fd = os.open(backup, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "w") as out:
        json.dump(record, out)
        out.write("\n")
        out.flush()
        os.fsync(out.fileno())
    exchange(dev, f'AT+QADBKEY="{key}"', "unlock ADB")
    print(json.dumps({"status": "adb_unlocked"}), flush=True)
    exchange(dev, config_command(target), "enable ADB/UAC")
    verified = parse_config(exchange(dev, 'AT+QCFG="usbcfg"', "verify USB configuration"))
    if verified != target:
        raise RuntimeError("configuration readback mismatch; no reboot sent")
    print(json.dumps({"status": "configuration_verified", "target": config_command(target)}), flush=True)
    try:
        exchange(dev, "AT+CFUN=1,1", "reboot module")
        print(json.dumps({"status": "reboot_acknowledged"}), flush=True)
    except (OSError, TimeoutError) as error:
        if isinstance(error, OSError) and not isinstance(error, TimeoutError) and error.errno not in (errno.ENODEV, errno.ENXIO, errno.ESHUTDOWN):
            raise
        print(json.dumps({"status": "reboot_response_lost_verify_reenumeration"}), flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--usb-path", required=True, type=valid_usb_path)
    parser.add_argument("--apply", action="store_true")
    parser.add_argument("--backup")
    args = parser.parse_args()
    dev = USBAT(args.usb_path)
    try:
        initialize(dev, args.usb_path, args.apply, args.backup)
    finally:
        try:
            dev.close()
        except OSError:
            pass  # The module may have re-enumerated after a confirmed write.


if __name__ == "__main__":
    main()
