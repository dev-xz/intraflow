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

## 平台支持

- **macOS** — 主要目标，原生体验（WKWebView + 原生管理员密码框 + 开机自启：通过
  `SMAppService` 注册到"系统设置 → 登录项 → 登陆时打开"，用户可随时在系统设置里开关）
- **Windows** — hosts 写入（UAC）和自启（注册表 Run 项）已实现
- **Linux** — hosts 写入（pkexec/sudo）和自启（.desktop autostart）已实现
