#!/usr/bin/env python3
"""Fetch the exact MaVo files used by CBD and dontmove-kit; never touch a modem."""
import argparse
import hashlib
import os
from pathlib import Path
import stat
import tempfile
import urllib.request

# Source: dontmove-kit@666d22a45f3b8df29945132520aaafd922d395a7,
# module-tools/voice_runtime.py. Copyright (c) 2026 zxd, MIT.
# Runtime binaries retain their upstream terms and are not redistributed here.
REVISION = "0443dfdaf8aec086fd76ba2ee9152fd908114524"
BASE_URL = f"https://raw.githubusercontent.com/moluncn/mavo/{REVISION}/Resources/ModuleVoice/"
FILES = {
    "qdc507_aprv3.ko": ("3d82d3dec4f1e323201bba87156df9d41438e08314097353f2607f9117211d4a", 0o644),
    "qdc507_voice.ko": ("ed3821682d5309969a01c764192c83feff9669c61ef237c69475cd1619cf296c", 0o644),
    "mavo-pcm-bridge.armv7": ("88d47c15e61d1428a59c821fed804c2e6490e82859a085062f21966b58d167fc", 0o755),
}


def validate_directory(destination):
    info = destination.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_mode & 0o022:
        raise ValueError("runtime destination must be a real directory without group/other write permission")


def verify(destination):
    validate_directory(destination)
    for name, (expected, _) in FILES.items():
        path = destination / name
        info = path.lstat()
        if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o022:
            raise ValueError(f"{name}: must be a regular file without group/other write permission")
        if hashlib.sha256(path.read_bytes()).hexdigest() != expected:
            raise ValueError(f"{name}: SHA-256 mismatch")


def download(url):
    with urllib.request.urlopen(url, timeout=45) as response:
        return response.read()


def fetch(destination, downloader=download):
    destination.mkdir(mode=0o750, parents=True, exist_ok=True)
    validate_directory(destination)
    # Validate the entire download before replacing any previously installed file.
    with tempfile.TemporaryDirectory(prefix=".download-", dir=destination) as staging:
        for name, (expected, mode) in FILES.items():
            existing = destination / name
            if existing.is_symlink():
                raise ValueError(f"{name}: refusing to replace a symlink")
            if existing.exists() and not existing.is_file():
                raise ValueError(f"{name}: not a regular file")
            data = existing.read_bytes() if existing.exists() else b""
            if hashlib.sha256(data).hexdigest() != expected:
                data = downloader(BASE_URL + name)
            if hashlib.sha256(data).hexdigest() != expected:
                raise ValueError(f"{name}: download SHA-256 mismatch; installed files untouched")
            target = Path(staging) / name
            target.write_bytes(data)
            target.chmod(mode)
        for name in FILES:
            os.replace(Path(staging) / name, destination / name)
    verify(destination)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dest", type=Path, default=Path("artifacts/module-voice"))
    parser.add_argument("--check", action="store_true", help="verify local files without network access")
    args = parser.parse_args()
    try:
        if args.check:
            verify(args.dest)
        else:
            fetch(args.dest)
    except (OSError, ValueError) as error:
        parser.exit(1, f"voice runtime: {error}\n")
    print(f"Verified {len(FILES)} runtime files from moluncn/mavo@{REVISION} in {args.dest}")


if __name__ == "__main__":
    main()
