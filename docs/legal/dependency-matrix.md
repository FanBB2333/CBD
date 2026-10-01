# 依赖与许可证矩阵

本项目包含 Gateway、协议模型和客户端基础层。CBD 的语音接入移植了 dontmove-kit 的部分 MIT 代码；模块的预编译 runtime/helper 仍由本地下载并挂载，不打进镜像。

| Name | Source URL | Version/commit | License | Copied code? | Modified? | Notice required? | Commercially compatible? |
|---|---|---|---|---|---|---|---|
| dontmove-kit | https://github.com/SheldonJoO/dontmove-kit | 666d22a45f3b8df29945132520aaafd922d395a7 | MIT | yes, selected Go code/tests and Python USB interface selection | yes, Linux/QNAP integration | retained in ThirdParty | subject to MIT terms |
| PyUSB | https://github.com/pyusb/pyusb | 1.2.1 (tested Debian 1.2.1-2) | BSD-3-Clause | no, container package | no | retained by Debian package and ThirdParty notices | subject to BSD terms |
| libusb | https://github.com/libusb/libusb | 1.0.26 (tested Debian 2:1.0.26-1) | LGPL-2.1-or-later | no, dynamic container dependency | no | retain license and corresponding source availability | subject to LGPL terms |
| Python | https://www.python.org/ | 3.11.2 (tested Debian 3.11.2-1+b1) | PSF and bundled component notices | no, container package | no | retained by Debian package | subject to component terms |
| MaVo module voice artifacts | https://github.com/moluncn/mavo | 0443dfdaf8aec086fd76ba2ee9152fd908114524 | binary terms not established here | no, local download only | no | review before redistribution | not established |
| MacCellular | https://github.com/yuexiazhuojiu-byte/MacCellular | review before use | PolyForm Noncommercial 1.0.0（按计划记录） | no | no | review | no, without legal review |
| NasAnySim | https://github.com/mccding/NasAnySim | review before use | review repository assets separately | no | no | review | unknown |
| dji-4g-vohive-mac | https://github.com/wlzh/dji-4g-vohive-mac | review before use | review repository and upstream assets separately | no | no | review | unknown |
| Go standard library | https://go.dev/ | toolchain | BSD-style | no | no | no | yes |
| modernc.org/sqlite | https://gitlab.com/cznic/sqlite | v1.57.0 | BSD-3-Clause | no | no | retain license | yes, subject to license |
| Swift / SwiftUI / SwiftData | https://developer.apple.com/ | iOS 17 SDK | Apple SDK terms | no | no | Apple terms | subject to Apple terms |
| WebRTC Native XCFramework | https://webrtc.googlesource.com/src/ | add only after audit | audit at adoption time | no | no | yes | audit required |
| Pion WebRTC | https://github.com/pion/webrtc | add only after audit | audit at adoption time | no | no | yes | audit required |
| coturn | https://github.com/coturn/coturn | deployment dependency | BSD-style | no | no | retain license | yes, subject to license |
| Caddy | https://github.com/caddyserver/caddy | deployment dependency | Apache-2.0 | no | no | retain license | yes |
| gopkg.in/yaml.v3 | https://github.com/go-yaml/yaml | v3.0.1 | MIT | no | no | retain license | yes |
| Pion WebRTC | https://github.com/pion/webrtc | v4.2.19 | MIT | no | no | retain license | yes |

任何新增依赖都必须补齐版本、许可证、notice 和商业兼容性结论后才能进入 release 镜像或 App。
