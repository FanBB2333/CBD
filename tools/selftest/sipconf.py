"""Settings and a minimal SIP UDP client for the self-test scripts.

Settings come from environment variables first, then from an optional,
git-ignored `.env` file next to this module (copy `.env.example`).
"""
import hashlib
import os
import re
import secrets
import socket
import time
from pathlib import Path


def _load_env_file(path):
    if not path.is_file():
        return
    for line in path.read_text().splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        os.environ.setdefault(key.strip(), value.strip().strip("\"'"))


def _host_port(value):
    host, sep, port = value.rpartition(":")
    return (host, int(port)) if sep else (value, 5060)


_load_env_file(Path(__file__).with_name(".env"))

SERVER = _host_port(os.environ.get("SIP_SERVER", "127.0.0.1:5060"))
DOMAIN = os.environ.get("SIP_DOMAIN", "cellbridge-nas.example.ts.net")
USER = os.environ.get("SIP_USER", "1001")
PASSWORD = os.environ.get("SIP_PASS", "")


def header(message, name):
    match = re.search(rf"^{name}:\s*(.+?)\r?$", message, re.MULTILINE | re.IGNORECASE)
    return match.group(1) if match else ""


def status(message):
    return int(message.split(" ", 2)[1])


def first_line(message):
    return message.split("\r\n", 1)[0]


def _md5(text):
    return hashlib.md5(text.encode()).hexdigest()


def authorization(challenge, method, uri):
    """Answer a WWW-Authenticate Digest challenge, using qop=auth when offered."""
    fields = dict(re.findall(r'(\w+)="?([^",]+)"?', challenge))
    realm, nonce = fields["realm"], fields["nonce"]
    ha1 = _md5(f"{USER}:{realm}:{PASSWORD}")
    ha2 = _md5(f"{method}:{uri}")
    value = f'Digest username="{USER}", realm="{realm}", nonce="{nonce}", uri="{uri}", algorithm=MD5'
    if fields.get("qop") == "auth":
        cnonce, nc = secrets.token_hex(8), "00000001"
        response = _md5(f"{ha1}:{nonce}:{nc}:{cnonce}:auth:{ha2}")
        value += f', qop=auth, nc={nc}, cnonce="{cnonce}"'
    else:
        response = _md5(f"{ha1}:{nonce}:{ha2}")
    return f'Authorization: {value}, response="{response}"'


class Client:
    def __init__(self, local_port, timeout=4):
        probe = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        probe.connect(SERVER)
        address = probe.getsockname()[0]
        probe.close()
        self.sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        self.sock.settimeout(timeout)
        self.sock.bind((address, local_port))
        self.address = address
        self.local = f"{address}:{local_port}"
        self.tag = secrets.token_hex(4)
        self.dialog = None

    def message(self, method, uri, to, call_id, cseq, branch, extra=(), body=""):
        lines = [
            f"{method} {uri} SIP/2.0",
            f"Via: SIP/2.0/UDP {self.local};branch={branch};rport",
            f"From: <sip:{USER}@{DOMAIN}>;tag={self.tag}",
            f"To: {to}",
            f"Call-ID: {call_id}",
            f"CSeq: {cseq} {method}",
            *extra,
        ]
        if body:
            lines.append("Content-Type: application/sdp")
        lines.append(f"Content-Length: {len(body.encode())}")
        return "\r\n".join(lines) + "\r\n\r\n" + body

    def send(self, message):
        self.sock.sendto(message.encode(), SERVER)

    def final(self, method, timeout):
        """Wait for the final response to method, printing provisional ones."""
        deadline = time.time() + timeout
        while time.time() < deadline:
            try:
                message = self.sock.recvfrom(65535)[0].decode(errors="replace")
            except socket.timeout:
                continue
            if not message.startswith("SIP/2.0") or not header(message, "CSeq").endswith(method):
                continue
            print("GOT:", first_line(message))
            if status(message) >= 200:
                return message
        return None

    def request(self, method, uri, to, call_id, cseq=1, extra=(), body="", timeout=8):
        """Send a request, answering one Digest challenge. Returns (response, cseq)."""
        branch = "z9hG4bK" + secrets.token_hex(6)
        self.send(self.message(method, uri, to, call_id, cseq, branch, extra, body))
        response = self.final(method, timeout)
        if response and status(response) == 401 and PASSWORD:
            if method == "INVITE":
                self.send(self.message("ACK", uri, header(response, "To"), call_id, cseq, branch))
            auth = authorization(header(response, "WWW-Authenticate"), method, uri)
            cseq += 1
            branch = "z9hG4bK" + secrets.token_hex(6)
            self.send(self.message(method, uri, to, call_id, cseq, branch, (*extra, auth), body))
            response = self.final(method, timeout)
        return response, cseq

    def register(self):
        """Authenticated REGISTER without Contact: queries bindings, leaves the phone's intact."""
        to = f"<sip:{USER}@{DOMAIN}>"
        response, _ = self.request("REGISTER", f"sip:{DOMAIN}", to, f"reg-{secrets.token_hex(6)}")
        return response

    def invite(self, dest, timeout=60):
        uri = f"sip:{dest}@{DOMAIN}"
        call_id = f"self-{secrets.token_hex(6)}"
        sdp = (
            f"v=0\r\no=self 0 0 IN IP4 {self.address}\r\ns=self\r\nc=IN IP4 {self.address}\r\n"
            "t=0 0\r\nm=audio 40082 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n"
        )
        contact = (f"Contact: <sip:{USER}@{self.local}>",)
        response, cseq = self.request("INVITE", uri, f"<{uri}>", call_id, extra=contact, body=sdp, timeout=timeout)
        if response and status(response) == 200:
            to = header(response, "To")
            target = re.search(r"<([^>]+)>", header(response, "Contact"))
            target = target.group(1) if target else uri
            self.send(self.message("ACK", target, to, call_id, cseq, "z9hG4bK" + secrets.token_hex(6)))
            self.dialog = (target, to, call_id, cseq)
        return response

    def bye(self):
        target, to, call_id, cseq = self.dialog
        self.dialog = None
        response, _ = self.request("BYE", target, to, call_id, cseq + 1)
        return response
