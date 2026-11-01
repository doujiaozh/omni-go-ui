# Omni-Go-UI 架构文档

> 本文是设计深潜：为什么这样分层、为什么这样取舍、窗口和通信到底怎么跑起来的。
> **线上契约（帧格式、错误码、方法目录）只有一份，见 [docs/PROTOCOL.md](PROTOCOL.md)**；本文不复制它，复制出来的第二份一定会过期。

---

## 1. 设计哲学

一句话：**把"窗口"和"渲染"借给系统里已经装好的 Chromium，把"业务"和"状态"留在 Go 里。**

这条哲学是从一台具体机器上倒推出来的：Windows 7、8 GB 内存、一块 2010 年前的集成显卡。在这台机器上逐一对照现有方案：

| 方案 | 用户要额外背上什么 | CGO | GPU / OpenGL | Win7 | 体积量级 |
| --- | --- | --- | --- | --- | --- |
| Electron | 整个 Chromium + Node 运行时 | 不需要（Node 侧） | 需要，软件渲染极慢 | 止步 v22（Chromium 108） | 100 MB+ |
| CEF | 约 1 GB 的分发体积 | 需要 | 需要 | 可行但代价巨大 | 100 MB+ |
| Wails / webview_go | 系统 WebView2 运行时 | **需要** | 不需要 | WebView2 在 109 之后不再支持 Win7 | 10 MB + 运行时下载 |
| Fyne / Gio | 无 | **需要**（GLFW/cgo） | **需要** OpenGL | 基本不可用 | 20 MB 上下 |
| **Omni-Go-UI** | 复用用户已经装好的 Chromium | **零** | **零** | **可用** | **~5 MB** |

四条红线决定了所有后续设计：

1. **零第三方依赖** —— `go.mod` 里不允许出现任何 `require`。这让 `go build` 在离线机器上就能出产物，也让审计面收窄到标准库。
2. **零 CGO** —— 交叉编译、32 位 (`GOARCH=386`) 构建、以及 Win7 上工具链的可用性都因此成立。所有 Win32 调用走 `syscall`，不走 cgo。
3. **零 GPU 依赖** —— 不链接 OpenGL，不调 DirectX；合成全部交给 Chromium 的软件路径，必要时（`SoftwareRendering`）连浏览器内部的 GPU compositing 也关掉。
4. **Win7 可用** —— Go 1.20 是最后一条支持 Win7 的 Go 线，Chrome/Edge 109 是最后一批支持 Win7 的 Chromium。两者都不是巧合，是选型约束。

前提是诚实的：用户机器上要有一个 Chromium 系浏览器（Chrome/Edge/Brave/Vivaldi/360se/QQ）。这是整套方案唯一的妥协。作为交换，框架本身只剩约 5 MB 的单文件 exe，且无需任何运行时下载。

---

## 2. 四层架构

四层，一层一件事，谁也别越界：

```
                    ┌─────────────────────────────────────────────────────┐
                    │  demo —— 示例应用（package main 装配 + demo 包）      │
                    │  main.go: core → hub → /ws → win.RegisterRPC        │
                    └──────┬──────────────────────────────────┬───────────┘
                           │ ui_core.New / ui_bind.New        │ ui_window.Open
                           ▼                                  ▼
    ┌──────────────────────────────────────┐   ┌──────────────────────────────────┐
    │ ui_core —— 渲染内核                  │   │ ui_window —— 窗口启动器           │
    │  //go:embed webassets（omni.js 等）  │   │  探测 Chromium 系浏览器           │
    │  net/http 绑定 127.0.0.1 临时端口    │   │  chrome --app=<url>              │
    │  token 种 cookie，常量时间比较       │   │  --user-data-dir=<临时目录>      │
    │  /_omni/ 与 /-/health 免令牌         │   │  Win32: 剥框 / 拖拽 / 最小最大化  │
    │  严格 CSP / SPA 回退 index.html      │◄──┤  --app 窗口经 HTTP 加载页面      │
    └──────────────────┬───────────────────┘   └──────────────────────────────────┘
                       │  /ws 挂载 hub.WebSocketHandler()
                       │  RFC 6455，仅文本帧
                       ▼
    ┌──────────────────────────────────────────────────────────────────────────────┐
    │ ui_bind —— 通信层                                                            │
    │  手写 RFC 6455 WebSocket 服务端（升级握手、分帧、掩码、1 MiB 帧上限）          │
    │  JSON-RPC 反射分发器：Handle() 注册，call/result/error 路由                    │
    │  Hub：客户端会话、hello 首帧、Broadcast 事件扇出、发送队列饱和即断线           │
    │  Origin host==Host 校验；panic 捕获为 INTERNAL，socket 存活                    │
    └──────────────────────────────────────────────────────────────────────────────┘
```

