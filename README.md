# IntraFlow

在局域网内直连你的内网服务，让"公网穿透域名"在本机被接管到内网真实地址，不绕公网。

## 它解决什么问题

你在局域网内部署了服务，公网通过穿透也能访问（比如 `svc.example.com:8080`）。但当你在内网时，访问这个域名仍然绕公网一圈——慢、费带宽、还依赖穿透服务在线。

IntraFlow 在本机做两件事：

1. **hosts 劫持** — 把公网域名指向 `127.0.0.1`
2. **端口转发** — 在本机监听一个端口，把流量透明转发到内网真实地址（域名 + 端口）

这样浏览器访问 `http://svc.example.com:8080` 实际走的是内网直连，HTTP/HTTPS 端到端透传（IntraFlow 不解密 TLS）。

```
浏览器                      本机 IntraFlow                  内网
  │                            │                              │
  │  svc.example.com:8080      │  hosts: svc.example.com      │
  │  ────────────────────────► │    -> 127.0.0.1              │
  │                            │                              │
  │  连 127.0.0.1:8080         │  TCP 转发                     │
  │  ────────────────────────► │  127.0.0.1:8080 ────────────►│  nas.local:8080
  │                            │    (缓存解析 + 定期刷新)      │  (真实服务)
  │                            │                              │
```

## 使用方式

1. 启动 IntraFlow — 它常驻在菜单栏（右上角小图标），无窗口、无 Dock 图标
2. 点菜单栏图标 → "打开主界面" → 打开配置面板
3. 面板分两部分，直接在两表内编辑：
   - **托管域名**：填公网域名（例如 `svc.example.com`），该域名会被指向 `127.0.0.1`
   - **端口转发**：填本机监听端口 → 内网地址 : 内网端口（真实服务，地址可以是域名）
4. 底部"保存并生效" — 输入一次密码（修改系统 hosts 需要管理员权限），所有改动一次生效

**批量操作**：增删改、启用/停用都是暂存操作——先改后存，点一次"保存并生效"统一生效，只
弹一次密码框。如果改动不影响 hosts 域名映射（只改了转发目标地址/端口），甚至不弹密码。

**全局暂停**：面板顶部或菜单栏里可以"暂停托管"——一键清空 hosts 劫持并停止所有转发，
各转发的启用状态会保留，恢复后按原配置重新生效。

**退出确认**：若仍有域名在托管，点菜单栏"退出"会先询问——可选择"暂停托管并退出"（清
空 hosts 后再退出）、"直接退出"或"取消"。系统关机时不会弹出此提示。

## 技术栈

- **Go 后端** — 转发器、hosts 管理、配置、IPC
- **Wails v2** — GUI（系统 WKWebView，非 Electron，体积小）
- **systray** — 菜单栏托盘（`github.com/cardinalby/go-systray`，Wails 兼容的 fork）

## 架构：双进程

```
intraflow (宿主进程, 常驻 ~30MB)
├── 菜单栏托盘图标 (systray)
├── orchestrator (配置 + hosts + 转发器池)
├── IPC server (unix socket ~/.intraflow/ipc.sock)
├── 单实例锁 + 开机自启
└── "打开主界面" → spawn: intraflow --gui

intraflow --gui (GUI 子进程, 按需启动, 关窗即死)
├── Wails WKWebView (前端界面)
├── IPC client (调宿主的 orchestrator)
└── 关窗 → 进程退出 → web 内存释放
```

关键设计：
- **按需 GUI** — 面板不打开时没有 web 进程，省内存
- **两阶段提权** — host 验证+构建 hosts 内容（不提权）→ GUI 进程执行 osascript（前台进程，密码框正常弹出）→ host 完成最终化
- **缓存 + 定期刷新解析** — 转发启动时解析目标域名并缓存 IP，每 5 分钟（可配置）刷新一次；拨号失败还会即时重解析兜底，内网 IP 变了能及时跟上

## 开发

