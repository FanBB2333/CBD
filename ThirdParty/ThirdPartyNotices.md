# Third-party notices

## Exyte/Chat

CellBridge uses Exyte/Chat under the MIT License.

- Source: https://github.com/exyte/Chat
- Vendored source: `third_party/Chat`
- Upstream revision: `e3966b35fe4a8a28f98cbda027da4371ccf5ebe6`

The vendored source contains small Xcode 27 compatibility changes in `ChatView.swift` and keeps the upstream license and notices with the source tree.

## SheldonJoO/dontmove-kit

Selected SIP call handling, modem event fan-out, directional CLCC detection and
regression tests are adapted from dontmove-kit under the MIT License.

- Source: https://github.com/SheldonJoO/dontmove-kit
- Revision: `666d22a45f3b8df29945132520aaafd922d395a7`
- Copyright (c) 2026 zxd
- License: [dontmove-kit.LICENSE](dontmove-kit.LICENSE)
- Adapted paths: `gateway/internal/sip/{server,session}.go`, associated new SIP
  tests, `gateway/internal/modem/{call_adapter,fanout}.go`, fan-out tests, and
  the CLCC wait helper/tests in `gateway/internal/modem/at`.
- USB AT interface selection in `gateway/tools/usb_at.py` is adapted from
  the reference project's `at_pty_bridge.py`; PTY lifecycle, physical-port
  pinning, read-only probing and Go supervision are integrated for Linux/QNAP.
- SMS submission status persistence and its deadline regression test in
  `gateway/internal/sms/{engine,engine_deadline_test}.go` are adapted from
  the reference project's gateway patches. SMS mode selection and short-code
  PDU fixes also follow its documented diagnosis.
- Linux-specific integration: API/SIP event subscribers in the gateway entrypoint,
  per-call QDC507 route invalidation, isolated ADB environment, QNAP Compose setup.
  The macOS Swift audio helper and Linphone push service are not included.

`scripts/fetch-voice-runtime.py` references the pinned artifact manifest from
`module-tools/voice_runtime.py` in that revision. The three module binaries come
from `moluncn/mavo@0443dfdaf8aec086fd76ba2ee9152fd908114524`, under
`Resources/ModuleVoice`. They are downloaded only for local runtime use and are
not bundled in source or Docker images. The MIT grant above covers dontmove-kit
code; it does not establish redistribution rights for those separate binaries.

## Container USB dependencies

The USB-to-PTY transport uses Debian's Python 3, PyUSB and dynamically loaded
libusb packages. Package copyright/license files remain under `/usr/share/doc`
in the image; no dependency source is vendored here.

- PyUSB 1.2.1 (tested Debian package `1.2.1-2`): BSD-3-Clause,
  https://github.com/pyusb/pyusb ; copyright Wander Lairson Costa and PyUSB contributors.
- libusb 1.0.26 (tested Debian package `2:1.0.26-1`): LGPL-2.1-or-later,
  https://github.com/libusb/libusb . Preserve the applicable notices and source
  availability obligations when distributing images.
- Python 3.11 (tested Debian package `3.11.2-1+b1`): PSF license and bundled
  component notices, https://www.python.org/ .
