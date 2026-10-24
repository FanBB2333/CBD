#!/usr/bin/env python3
"""
Call, then hang up as soon as the gateway answers; checks that BYE gets 200.

Usage: SIP_PASS=yourpass python3 sip_bye2.py [dest]   (settings: see sipconf.py)
Real call, use at your own cost.
"""
import sys

from sipconf import PASSWORD, Client, first_line, status

DEST = sys.argv[1] if len(sys.argv) > 1 else "10010"

if __name__ == "__main__":
    if not PASSWORD:
        print("Set SIP_PASS env var"); sys.exit(2)
    client = Client(5090)
    response = client.invite(DEST)
    if response is None or status(response) != 200:
        print("CALL: FAIL (no 200)"); sys.exit(1)
    response = client.bye()
    print("BYE:", first_line(response) if response else "TIMEOUT")