```bash
# 依赖: Go 1.23+, Node 18+, Wails CLI v2
go install github.com/wailsapp/wails/v2/cmd/wails@latest

# 开发模式（热重载）
wails dev

# 单元测试
go test ./internal/...

# 打包
wails build
# 产出: build/bin/intraflow.app
```

## 安装

本项目**不做 Apple 公证**（不购买 Apple Developer Program）。macOS 包由 `wails build` 做
ad-hoc 签名 —— 这已足够让开机自启（`SMAppService`）正常工作，但**不足以通过 Gatekeeper**。
所以从浏览器下载的 zip 首次启动会被拦一次，需要按下面的方式放行。

### 方式一：Homebrew（推荐）

```bash
brew install dev-xz/tap/intraflow
```

**必须用带 tap 的完整名字** `dev-xz/tap/intraflow`。Homebrew 7 起对第三方 tap 有信任机制：
写完整名字会隐式信任该 cask；若先 `brew tap dev-xz/tap` 再简写成 `brew install intraflow`，
会被 `these taps are not trusted` 挡住。

该 cask 会在安装后自动清除隔离标记（见 `packaging/homebrew/`），所以 brew 用户装完即可直接启动。

### 方式二：手动下载 zip

1. 下载 `intraflow-<version>-macos-universal.zip` 并解压
2. 把 `intraflow.app` 拖进 `/Applications`
3. 执行一次（清除隔离标记）：

```bash
xattr -d com.apple.quarantine /Applications/intraflow.app
```

之后双击即可正常启动，不会再有拦截。

### ⚠️ 不要用「仍要打开」

在「系统设置 → 隐私与安全性」里点 **「仍要打开」** 虽然也能启动，但会触发 macOS 的
**App Translocation**：应用会从 `/private/var/folders/.../AppTranslocation/<随机>/d/`
这个临时随机路径运行，而该路径在应用退出后即消失。

后果是**开机自启静默失效** —— 此时注册的登录项指向那个随机路径，重启后自然不存在。
应用已针对这种情况加了检测：在这种状态下开启自启会直接报错并提示正确做法，而不是
悄悄写入一个坏掉的登录项。

**安全的两种方式**（都不会 translocation）：

- `xattr -d com.apple.quarantine ...` —— 彻底清除标记
- Homebrew 安装 —— 其 quarantine 标记带有"已移动"标志位，不触发 translocation

若已经踩坑：把应用拖进「应用程序」，执行上面的 `xattr -d`，重新打开后再开启开机自启。

## 发布与签名

打 tag 即触发 `.github/workflows/release.yml`，构建 macOS / Windows / Linux 三个平台并创建
GitHub Release：

```bash
git tag v0.1.1
git push origin v0.1.1
```

也可以在 Actions 页面手动 `workflow_dispatch` 跑一遍不带 Release 的构建。产物命名：

| 平台 | 产物 |
| --- | --- |
| macOS（universal，x86_64 + arm64） | `intraflow-<version>-macos-universal.zip` |
| Windows（amd64） | `intraflow-<version>-windows-amd64-installer.exe`、`intraflow-<version>-windows-amd64.exe` |
| Linux（amd64） | `intraflow-<version>-linux-amd64.tar.gz` |

版本号从 tag 解析（`v0.1.0` → `0.1.0`），构建时写入 `wails.json` 的 `info.productVersion`，
再进到 `CFBundleShortVersionString` 和 Windows 文件版本信息。tag 必须是数字 semver
（`v1.2.3` 或 `v1.2.3-rc.1`），预发布后缀只用于产物文件名，不写进 bundle 版本。

Linux 构建用 `-tags webkit2_41`：Ubuntu 24.04 只有 `webkit2gtk-4.1`，Wails v2 默认链接的
`webkit2gtk-4.0` 不存在，不加这个 tag 会在 linking 阶段失败。

### 签名（可选，未配置则出无签名包）

签名完全是可选的：**只有对应的 secrets 存在时才会执行签名**，否则构建照常成功，只是产物
未签名。要开启就加下面这些 repository secrets：

