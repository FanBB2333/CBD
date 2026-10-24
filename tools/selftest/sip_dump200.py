#!/usr/bin/env python3
"""
Print the gateway's full 200 OK to an INVITE (Contact/Allow/SDP), then hang up.

Usage: SIP_PASS=yourpass python3 sip_dump200.py [dest]   (settings: see sipconf.py)
Real call, use at your own cost.
"""
import sys

from sipconf import PASSWORD, Client, first_line, status

DEST = sys.argv[1] if len(sys.argv) > 1 else "10010"

if __name__ == "__main__":
    if not PASSWORD:
        print("Set SIP_PASS env var"); sys.exit(2)
    client = Client(5090)
    response = client.register()
    print("REGISTER:", first_line(response) if response else "TIMEOUT")
    response = client.invite(DEST, timeout=25)
    if response is None or status(response) != 200:
        print("CALL: FAIL (no 200)"); sys.exit(1)
    print("=== FULL 200 OK ===")
    print(response)
    client.bye()