各层职责（契约边界）：

| 层 | 包 | 职责 | 不做什么 |
| --- | --- | --- | --- |
| 渲染内核 | `ui_core` | `//go:embed` 内嵌静态资源；极简 `net/http` 服务；token/cookie 门禁；安全响应头；SPA 回退 | 不懂 WebSocket，不懂业务 |
| 通信层 | `ui_bind` | RFC 6455 服务端；JSON-RPC 反射分发；会话管理；事件广播 | 不读文件，不开端口，不碰窗口 |
| 窗口启动器 | `ui_window` | 浏览器探测（注册表/PATH/.app bundle）；`--app` 启动；临时 profile；Win32 剥框与拖拽；`win.*` RPC | 不懂业务，不提供 UI |
| 示例应用 | `demo` | 系统监视器：每秒采样、Top 进程、环形历史、告警；演示如何把上面三层装配成一个完整应用 | 不引入任何框架私有 API |

依赖方向是单向的：`demo` → `ui_window` → `ui_bind` → `ui_core`。`ui_bind` 不依赖 `ui_core`，所以它也能挂在任何别的 `http.Handler` 上单测。

---

## 3. 启动时序（六步，顺序不可调换）

```
main                ui_window            ui_core              ui_bind            浏览器
 │ │                   │                    │                    │                 │
 │ │ ①Detect()─────────►│                    │                    │                 │
 │ │   注册表 App Paths │                    │                    │                 │
 │ │ ◄──────Browser─────│                    │                    │                 │
 │ │                                        │                    │                 │
 │ │ ②New(opts)─────────────────────────────►│ New(opts...)────────►│                │
 │ │   core.Start() 绑 127.0.0.1:0、生成 token│ hub.Handle(...)     │                │
 │ │                                        │ ◄─*Server/*Hub──────│                │
 │ │ ③core.Handle("/ws", hub.WebSocketHandler())                   │                │
 │ │   win.RegisterRPC(hub) 注册 win.*                            │                │
 │ │                                        │                    │                 │
 │ │ ④Open(Options{URL: core.URL(),...})────►│                    │                 │
 │ │   --app=http://127.0.0.1:p/?k=<token>   │                    │                 │
 │ │ ◄──────*Window（立即返回，不阻塞）───────│                    │                 │
 │ │                                        │                    │                 │
 │ │ ⑤trackWindow(): 后台枚举窗口、剥框      │                    │                 │
 │ │                                        │                    │                 │
 │ │                                        │  GET /?k=<token> ────────────────────►│
 │ │                                        │ ◄────── index.html + 种 omni_id cookie│
 │ │                                        │  GET /_omni/omni.js ────────────────►│
 │ │                                        │  GET /ws（升级握手 + Origin 校验）────►│
 │ │                                        │ ◄──── ⑥hello（方法表/perf/features）──│
 │ │ └─ win.Wait() 阻塞直至窗口关闭           │                    │                 │
```