**macOS** — Developer ID 签名 + 公证，需要全部 3 个签名 secret，加上 3 个公证 secret 才做公证：

| Secret | 说明 |
| --- | --- |
| `MACOS_CERT_P12_BASE64` | Developer ID Application 证书 `.p12` 的 base64 |
| `MACOS_CERT_PASSWORD` | 上面 `.p12` 的密码 |
| `MACOS_SIGN_IDENTITY` | 如 `Developer ID Application: Your Name (TEAMID)` |
| `APPLE_ID` | Apple ID 邮箱（公证用） |
| `APPLE_TEAM_ID` | 10 位 Team ID（公证用） |
| `APPLE_APP_PASSWORD` | App 专用密码（不是 Apple ID 密码） |

```bash
base64 -i DeveloperID.p12 | pbcopy   # 填入 MACOS_CERT_P12_BASE64
```

流程是：`wails build` 先做 ad-hoc 签名（为了让通知能工作），workflow 会移除它，再用
hardened runtime + `build/darwin/entitlements.plist` 重新签名（先内层 Mach-O 再整个
bundle），然后 `notarytool submit --wait` + `stapler staple`。

**为什么 macOS 值得签名**：签名只影响「首次启动是否被 Gatekeeper 拦」，**不影响开机自启**。
`wails build` 会自动做 ad-hoc 签名，而 ad-hoc 已满足 `SMAppService` 的要求（实测
`registerAndReturnOK` 返回成功、状态为 enabled）—— 所以不买 $99 也能正常自启。
公证（notarization）的作用是让**浏览器下载的包**首次双击不被拦；若采用上面的 Homebrew
或 `xattr -d` 方案，这一项可以从容放弃。若将来要签，上面的 secrets 配好即自动生效。

**Windows** — 可选，加这两个 secret 即启用 `signtool`：

| Secret | 说明 |
| --- | --- |
| `WINDOWS_CERT_PFX_BASE64` | 代码签名证书 `.pfx` 的 base64 |
| `WINDOWS_CERT_PASSWORD` | 上面 `.pfx` 的密码 |

注意：OV / EV 证书自 2024 年起都不再提供 SmartScreen 即时信誉，信誉需要靠下载量积累；
更省事的 CI 方案是 Azure Trusted Signing（云端 HSM，无需硬件 token）。

### 签名相关文件

- `build/darwin/entitlements.plist` — hardened runtime 签名用。目前故意为空：非沙盒、
  非 App Store 分发只需 hardened runtime，不需要 `com.apple.security.*` 权限。
  **文件内注释不能出现连续两个连字符**，签名工具会拒绝（`plutil` 却会通过）。

### 自动更新 Homebrew tap

发版后 `homebrew-tap` job 会自动把新的 `version` + `sha256` 推进 `dev-xz/homebrew-tap`，
用户 `brew install dev-xz/tap/intraflow` 始终拿到最新版。需要这个 secret：

| Secret | 说明 |
| --- | --- |
| `HOMEBREW_TAP_TOKEN` | 细粒度 PAT，对 `dev-xz/homebrew-tap` 仓库有 **Contents: Read and write** |

默认的 `GITHUB_TOKEN` 不能推别的仓库，所以必须单独配。**未配置时该 job 自动跳过**（只发
notice，不影响发版）。tap 仓库的初始化见 `packaging/homebrew/README.md`。

## 平台支持

- **macOS** — 主要目标，原生体验（WKWebView + 原生管理员密码框 + 开机自启：通过
  `SMAppService` 注册到"系统设置 → 登录项 → 登陆时打开"，用户可随时在系统设置里开关）
- **Windows** — hosts 写入（PowerShell `Start-Process -Verb RunAs`，原生 UAC 弹窗）和自启
  （注册表 Run 项）已实现。注意 Windows 分支尚未在真机上验证过（CI 只覆盖到构建与单元
  测试层面）
- **Linux** — hosts 写入（pkexec/sudo）和自启（.desktop autostart）已实现
