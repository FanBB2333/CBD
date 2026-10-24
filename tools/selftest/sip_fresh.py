#!/usr/bin/env python3
"""
SIP full-chain self-test for the CellBridge gateway.

Usage:
  cp .env.example .env    # once: fill in SIP_SERVER, SIP_DOMAIN, SIP_USER
  SIP_PASS=yourpass python3 sip_fresh.py [dest] [hold_seconds]

Flow: REGISTER (digest) -> INVITE (digest, fresh Call-ID) -> 200 OK -> ACK -> BYE.
Dest defaults to 10010 (free operator hotline). Real call, use at your own cost.
"""
import sys
import time

from sipconf import PASSWORD, Client, first_line, status

DEST = sys.argv[1] if len(sys.argv) > 1 else "10010"
HOLD = float(sys.argv[2]) if len(sys.argv) > 2 else 6.0

if __name__ == "__main__":
    if not PASSWORD:
        print("Set SIP_PASS env var"); sys.exit(2)
    client = Client(5092)
    response = client.register()
    print("REGISTER:", first_line(response) if response else "TIMEOUT")
    response = client.invite(DEST)
    ok = response is not None and status(response) == 200
    if ok:
        time.sleep(HOLD)
        client.bye()
    print("CALL:", "OK" if ok else "FAIL (no 200)")