1. **探测浏览器** —— `ui_window.Detect()`（或 `DetectBrowser(OMNI_BROWSER)`）。Windows 先查注册表 App Paths（`HKLM` 64 位视图 → 32 位视图 → `HKCU`），再扫 `Program Files` / `%LOCALAPPDATA%` 等常见路径；Linux 走 `exec.LookPath`；macOS 扫 `/Applications` 下的 `.app` bundle。非 `chromium` 系（IE 一律拒绝、Firefox 是 `gecko`）直接出局。
2. **起服务** —— `ui_core.New(opts)` 造 `*Server`，`core.Start()` 把监听绑到 `127.0.0.1:0`（内核分配临时端口，避开防火墙弹窗），并生成 128 位随机 token。此刻 `Origin()` / `URL()` / `Port()` 才有意义。
3. **注册 RPC** —— `ui_bind.New(...)` 造 Hub，`hub.Handle("my.method", fn)` 注册方法，`core.Handle("/ws", hub.WebSocketHandler())` 把 WebSocket 挂进 core 的 mux；`win.RegisterRPC(hub)` 补上 `win.minimize` / `win.maximize` / `win.close` / `win.drag` / `win.state`。
4. **`--app` 开窗** —— `ui_window.Open(Options{URL: core.URL(), ...})` 以 `--app=<带 token 的 URL>`、独立临时 `--user-data-dir` 启动浏览器进程，**立即返回** `*Window`，不阻塞。
5. **剥框** —— `trackWindow()` 在后台每 150 ms 枚举一次窗口（20 s 超时）：按 pid + `Chrome_WidgetWin_1` 类 + 可见 + 有标题 + 面积最大找到主窗口；若 `Frameless: true`，清 `WS_CAPTION | WS_SYSMENU | WS_THICKFRAME` 等并置 `WS_POPUP`，再 `SetWindowPos(SWP_FRAMECHANGED)` 让非客户区真正重算（详见 §7）。
6. **广播 hello** —— 浏览器加载页面，`omni.js` 连上 `/ws`，Hub 在任何业务帧之前先把 `hello`（方法表、平台、版本、`perf`、`features`）推给这个连接。此后 `main` 阻塞在 `win.Wait()`；窗口关闭即应用退出，随后 `core.Shutdown(ctx)` 收摊。

---

## 4. 双向通信与 wire 协议要点

传输层的事实：

