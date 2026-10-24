<div align="center">

# CBD

**把 QDC507 4G 模块和一张 SIM 卡，变成 NAS 上 7×24 在线的 iPhone 副号网关：
VoLTE 通话、短信收发、锁屏来电。**

[![Go][go-shield]][go-url]
[![Docker][docker-shield]][docker-url]
[![License][license-shield]][license-url]

[简体中文](README.md) · [English](README.en.md)

[报告问题](https://github.com/FanBB2333/CBD/issues/new?labels=bug) ·
[功能建议](https://github.com/FanBB2333/CBD/issues/new?labels=enhancement)

</div>

> [!IMPORTANT]
> CBD 目前只在一台 QNAP NAS 上完成了实机验收，还有若干场景未验收（见[限制](#限制)），
> 暂不建议用来承接主号。模块侧语音运行时是厂商闭源二进制，不随本仓库分发。

## 目录

1. [关于项目](#关于项目)
2. [快速开始](#快速开始)
3. [用法](#用法)
4. [文档](#文档)
5. [开发](#开发)
6. [限制](#限制)
7. [贡献](#贡献)
8. [许可证](#许可证)
9. [致谢](#致谢)

## 关于项目

CBD（CellBridge-Docker）派生自 [CellBridge](https://github.com/mccding/CellBridge)。
SIM 卡插在 QDC507 模块里，模块通过 USB 接到 NAS；NAS 上的网关容器充当 SIP 服务器，
iPhone 上的 YakPhone 通过 Tailscale 注册后，就能用这张卡打电话、收发短信。

```mermaid
flowchart LR
    Y[iPhone · YakPhone] <-->|SIP/RTP over Tailscale| G[NAS · 网关容器]
    G -->|USB：AT、ADB| Q[QDC507 + SIM]
    Q <-->|USB Audio：8 kHz 单声道| G
    Q <-->|VoLTE| N[蜂窝网络]
```

### 能做什么

- **打电话、接电话**：YakPhone 拨号经模块走 VoLTE；来电转到手机振铃，锁屏时由 PushKit 唤醒。
- **收发短信**：支持中文、长短信和运营商短号；手机离线时短信存在 NAS，重新连上后补收。
- **保留记录**：通话记录和短信存入 SQLite。
- **放在 NAS 上常驻**：不需要一台一直开着的 Mac，也不需要在宿主上装串口驱动。

### 与 CellBridge 的区别

| | CellBridge | CBD |
|---|---|---|
| 部署 | fnOS 裸机 + systemd | Docker 容器，非特权（`cap_drop: ALL`） |
| AT 串口 | 需要宿主提供 `ttyUSB` 驱动 | 容器内 libusb→PTY 桥接，宿主无需驱动 |
| 模块初始化 | 脚本会修改 VID/PID | 默认只读；只开启 ADB/UAC，先备份原值 |
| SIP 鉴权 | 占位实现（只检查是否带 `Authorization` 头） | 完整 Digest 鉴权，防重放 |
| 呼叫控制 | 不处理 ACK；振铃时取消会被阻塞 | 完整 INVITE 事务；取消立即生效；挂断经 CLCC 确认 |
| 代理与 NAT | RTP 固定发往 SDP 端口 | 学习实际 RTP 端口，手机开着 Surge 等代理也能通话 |
| 上行延迟 | 播放缓冲约 1 秒 | 80 ms 环形缓冲，声卡侧实测 21–41 ms |
| 连续通话 | 取消后的下一通可能无声 | 每通电话重建音频路由 |
| 短信 | SIP 重传可能导致重复发送 | 持久化去重；长短信可断点恢复；离线补收 |
| 锁屏来电 | token-only 推送格式 | YakPhone Pro Token 回调；手机已振铃时不重复推送 |

### 技术栈

- [Go](https://go.dev/) 1.25：网关、SIP/RTP、短信引擎
- [PyUSB](https://github.com/pyusb/pyusb) + [libusb](https://libusb.info/)：USB AT 桥接
- [SQLite](https://www.sqlite.org/)（[modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite)）：通话与短信存储
- [Docker](https://www.docker.com/) Compose 和 [Tailscale](https://tailscale.com/)

## 快速开始

你需要：

- QDC507 模块（DJI 一代 4G 模块，USB ID `2ca3:4006` 或 `2c7c:0125`）
- 已开通 VoLTE 的 SIM 卡
- 带 USB 口、能跑 Docker 的 Linux NAS（已验证 QNAP QTS 5.2，x86_64）
- NAS 和 iPhone 在同一个 Tailscale tailnet
- iPhone 上的 [YakPhone](https://apps.apple.com/cn/app/yak-ai-sip-%E7%94%B5%E8%AF%9D/id6763033863)；锁屏来电需要 Pro

以下命令以 QNAP 为例，在 NAS 上执行。QNAP 的 `docker` 位于 Container Station 目录：

```sh
cs_path=$(getcfg container-station Install_Path -f /etc/config/qpkg.conf)
export PATH="$cs_path/bin:$PATH"
git clone https://github.com/FanBB2333/CBD.git && cd CBD
```

1. **检查宿主**（只读）：确认 Docker、模块的 USB 设备和 USB 声卡都已识别。

   ```sh
   sh scripts/qnap-preflight.sh
   ```

2. **初始化模块**：开启模块的 ADB 和 USB 音频（UAC）。
   `gateway/tools/init_usb.py` 默认只读，只有显式传入 `--apply --backup <文件>`
   才会写入并重启模块。具体步骤和回退方法见[部署指南](docs/deployment/qnap.md)。

3. **下载模块语音运行时**：文件按固定 SHA-256 校验，不打进镜像。

   ```sh
   mkdir -p /share/Container/cbd/data
   python3 scripts/fetch-voice-runtime.py --dest /share/Container/cbd/module-voice
   ```

4. **构建镜像**：国内网络可通过 `GO_IMAGE`、`GOPROXY`、`DEBIAN_MIRROR` 等 build-arg 换源，
   示例见 [`.env.qnap.example`](infra/docker/.env.qnap.example)。

   ```sh
   docker build --platform linux/amd64 -f gateway/Dockerfile -t cellbridge-gateway:qnap .
   ```

5. **编写配置**：从 [`config.qnap.example.yaml`](infra/config.qnap.example.yaml) 复制，
   修改以下字段：

   ```sh
   cp infra/config.qnap.example.yaml /share/Container/cbd/config.yaml
   ```

   ```yaml
   modem:
     usb_path: 1-2                  # 模块所在的物理 USB 端口
   voice:
     enabled: true                  # 模块初始化通过后再开启
     bootstrap: true
     rx_path: hw:CARD=Baiwang,DEV=0
     tx_path: hw:CARD=Baiwang,DEV=0
   sip:
     enabled: true
     listen: 100.64.0.10:5060       # 示例，换成 NAS 的 Tailscale 地址
     users:
       - username: iphone
         password: <随机密码>
   ```

6. **启动**：

   ```sh
   cp infra/docker/.env.qnap.example infra/docker/.env   # 填写 NAS 上的绝对路径
   docker compose --env-file infra/docker/.env -f infra/docker/docker-compose.qnap.yml config --quiet
   docker compose --env-file infra/docker/.env -f infra/docker/docker-compose.qnap.yml up -d --no-build
   curl -fsS http://127.0.0.1:8787/api/v1/health
   ```

   健康接口只说明进程在运行；模块和语音是否就绪，以手机实际注册和通话为准。

## 用法

### 连接 YakPhone

| 设置 | 值 |
|---|---|
| 服务器 | `100.64.0.10:5060`（示例，换成 NAS 的 Tailscale 地址或 MagicDNS 名） |
| 传输 | UDP |
| 用户名 / 密码 | `sip.users` 中的配置 |
| 编解码 | PCMU 优先 |
| STUN / TURN | 关闭 |

注册成功后即可拨号。发短信时，在 YakPhone 的消息界面输入完整号码。

### 开启锁屏来电

需要 YakPhone Pro。

1. 在配置中设置 `sip.push_callback_listen: 127.0.0.1:8788` 并重启容器。
   网关会生成 `data/yakphone-push.json`（权限 0600），其中包含 `callback_key`。
2. 用 Tailscale Serve 把 HTTPS 8443 转到回调端口（仅 tailnet 内可访问，不要开 Funnel）：

   ```sh
   tailscale serve https:8443 / http://127.0.0.1:8788   # Tailscale 1.40 语法
   ```

3. 在 YakPhone 里把 Token 回调 URL 填为
   `https://qnap-nas.example.ts.net:8443/token/<callback_key>`（主机名换成 NAS 的 ts.net 名称）。

如果手机已经通过 SIP 振铃，网关就不再发推送，避免同一通来电响两次。

### 开启短信

首次部署默认不收件，发送也只是 dry-run。确认无误后：

- 在配置中设置 `sms.inbox_enabled: true`；
- 在 `.env` 中设置 `CELLBRIDGE_SMS_DRY_RUN=false`。

关闭 dry-run 后，发送会真实提交给运营商并计费。

### 其他 Linux 主机

如果宿主已经有 `ttyUSB` 串口驱动（例如 fnOS），可以设置 `modem.transport: serial`，
然后按[上游教程](docs/CellBridge.md)部署。

## 文档

- [QNAP 部署指南与实测记录](docs/deployment/qnap.md)：模块初始化、容器权限、各项修复与验收结果
- [CellBridge 完整教程](docs/CellBridge.md)：上游的串口部署、八层排查方法和踩坑表
- [运维与管理](docs/operations/admin.md)
- [架构决策记录](docs/architecture/)
- [第三方声明](ThirdParty/ThirdPartyNotices.md)

## 开发

```sh
cd gateway
go test ./...
go test -race ./internal/config ./internal/modem/... ./internal/sip ./internal/voice/...
go vet ./...
cd ..
python3 -m unittest discover -s gateway/tools -p 'test_*.py'
```

自动化测试使用模拟的模块、AT 和 UDP，不会拨打电话，也不会发送短信。

## 限制

- 只支持 QDC507；一张 SIM，同一时间只能有一通电话。
- 已在 QNAP 上实测通过：前台注册、呼入和呼出双向通话、响铃时取消、锁屏推送接听、短信接收、容器重启恢复。
- 尚未验收：QNAP 上的短信真实发送、超过 60 秒的长通话、连续多通重拨、USB 热插拔、
  模块或 NAS 重启后的自动恢复、多日 Token 轮换。
- 上游的串口传输和 arm64 平台在本项目改动后没有做过真机回归测试。
- 语音为 PCMU/8000，没有启用 TLS/SRTP，传输加密依赖 Tailscale。
- 锁屏来电依赖 YakPhone Pro。Linphone 等其他 SIP 客户端可以在前台通话，但没有推送。
- 模块语音运行时下载自 [mavo](https://github.com/moluncn/mavo)，能否获取取决于该仓库。

## 贡献

欢迎提交问题报告、测试用例、文档修正和补丁。

1. Fork 本仓库并创建分支。
2. 为改动的行为补充或更新测试。
3. 运行[开发](#开发)中的测试。
4. 按 [Conventional Commits](https://www.conventionalcommits.org/) 撰写提交信息，然后发起 Pull Request。

报告通话问题时请附上网关日志、NAS 型号与系统版本。
**请先删除手机号、短信正文、SIP 密码和 Tailscale 地址。**

## 许可证

基于 MIT 许可证发布，详见 [LICENSE](LICENSE)。
模块侧语音运行时不在本许可证范围内。

## 致谢

感谢以下项目和作者：

- [CellBridge](https://github.com/mccding/CellBridge)：本项目的上游，提供了
  QDC507 + NAS + YakPhone 的整体架构、网关代码和完整复刻教程。没有它就没有 CBD。
- [dontmove-kit](https://github.com/SheldonJoO/dontmove-kit)：SIP 呼叫处理、事件分发、CLCC 来电判定、
  USB AT 接口选择和短信状态持久化改编自该项目（MIT）。
- [mavo](https://github.com/moluncn/mavo)：提供模块侧语音运行时。
- [DJOneHub](https://github.com/ZenGeekLabs/DJOneHub)、[dji-voice-go](https://github.com/iniwex5/dji-voice-go)、
  [iPhone_air_esim_tutorial](https://github.com/AmorXxx/iPhone_air_esim_tutorial)：
  模块 USB 通道、ADB 解锁和 UAC 语音桥接方面的参考。
- [pion](https://github.com/pion)、[gorilla/websocket](https://github.com/gorilla/websocket)、
  [modernc.org/sqlite](https://gitlab.com/cznic/sqlite)、[google/uuid](https://github.com/google/uuid)、
  [zeroconf](https://github.com/grandcat/zeroconf)、[yaml.v3](https://github.com/go-yaml/yaml)、
  [PyUSB](https://github.com/pyusb/pyusb)、[libusb](https://libusb.info/)：网关依赖的开源库。
- [Tailscale](https://tailscale.com/) 和 [YakPhone](https://apps.apple.com/cn/app/yak-ai-sip-%E7%94%B5%E8%AF%9D/id6763033863)：组网与手机客户端。
- README 结构参考 [Best-README-Template](https://github.com/othneildrew/Best-README-Template)。

[go-shield]: https://img.shields.io/badge/Go-1.25-00ADD8?style=for-the-badge&logo=go&logoColor=white
[go-url]: https://go.dev/
[docker-shield]: https://img.shields.io/badge/Docker-Compose-2496ED?style=for-the-badge&logo=docker&logoColor=white
[docker-url]: infra/docker/docker-compose.qnap.yml
[license-shield]: https://img.shields.io/badge/License-MIT-yellow?style=for-the-badge
[license-url]: LICENSE
