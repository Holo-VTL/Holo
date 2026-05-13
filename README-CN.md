# Holo

**全息磁带库**

<div align="center">

<img src="./banner.png" alt="Holo" width="100%">

*芥子纳须弥——每一个碎片都蕴含完整的图景。*

[English](README.md) | 简体中文

[![License: MIT](https://img.shields.io/badge/License-MIT-00FF88?style=flat-square)](LICENSE)
[![Rust](https://img.shields.io/badge/Rust-1.78+-000000?style=flat-square&logo=rust&logoColor=white)](https://www.rust-lang.org/)
[![Go](https://img.shields.io/badge/Go-1.24-00ADD8?style=flat-square&logo=go&logoColor=white)](https://go.dev/)
[![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen?style=flat-square)](https://makeapullrequest.com)

![](https://img.shields.io/badge/48_驱动配置-7B61FF?style=for-the-badge)
![](https://img.shields.io/badge/50+_磁带库配置-00FF88?style=for-the-badge)
![](https://img.shields.io/badge/零_Unsafe-FF61DC?style=for-the-badge&logo=rust&logoColor=white)
![](https://img.shields.io/badge/Linux_%7C_iSCSI-FFA116?style=for-the-badge&logo=linux&logoColor=white)

<a href="#安装"><img src="https://img.shields.io/badge/立即安装-00FF88?style=for-the-badge&logo=rocket&logoColor=black" alt="Install Now"></a>&nbsp;
<a href="#快速开始"><img src="https://img.shields.io/badge/快速开始-7B61FF?style=for-the-badge&logo=terminal&logoColor=white" alt="Quick Start"></a>&nbsp;
<a href="#支持的设备"><img src="https://img.shields.io/badge/设备列表-FF61DC?style=for-the-badge&logo=list&logoColor=white" alt="Devices"></a>

</div>

---

## Holo 是什么？

> **Holo** /ˈhɒləʊ/ — 取自 Hologram（全息图）。芥子纳须弥，每一个碎片都蕴含完整的图景。

Holo 是一套开源虚拟磁带库，通过 iSCSI 模拟磁带驱动器和自动加载机。备份软件看到的是仿真硬件——无需安装代理或插件。

使用指南：

- [简体中文：Holo VTL 使用指南](https://blog.backupnext.cloud/2026/05/holo-VTL-launch/)
- [English: Holo VTL Launch Guide](https://blog.backupnext.cloud/en/2026/05/holo-vtl-launch/)

---

## 为什么选择 Holo？


|        | 特性             | 说明                                                                                          |
| ------ | -------------- | ------------------------------------------------------------------------------------------- |
| **全息** | **真实 SCSI 行为** | 完整 SSC-3 / SMC-3 / SPC-4 协议合规 — INQUIRY、MODE SENSE、LOG SENSE、PERSISTENT RESERVE、WORM、MAM 属性 |
| **性能** | **Rust 数据面**   | 零 `unsafe`，零拷贝 I/O，追加式段存储引擎，支持 LZ4/Zlib 压缩和数据去重                                             |
| **安全** | **崩溃安全存储**     | 原子写入（tmpfile → fsync → rename → dirsync）——断电不丢数据                                            |
| **广泛** | **48 种驱动器配置**  | IBM LTO-1 至 LTO-9、HP Ultrium、Quantum SuperLoader、Dell TL/ML、STK、Spectra、Overland            |
| **便捷** | **一键安装**       | 单条命令完成全部部署——数据面二进制、控制面 API、Web 控制台                                                          |
| **本机** | **本地挂载**       | 让 Holo 主机登录自己发布的 VTL target，Linux 备份软件可直接把这台机器作为 Tape Server 使用                           |
| **开放** | **MIT 许可**     | 完全开源。无厂商锁定。无隐藏遥测。                                                                           |


---

## 架构

```
 备份软件 (Veeam, NetBackup等)
        |
        | iSCSI（标准协议）
        v
 +------LIO Target（内核）--------+
 |                                 |
 |  TCMU（Target Core User-space） |
 |                                 |
 +---------------+-----------------+
                 |
                 | UNIX 套接字 (CDB 帧)
                 v
 +------ Holo 数据面 ------------+    +----- Holo 控制面 ------------+
 |  Rust                          |    |  Go                           |
 |  + SCSI 磁带状态机              |    |  + REST API（发布/管理）       |
 |  + CDB 分发（48 种操作码）       |    |  + 认证中间件                  |
 |  + 段存储引擎                   |    |  + Target 编排                 |
 |  + 去重 / 压缩                  |    |  + 审计日志 (JSONL)            |
 |  + WORM 强制执行                |    |                                |
 +--------------------------------+    +----- Holo Web 控制台 ---------+
                                              |  React + Vite                  |
                                              |  仪表盘 / 配置 / 监控           |
                                              +--------------------------------+
```

**三层架构，职责清晰：**


| 层级          | 语言    | 职责                         |
| ----------- | ----- | -------------------------- |
| **数据面**     | Rust  | SCSI 磁带仿真、段存储、崩溃恢复         |
| **控制面**     | Go    | REST API、Target 生命周期、认证、审计 |
| **Web 控制台** | React | 浏览器管理仪表盘                   |


---

## 安装

### 统一安装脚本

推荐使用发布包配合统一入口脚本 `install.sh` 安装。`install.sh` 负责在线下载或从本地 tarball 解包，发布包内部真正执行系统安装的是 `install-holo.sh`。

离线发布包内置 Holo 二进制、Web 控制台、TCMU handler，以及 EL 8 / EL 9 / EL 10 对应的 `tcmu-runner` / `libtcmu` RPM。正常离线安装不需要额外传 `--deps-dir`。

#### 在线安装（推荐）

运行以下命令自动下载最新版本并完成完整安装：

```bash
curl -fsSL https://raw.githubusercontent.com/Holo-VTL/Holo/main/scripts/install.sh | bash
```

#### 离线安装

1. 从 [Releases](https://github.com/Holo-VTL/Holo/releases) 页面下载最新的发布包 (`holo-vtl-<version>-linux-x86_64.tar.gz`)。
2. 将 `install.sh` 放到同一目录。
3. 使用 `--offline` 参数运行：

```bash
bash install.sh --offline
```

如果当前目录里有多个 `holo-vtl-*.tar.gz`，`install.sh --offline` 会选择修改时间最新的发布包。

### 进阶指令

安装脚本支持以下命令：

- `install`: (默认) 执行全新的安装。
- `upgrade`: 升级二进制文件和 Web 控制台，同时保留所有数据和配置。
- `uninstall`: 移除服务和应用程序文件。

在线升级：

```bash
curl -fsSL https://raw.githubusercontent.com/Holo-VTL/Holo/main/scripts/install.sh | bash -s -- upgrade
```

在线卸载：

```bash
curl -fsSL https://raw.githubusercontent.com/Holo-VTL/Holo/main/scripts/install.sh | bash -s -- uninstall
```

离线升级示例：

```bash
sudo bash install.sh --offline upgrade
```

### 支持平台


| 发行版                    | 支持版本                          | 包管理器   | 说明                                                 |
| ---------------------- | ----------------------------- | ------ | -------------------------------------------------- |
| **RHEL**               | 8 / 9 / 10                    | dnf    | 需要启用 Red Hat BaseOS、AppStream、CodeReady Builder 仓库 |
| **Rocky / Alma Linux** | 8 / 9 / 10                    | dnf    | 使用离线包内置 EL `tcmu-runner` 包                         |
| **Ubuntu**             | 20.04 LTS / 24.04 LTS / 25.04 | apt    | 使用发行版运行包                                           |
| **Debian**             | 12 / 13                       | apt    | 使用发行版运行包                                           |
| **openSUSE Leap**      | 15.6                          | zypper | 兼容 SLES 15 的包和运行时布局                                |


安装脚本会处理运行依赖、二进制安装、systemd 服务、LIO/TCMU 初始化和 Web 控制台部署。

### 运行时要求


| 依赖                      | 用途                                                      |
| ----------------------- | ------------------------------------------------------- |
| 带 LIO/TCMU 模块的 Linux 内核 | `target_core_mod`、`target_core_user`、`iscsi_target_mod` |
| `targetcli`             | LIO iSCSI 配置                                            |
| `open-iscsi` / `iscsi-initiator-utils` | 可选的本地发起端支持，用于“本地挂载”功能              |
| `tcmu-runner` 1.5+      | 用户态 SCSI 命令处理                                           |
| `xfsprogs`              | XFS 文件系统工具                                              |
| `kmod`、`sudo`           | 内核模块加载、权限提升                                             |


### 各发行版前置配置

安装脚本会自动安装运行依赖。主要前提是系统仓库能提供基础运行包。

**【推荐】Ubuntu / Debian** — 无需额外配置

`targetcli-fb`、`tcmu-runner` 从已配置的 apt 仓库安装。

```bash
sudo apt update && sudo apt install targetcli-fb tcmu-runner xfsprogs open-iscsi
```

**Rocky Linux 8 / 9 / 10** — 离线包内置 TCMU RPM

离线发布包已经包含 `packages/dnf/el8`、`packages/dnf/el9`、`packages/dnf/el10` 下的 `libtcmu` 和 `tcmu-runner` RPM。

安装脚本仍会从系统仓库安装 `targetcli`、`kmod`、`sudo`、`xfsprogs` 等基础包。EL 9 上 CRB 可能参与依赖解析，安装脚本会尝试自动启用。

**RHEL 8/9/10** — 注册订阅并启用官方仓库

RHEL 系统安装前需要有效的 Red Hat 订阅，并启用标准 BaseOS、AppStream 和 CodeReady Builder 仓库：

```bash
sudo subscription-manager register --auto-attach

# RHEL 8
sudo subscription-manager repos \
  --enable rhel-8-for-$(uname -m)-baseos-rpms \
  --enable rhel-8-for-$(uname -m)-appstream-rpms \
  --enable codeready-builder-for-rhel-8-$(uname -m)-rpms

# RHEL 9
sudo subscription-manager repos \
  --enable rhel-9-for-$(uname -m)-baseos-rpms \
  --enable rhel-9-for-$(uname -m)-appstream-rpms \
  --enable codeready-builder-for-rhel-9-$(uname -m)-rpms

# RHEL 10
sudo subscription-manager repos \
  --enable rhel-10-for-$(uname -m)-baseos-rpms \
  --enable rhel-10-for-$(uname -m)-appstream-rpms \
  --enable codeready-builder-for-rhel-10-$(uname -m)-rpms
```

离线发布包提供 EL 8/9/10 的 `tcmu-runner`，RHEL 仓库主要用于 `targetcli` 及其 Python 依赖。

**openSUSE Leap 15.6** — zypper 运行包

```bash
sudo zypper -n install kernel-default kmod sudo xfsprogs util-linux-systemd python3-targetcli-fb tcmu-runner open-iscsi
```

Holo 需要包含 LIO/TCMU 模块的完整内核包，不能使用缺少 `target_core_user` 的最小内核。

---

## 快速开始

安装后打开 Web 控制台：

`http://<server-name-or-ip>/ui/`

Holo Web 控制台

![System-overview](screenshots/System-overview.png)

存储管理

![Storage-Management](screenshots/Storage-Management.png)

虚拟带库视图

![VTL-Rack-view](screenshots/VTL-Rack-view.png)

### 本地挂载

在 **Targets** 页面启用 **Mount Locally / 本地挂载** 后，Holo 主机会登录自己发布出来的 VTL target。这个功能适合实验室验证，也适合 Veeam 等备份软件直接把这台 Holo Linux 主机作为 Tape Server 使用。

启用后，Holo 只会为当前需要挂载的 Holo IQN 创建本地 iSCSI node 记录，将其设置为开机自动登录，并登录对应的磁带驱动器和机械臂 target；同时会清理同一 portal 下已经过期的本地 Holo node。挂载成功后，Linux 上会出现标准磁带和介质更换器设备，例如 `/dev/st0`、`/dev/st1` 和 `/dev/sch0`。

---

## 支持的设备

### 磁带驱动器（48 种配置）


| 厂商           | 型号                                           | Windows 驱动  |
| ------------ | -------------------------------------------- | ----------- |
| **IBM**      | ULT3580-TD1 至 TD9 (LTO-1 至 LTO-9)、3592       | 有（IBM 磁带驱动） |
| **HP**       | Ultrium 1-SCSI 至 9-SCSI (LTO-1 至 LTO-9)      | 有（HP 磁带驱动）  |
| **Quantum**  | ULTRIUM-TD2 至 TD7、SDLT 220/320/600、DLT-V4/S4 | 无           |
| **Dell**     | PowerVault TL1000/2000/4000、ML3/ML6000       | 部分          |
| **STK**      | T10000A/B/C/D                                | 无           |
| **Overland** | NEO 2000e/4000e/8000e、T50e/T24e/TFinity      | 无           |
| **Spectra**  | T-Finity、T950、T380、T120、T50                  | 无           |


### 磁带库（50+ 种配置）


| 厂商           | 型号                                       |
| ------------ | ---------------------------------------- |
| **IBM**      | 03584L22/L32/L72、3584 UltraScalar        |
| **HP**       | MSL2024/4048/8096、MSL6000、EML E-Series   |
| **Quantum**  | Scalar i3/i6/i40/i80/i6000、SuperLoader 3 |
| **Dell**     | PowerVault TL/ML 系列                      |
| **Overland** | NEO 系列、T 系列                              |
| **Spectra**  | T-Finity、T 系列                            |


---

## 参与贡献 & 开发

**从源码构建**

**构建要求：** Rust 1.78+、Go 1.24+、Node.js 18+

```bash
git clone https://github.com/Holo-VTL/Holo.git
cd Holo

# 构建数据面 (Rust)
cd data-plane && cargo build --release && cd ..

# 构建控制面 (Go)
cd control-plane && go build -o bin/api ./cmd/api && cd ..

# 构建 Web 控制台 (Node.js)
cd web-console && npm install && npm run build && cd ..
```

**运行测试**

```bash
# 运行所有 Rust 测试（197 个测试）
cd data-plane && cargo test

# 运行所有 Go 测试
cd control-plane && go test ./...

# 运行代码规范检查
bash scripts/lint-guardrails.sh
```

**项目结构**

```
holo/
├── data-plane/              # Rust — SCSI 磁带仿真 & 存储引擎
│   └── src/
│       ├── iscsi/           # CDB 分发、驱动器/机械手处理、线协议
│       ├── scsi_tape/       # 磁带状态机、配置文件、身份标识、WORM、PR
│       ├── storage/         # 段引擎、块映射、去重、压缩整理、回收
│       └── media/           # 挂载桥接（附加/分离磁带盒）
├── control-plane/           # Go — REST API、认证、编排
│   └── internal/
│       ├── api/             # HTTP 处理器、路由、中间件
│       ├── domain/          # 模型 & 错误（无 I/O）
│       ├── orchestration/   # 服务层（Target 生命周期）
│       ├── repo/memory/     # 内存存储库
│       ├── audit/           # JSONL 追加式审计日志
│       ├── auth/            # 访问评估器
│       └── config/          # 环境驱动配置
├── web-console/             # React — 浏览器管理界面
├── infra/                   # TCMU Handler (C11)、CI
└── scripts/                 # 安装、检查、验证脚本
```

**代码规范**

1. Fork 本仓库
2. 创建功能分支（`git checkout -b feature/my-feature`）
3. 为你的修改编写测试
4. 确保所有测试通过（`cargo test` + `go test ./...`）
5. 提交 Pull Request

- Rust：生产代码中禁止 `unsafe`/`unwrap()`，类型转换使用 `u32::try_from()`
- Go：使用 `decodeRequiredJSONBody`/`respondError`——禁止直接使用 `json.Decoder`/`http.Error`
- 所有修改必须通过 `scripts/lint-guardrails.sh` 检查

---

## 许可证

本项目基于 [MIT 许可证](LICENSE) 开源。

---

**Holo** — 全息磁带库

*一个由光与代码构成的匣子。*

[报告问题](https://github.com/Holo-VTL/Holo/issues) · [功能建议](https://github.com/Holo-VTL/Holo/issues) · [提问讨论](https://github.com/Holo-VTL/Holo/issues)