- **HTTP**：`/` 与应用资产（`/app.css` 等）、框架资产（`/_omni/omni.js`、`/_omni/omni.css`）、`/favicon.ico`、`/-/health`；无扩展名路径 SPA 回退到 `index.html`。
- **WebSocket**：`/ws`，RFC 6455 由 `ui_bind/ws.go` **从零实现**（升级握手、分帧、掩码、close 处理），仅文本帧，不协商 `permessage-deflate`，帧上限 1 MiB（`WithMaxFrame` 可调）。
- **消息**：JSON 对象，`type` 字段 discriminator。完整帧格式见 **[PROTOCOL.md §2](PROTOCOL.md#2-message-envelope)**。

| 方向 | `type` | 用途 |
| --- | --- | --- |
| server → client | `hello` | 每个连接的第一帧：`methods` / `platform` / `version` / `perf` / `features` |
| client → server | `call` | 调用一个已注册的 Go 方法，`params` 永远是数组 |
| server → client | `result` | `call` 的成功应答 |
| server → client | `error` | `call` 的失败应答，带 `code` |
| server → client | `event` | 服务端主动推送，无 `id`、无回执、不重放 |
| client ↔ server | `ping` / `pong` | 连通性探针，传输层直接处理 |

Go 侧的分发规则（`ui_bind/bind.go`）：

- `Handle` 接受 `func()` / `func(T)` / `func(...) (R)` / `func(...) (R, error)`，可带前导 `*ui_bind.Context` 参数；超过 2 参 / 2 返回值、或第二个返回值不是 `error`，注册时直接 panic（启动期暴露编程错误，和 `http.HandleFunc` 一个脾气）。
- handler panic 被 recover，编成 `INTERNAL` 返回，**socket 存活**——一个坏方法不该带走整个连接。
- 错误码：`BAD_REQUEST`（帧畸形）/ `NOT_FOUND`（方法不存在）/ `BAD_PARAMS`（参数解码失败）/ `INTERNAL`（handler 出错或 panic）/ `TOO_LARGE`（超帧上限）；`OFFLINE` 只存在于客户端（连接断了，应答无处可去）。返回 `ui_bind.NewError(code, msg)` 可自选错误码，返回普通 `error` 一律 `INTERNAL`。
- `Broadcast` 只序列化一次、给每个 client 发一份拷贝；**客户端发送队列饱和时直接断该连接**，绝不阻塞广播方——慢消费者被丢弃而不是无限缓冲（PROTOCOL.md §2.3 的 "fire-and-forget" 契约由此保证）。

Demo 的方法目录（`sys.*` / `app.*` / `win.*` 的参数与返回结构）见 **[PROTOCOL.md §6](PROTOCOL.md#6-demo-method-catalog-v100)**。

---

## 5. 安全模型

一句话：**loopback 绑定 + 随机 token + 严格 cookie + 同源 WebSocket 校验**，陌生网页够不到这个应用。

| 防线 | 实现 |
| --- | --- |
| 仅环回口 | 监听 `127.0.0.1`，从不绑 `0.0.0.0`——同时避开了 Windows 防火墙弹窗，LAN 上的其他机器也访问不到 |
| 一次性 token | 启动时 `crypto/rand` 生成 128 位随机 token；窗口打开 `http://127.0.0.1:<port>/?k=<token>` |
| 种 cookie | 首次 `GET /?k=<token>` 设置 `omni_id` cookie：`Path=/`、`HttpOnly`、`SameSite=Strict`、无 `Max-Age`（窗口关即失效） |
| 常量时间比较 | token 比较走 `subtle.ConstantTimeCompare`，防时序侧信道 |
| Origin 校验 | `/ws` 校验 `Origin` header 的 host 等于请求 `Host`，挫败同浏览器其他标签页的跨站 WebSocket 劫持 |
| 严格 CSP | `default-src 'self'`；`script-src 'self'`（内联脚本被禁）；`style-src 'self' 'unsafe-inline'`；`connect-src` 只允许 `self` 与 `ws://127.0.0.1:*` / `ws://localhost:*`；`object-src 'none'`、`base-uri 'none'`、`frame-ancestors 'none'` |
| 其他响应头 | `X-Frame-Options: DENY`、`X-Content-Type-Options: nosniff`、`Referrer-Policy: no-referrer` |
| 临时 profile | `--user-data-dir` 指向 `%TEMP%` 下一次性目录，进程退出即删；不碰用户的书签、扩展、登录态 |
| 无外链 | 应用页面加载的一切都来自 embed.FS；不连任何 CDN，不下载任何运行时 |

公共命名空间故意豁免令牌：`/_omni/*`、`/favicon.ico`、`/-/health`——它们不含任何 secret，`/-/health` 只回报存活信息，方便监控脚本探活。不合令牌的请求拿到 `403` 和一个解释启动流程的极简 HTML 页面。

`OMNI_DEV=1` 会**关闭 token 门禁**（任何 `GET /` 自动发新 token，且所有响应 `no-store`），这是给开发热重载用的；**永远不要带进生产产物**。

---

## 6. Win7 与老硬件适应

- **引擎版本红线**：Chrome 109 / Edge 109 是最后支持 Win7 的版本。协议层面也因此克制——`omni.js` 用 ES5 安全语法，不用 `EventSource`、`structuredClone`、top-level `await`，传输路径不用 optional chaining，以便在 360se、QQ 浏览器这类老旧 Chromium 壳里活下来。Chromium 60 以下（IE11、360 兼容模式）明确不支持，探测到 IE 直接拒绝启动并打印提示。
- **注册表探测优先**：Windows 下先查 `HKLM/HKCU\SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths`（64 位视图 → 32 位视图 → HKCU）——这是 per-user 安装（住在 `%LOCALAPPDATA%` 里的 Chrome/Edge）唯一可靠的发现途径；查不到再扫 `Program Files`、`ProgramFiles(x86)`、`%LOCALAPPDATA%`、`%APPDATA%` 下的常见安装路径。
- **360se 回退**：国产 Win7 机器常装 360 安全浏览器（Chromium 内核、支持 `--app`），探测路径里包含 `%APPDATA%\360se6\Application\360se.exe` 与 `360Chrome`，实测可用；`OMNI_BROWSER` 也接受别名 `360` / `qq`。
- **SoftwareRendering**：老显卡花屏/黑屏时，`Options.SoftwareRendering: true` 或 `OMNI_LOWEND=1` 会加上 `--disable-gpu --disable-gpu-compositing --swiftshader-webgl`，彻底走软件合成。
- **lite perf 档**：`OMNI_LOWEND=1` 让 `hello.perf = "lite"`，前端通过 `document.documentElement.dataset.perf` 关掉 `backdrop-filter`（毛玻璃）、渐变和非功能性动画——老 GPU 的合成负担主要来自这些。
- **窗口尺寸默认值**：1280×820 是为了塞进 1366×768 这块最常见的老面板。
- **MIME 硬编码**：Win7 上 `.js` 常被注册成 `text/plain`，框架对 `.js` / `.css` 等硬编码带 `charset=utf-8` 的正确类型，不信注册表。
- **`CalculateNativeWinOcclusion`**：Win7 + 老驱动上，被遮挡的窗口会停止重绘、恢复时留黑块；启动参数直接 `--disable-features` 掉它。
- **32 位支持**：`GOARCH=386` 可直接构建；Win7 32-bit + 2 GB 内存是明确目标，代码里不假设 64 位指针（`SetWindowLongPtrW` 在 32 位上不存在，自动回退 `SetWindowLongW`）。

构建命令（单文件 exe，约 5 MB）：

```bat
go build -ldflags "-s -w -H windowsgui" -o omnigoui.exe .
```

环境变量：`OMNI_DEV=1`（dev 模式免 token、开 DevTools 日志）、`OMNI_LOWEND=1`（lite perf 档 + 软件渲染）、`OMNI_BROWSER=<路径或别名 chrome/edge/brave/vivaldi/360/qq>`。

---

## 7. 无边框窗口实现剖析（Windows）

### 7.1 Win32 调用序列

全部通过 `syscall.NewLazyDLL("user32.dll")` + `LazyProc.Call` 完成，**零 CGO**：

```
trackWindow()（goroutine，150 ms 轮询，20 s 超时）
  └─ findMainWindow(pid)
       └─ EnumWindows(callback)                  枚举所有顶层窗口，过滤：
            ├─ IsWindowVisible                   不可见 → 跳过（Chrome 有隐藏中间窗口）
            ├─ GetWindowThreadProcessId          pid 不匹配 → 跳过
            ├─ GetWindowTextLengthW              无标题 → 跳过
            ├─ GetClassNameW == "Chrome_WidgetWin_1"  非 Chromium 主窗口类 → 跳过
            └─ GetWindowRect                     取可见面积最大者
  └─ stripFrame(hwnd)（仅 Frameless: true 且非 Kiosk）
       ├─ GetWindowLongPtrW(GWL_STYLE)
       │    style &^= WS_CAPTION | WS_SYSMENU | WS_THICKFRAME
       │             | WS_MINIMIZEBOX | WS_MAXIMIZEBOX | WS_BORDER | WS_DLGFRAME
       │    style |= WS_POPUP
       ├─ SetWindowLongPtrW(GWL_STYLE, style)
       ├─ GetWindowLongPtrW(GWL_EXSTYLE) &^= WS_EX_WINDOWEDGE → SetWindowLongPtrW
       └─ SetWindowPos(hwnd, ..., SWP_FRAMECHANGED | SWP_NOZORDER
                                 | SWP_SHOWWINDOW | SWP_NOACTIVATE)
            ↑ 没有 SWP_FRAMECHANGED，非客户区不会重算，
              窗口会留着旧标题栏的"幽灵"尺寸
```

最小化 / 最大化 / 还原用 `ShowWindowAsync`（`SW_MINIMIZE=6` / `SW_MAXIMIZE=3` / `SW_RESTORE=9`）：异步版本避免阻塞 RPC 线程，也不和拖拽循环抢锁。窗口状态读取（`win.state`）用 `GetWindowRect` + 读 `GWL_STYLE` 里的 `WS_MAXIMIZE` / `WS_MINIMIZE` 位。

### 7.2 为什么是轮询而不是 SetWindowsHookEx

拖拽的核心矛盾：剥掉 `WS_CAPTION` 之后，系统不再替你移动窗口，而**没有人会给你发 mouseup**。两个选项：

- `SetWindowsHookEx(WH_MOUSE_LL, ...)`：能收到真正的鼠标消息，但需要一个常驻消息循环（`GetMessage` 泵），且低级鼠标钩子是**全局**的——要么自己跑一个独立线程的消息泵，要么处理被其他应用事件抢占的复杂度，与 `EnumWindows` 回调已有的线程亲和性（`runtime.LockOSThread`）纠缠在一起。
- **轮询**：只需要 `GetCursorPos` + `GetAsyncKeyState` + `SetWindowPos` 三个无状态 user32 调用，塞进一个带 `time.Ticker` 的 goroutine，`LockOSThread` 保证与窗口枚举同线程语义，`select` 同时监听取消信号与进程退出 channel。

选轮询：它保持在"零 CGO syscall 最小集合"里，代码路径是一条直线，没有任何消息泵、钩子链、反注入的复杂度。代价是 ~90 Hz 的唤醒频率——在一台目标机器上完全可承受，换来的是可读、可审计、可取消。

### 7.3 拖拽循环

前端在自己的标题栏区域 `mousedown` 时 `omni.call('win.drag', 'start')`，`mouseup` 时 `'end'`：

```go
// ui_window/frameless_windows.go（节选，展示算法而非完整代码）
ticker := time.NewTicker(time.Second / 90)        // ~90 Hz
for {
    select { case <-cancel: return                // win.drag("end") / Close()
             case <-w.exit: return                // 浏览器退出了
             case <-ticker.C: }
    if GetAsyncKeyState(VK_LBUTTON)&0x8000 == 0 { // 按键松开 → 自动结束
        return
    }
    GetCursorPos(&p)
    SetWindowPos(hwnd, 0,
        winX + (p.X - origX), winY + (p.Y - origY),  // 锚点 + 光标位移
        0, 0, SWP_NOSIZE | SWP_NOZORDER | SWP_NOACTIVATE)
}
```

锚点在 `start` 时一次性记录：窗口矩形原点 `(winX, winY)` 与光标原点 `(origX, origY)`，之后每帧只做加法，窗口因此"粘"在光标上而不是跳到光标处。`SWP_NOSIZE | SWP_NOZORDER | SWP_NOACTIVATE` 避免每帧重算尺寸/层级/焦点，拖拽因此不抖。

补充：`-webkit-app-region: drag` 在这里是**无效的**——那是 Electron 的私有实现，普通 Chromium `--app` 窗口不认。这也是拖拽必须回到 Go 侧 Win32 的原因。

---

## 8. 扩展指南

### 8.1 注册自己的 RPC 方法

```go
hub.Handle("my.method", func(ctx *ui_bind.Context, name string) (string, error) {
    if name == "" {
        return "", ui_bind.NewError(ui_bind.CodeBadParams, "name is required")
    }
    ctx.Emit("my:ack", map[string]any{"of": name}) // 只回给这个调用者
    return "hello " + name, nil
})
```

签名规则：0–2 个参数（第一个可以是 `*ui_bind.Context`），0–2 个返回值（第二个必须是 `error`）；参数/返回值须可 JSON 编解码；缺省或 `null` 参数解码为 T 的零值。注册时签名不合法会 panic——这是故意的，启动期错误最容易修。前端：`await omni.call('my.method', 'world')`。

### 8.2 广播事件

```go
// 推给所有已连接窗口；n = 实际写入的 socket 数
n := hub.Broadcast("my:event", map[string]any{
    "ts":   time.Now().Unix(),
    "value": 42,
})
_ = n // 慢消费者会被断线，不会阻塞调用方
```

事件是 fire-and-forget：无 `id`、无回执、不重放。只想推给单个连接用 `ctx.Emit("my:private", data)`（在 handler 里）或 `OnConnect` 钩子里收到的 `*Context`。没人连着时可以短路掉采样逻辑：`if hub.Clients() == 0 { continue }`——demo 的 Sampler 就是这么省电的。

### 8.3 换前端（ui_core.Options.Assets）

```go
//go:embed web
var webFS embed.FS

func main() {
    assets, err := fs.Sub(webFS, "web") // index.html 必须在 fs.FS 根
    if err != nil { log.Fatal(err) }
    core := ui_core.New(ui_core.Options{
        Assets: assets,        // 换成你自己的前端
        Title:  "My App",
    })
    // ... hub / /ws / Open ...
}
```

页面上三行接入框架运行时，之后整套 `omni.call` / `omni.on` 就能用：

```html
<link rel="stylesheet" href="/_omni/omni.css">
<script src="/_omni/omni.js"></script>
<script src="app.js" defer></script>
```

应用资产优先于框架资产；无扩展名路径回退到你的 `index.html`（SPA 路由可用）；`Assets` 为 `nil` 时回落到框架占位页。`OMNI_DEV=1` 时所有响应 `no-store`，改完文件刷新页面即可，不用重启进程。

---

## 9. 目录结构

```
omni-go-ui/
├── main.go                  # 入口：装配 core / hub / window，阻塞在 win.Wait()
├── go.mod                   # module github.com/omnigo/omni-go-ui，go 1.20，零 require
├── LICENSE                  # MIT（Copyright (c) 2026 Omni-Go-UI contributors）
├── README.md                # 项目自述（中英双语）
├── ui_core/                 # 第一层：embed.FS + net/http + token/cookie 门禁
│   ├── core.go              #   Server、Options、CSP、SPA 回退、健康检查
│   ├── embed.go             #   //go:embed webassets
│   └── webassets/           #   框架自身资产，挂在 /_omni/ 下
│       ├── omni.js          #     window.omni：RPC 客户端，ES5 安全语法
│       ├── omni.css         #     基础样式 + [data-perf="lite"] 降级规则
│       └── index.html       #     占位页
├── ui_bind/                 # 第二层：手写 RFC 6455 + JSON-RPC 分发 + 广播
│   ├── ws.go                #   WebSocket 协议（升级、分帧、掩码、帧上限）
│   ├── bind.go              #   反射方法表、panic 恢复、参数解码
│   ├── hub.go               #   Hub / Client / hello / Broadcast / Context.Emit
│   └── errors.go            #   错误码与 RPCError
├── ui_window/               # 第三层：浏览器探测、--app 启动、无边框、win.* RPC
│   ├── window.go            #   Window / Options / RegisterRPC / 生命周期
│   ├── detect_windows.go    #   注册表 App Paths + 常见路径
│   ├── detect_linux.go      #   exec.LookPath
│   ├── detect_darwin.go     #   .app bundle 扫描
│   ├── frameless_windows.go #   剥框 / 最小最大化 / 拖拽轮询（user32 syscall）
│   └── frameless_other.go   #   非 Windows 的 no-op 桩（返回 ErrUnsupported）
├── demo/                    # 第四层：示例应用（系统信息监视器）
│   ├── *.go                 #   Sampler（每秒采样）、sys.* 方法、告警消抖
│   └── web/                 #   仪表盘前端（index.html / app.css / app.js）
└── docs/
    ├── PROTOCOL.md          #   线上契约，唯一事实来源
    └── ARCHITECTURE.md      #   本文：设计深潜
```

## 10. 已知限制

诚实的部分：

- **没有多窗口**：一个 `ui_window.Open` 对应一个 `*Window`。开两个应用实例就是两个独立进程、两个端口、两个临时 profile，实例间没有 IPC。
- **非 Windows 的窗口控制是 `ErrUnsupported`**：`win.minimize` / `win.maximize` / `win.restore` / `win.state` 在 macOS / Linux 上返回 `ErrUnsupported`（`frameless_other.go`），`win.drag` 是 no-op；窗口本身能正常打开和渲染，前端应依据 `hello.features` 里有没有 `frameless` 隐藏这些控件。
- **IE 内核不可用**：只有 Chromium 系（`Kind == "chromium"`）能开 `--app` 窗口；IE 一律拒绝启动，Firefox（`gecko`）也不支持。
- **没有托盘图标、没有全局热键**：要做得干净得再挂一个消息循环，v1 不承诺。
- **Chromium profile 每次运行隔离**：不污染用户数据，代价是首次启动多花两三百毫秒，且不能复用登录态。
- **体积取决于你的资产**：demo 约 5 MB，塞 30 MB 图片进去就是 30 MB。
- **依赖系统浏览器**：这是全套设计的前提（见 §1），不是缺陷补偿；无 Chromium 环境的机器跑不起来。

路线图：托盘 + 全局热键（纯 `user32`）→ 多窗口共享一个 Hub → Linux 无边框（`_MOTIF_WM_HINTS`）→ 实例间 IPC（带 token 的本机 socket）→ 构建期预压缩资产。
