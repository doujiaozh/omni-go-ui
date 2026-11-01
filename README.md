# Omni-Go-UI — 纯 Go 零 CGO 的桌面 GUI 框架

> 用 `//go:embed` 装前端，用 `net/http` 在环回口发出去，用手写的 RFC 6455 说 JSON-RPC，最后让系统里已经装好的 Chrome/Edge 用 `--app` 开一个没有地址栏的窗口。
> 没有 CGO，没有 OpenGL，没有 Node，没有 `node_modules`。

![Go](https://img.shields.io/badge/Go-1.20%2B-00ADD8?style=flat-square&logo=go&logoColor=white)
![CGO](https://img.shields.io/badge/CGO-none%20required-2ea44f?style=flat-square)
![Dependencies](https://img.shields.io/badge/dependencies-0-2ea44f?style=flat-square)
![Platform](https://img.shields.io/badge/platform-Windows%20%7C%20macOS%20%7C%20Linux-6f42c1?style=flat-square)
![Binary](https://img.shields.io/badge/binary-single%20file-e0a000?style=flat-square)
![License](https://img.shields.io/badge/license-MIT-blue?style=flat-square)

[中文](#omni-go-ui--纯-go-零-cgo-的桌面-gui-框架) · [English](#english)

---

> Omni-Go-UI：摒弃 CGO 和显卡依赖，用 Go 和 Web 技术，在 Win7 上重构现代桌面体验。

---

## 为什么再造一个 Go GUI？

因为现有的每一条路，在 Windows 7 + 8 GB 内存 + 一块老掉牙的集成显卡这台机器上，都要么装不上，要么跑不动，要么让你先装一个 100 MB 的运行时。

| 方案 | 你要额外背上的东西 | CGO | GPU / OpenGL | Win7 | 体积量级 |
| --- | --- | --- | --- | --- | --- |
| Electron | 整个 Chromium + Node 运行时 | 不需要（Node 侧） | 需要，软件渲染极慢 | 止步 v22（Chromium 108） | 100 MB+ |
| Wails / webview_go | 系统 WebView2 运行时 | **需要** | 不需要 | WebView2 在 109 之后不再支持 Win7 | 10 MB + 运行时 |
| Fyne / Gio | 无 | **需要**（GLFW/cgo） | **需要** OpenGL | 基本不可用 | 20 MB 上下 |
| **Omni-Go-UI** | 复用用户已经装好的 Chromium | **零** | **零**（纯 HTML/CSS 合成） | **可用** | **~5 MB** |

先把话说清楚：我们不是在"重新实现一个浏览器"。我们承认一个前提——**这台机器上大概率已经有一个现代 Chromium**。我们只是把"窗口"和"渲染"这两件事借给它，把"业务"和"状态"留在 Go 里。代价是：用户必须先装 Chrome 或 Edge（109 及以上）。这就是这套方案的全部妥协，没有别的暗坑。

## 架构

三个包，一层一件事，谁也别越界：

```
                    ┌────────────────────────────────────────────────┐
                    │  main.go —— 你的应用（package main）            │
                    │  ui_core  +  ui_bind  +  ui_window  +  demo     │
                    └──────┬───────────────────────────────┬─────────┘
      //go:embed assets    │                               │ exec.Command
                           ▼                               ▼
     ┌──────────────────────────────────┐   ┌──────────────────────────────────┐
     │ ui_core                          │   │ ui_window                        │
     │  fs.FS (embed.FS)                │   │  探测 Chrome/Edge/Brave/360se    │
     │  net/http  @ 127.0.0.1:0         │   │  chrome --app=<url>              │
     │  GET /            ▸ index.html   │◄──┤  --user-data-dir=<临时目录>      │
     │  GET /_omni/*     ▸ 框架资产     │HTTP│  --disable-features=...          │
     │  GET /ws          ▸ ui_bind.Hub  │   │  SetWindowLongPtr 去边框（可选） │
     └───────────────┬──────────────────┘   └──────────────────────────────────┘
                     │  WebSocket（RFC 6455，文本帧）
                     │  JSON-RPC：call / result / error / event
                     ▼
     ┌──────────────────────────────────┐       无地址栏的 app 窗口
     │ ui_bind.Hub                      │       （Chromium 自己画的）
     │  Handle("sys.stats", fn) ▸ 反射  │
     │  Broadcast("sys:stats", v) ▸ 扇出│
     └──────────────────────────────────┘
```

启动顺序，六步，一步都不能换：

1. `main` 组装 `ui_core.Options`：`Assets` 指向你 `//go:embed` 进来的 `fs.FS`（`index.html` 在根），顺带 `Title`、`LowEnd`。
2. `ui_core.New(opts)` 造出 `*Server`；此时还没监听任何端口。接着 `ui_bind.New(...)` 造 Hub，用 `hub.Handle(name, fn)` 注册方法，再用 `core.Handle("/ws", hub.WebSocketHandler())` 把 WebSocket 挂上去。
3. `core.Start()`：内核分配一个临时端口绑到 `127.0.0.1`，生成 128 位随机 token，静态资源与 `/ws` 一起就绪。`Origin()` / `URL()` / `Port()` 此刻才有意义。
4. `ui_window.Detect()` 找浏览器（Windows 走注册表 App Paths，macOS/Linux 走 well-known 路径与 `PATH`），`ui_window.Open(...)` 用 `--app=<URL>?k=<token>` 拉起一个**独立 profile** 的进程，立刻返回 `*Window`，不阻塞。
5. 浏览器请求 `/?k=<token>`，服务端种下 `HttpOnly; SameSite=Strict` 的 `omni_id` cookie 并吐出 `index.html`；页面加载 `/_omni/omni.js`，连上 `ws://127.0.0.1:<port>/ws`，收到第一帧 `hello`（含方法表、`perf`、`features`）。
6. `win.RegisterRPC(hub)` 把 `win.minimize` / `win.maximize` / `win.close` / `win.drag` / `win.state` 挂进方法表；`win.Wait()` 阻塞到窗口关闭，之后 `core.Shutdown(ctx)` 收摊。

线上契约（帧格式、错误码、事件）只有一份：**[docs/PROTOCOL.md](docs/PROTOCOL.md)**。本文不复制它，因为复制出来的第二份一定会过期。

## 快速开始

```bash
# bash / zsh / Git Bash
cd omni-go-ui
go run .
```

```bat
:: cmd / PowerShell
cd omni-go-ui
go run .
```

跑起来会直接弹出一个没有地址栏、没有标签页的窗口。同时控制台会打印调试用的完整 URL 和 token——**窗口白屏或者你想在 DevTools 里看它到底在干什么时，把 URL 粘进普通浏览器标签页**，行为完全一致。

编译成单文件：

```bash
go build -o omnigoui .                                   # 约 6 MB
go build -trimpath -ldflags "-s -w" -o omnigoui .        # 约 5 MB，去掉符号表与 DWARF
```

```bat
go build -o omnigoui.exe .
go build -trimpath -ldflags "-s -w" -o omnigoui.exe .
```

32 位目标（Win7 32-bit 机器、2 GB 内存也能跑）：

```bat
set GOARCH=386 && go build -trimpath -ldflags "-s -w" -o omnigoui-386.exe .
```

三个环境变量覆盖默认行为：

| 变量 | 取值 | 作用 |
| --- | --- | --- |
| `OMNI_DEV` | `1` | 直接从磁盘读 `assets`（含 `/_omni/*`），且任何 `GET /` 自动发一个新 token。改完文件刷新页面即可，不用重启进程。 |
| `OMNI_LOWEND` | `1` | 低配档：传 `--disable-gpu`，并让 `hello.perf = "lite"`，前端去掉 `backdrop-filter`、渐变和动画。 |
| `OMNI_BROWSER` | 可执行文件路径 | 跳过探测，直接用这个浏览器（比如固定用 `"C:\Program Files (x86)\Google\Chrome\Application\chrome.exe"`）。 |

```bat
set OMNI_LOWEND=1 && go run .
set "OMNI_BROWSER=C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe" && go run .
```

```bash
OMNI_LOWEND=1 go run .
OMNI_BROWSER="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" go run .
```

## 最小可运行程序

`web/index.html` 随便放一个页面，然后：

```go
package main

import (
	"context"
	"embed"
	"io/fs"
	"log"

	"github.com/omnigo/omni-go-ui/ui_bind"
	"github.com/omnigo/omni-go-ui/ui_core"
	"github.com/omnigo/omni-go-ui/ui_window"
)

//go:embed web
var webFS embed.FS

func main() {
	app, _ := fs.Sub(webFS, "web")
	core := ui_core.New(ui_core.Options{Title: "Hello Omni", Assets: app})
	hub := ui_bind.New(ui_bind.WithDebug(true))

	hub.Handle("app.ping", func(echo string) (map[string]any, error) {
		return map[string]any{"pong": true, "echo": echo}, nil
	})
	core.Handle("/ws", hub.WebSocketHandler())

	if err := core.Start(); err != nil {
		log.Fatal(err)
	}
	win, err := ui_window.Open(ui_window.Options{URL: core.URL(), Title: "Hello Omni", Width: 900, Height: 640})
	if err != nil {
		log.Fatal(err)
	}
	win.RegisterRPC(hub) // win.minimize / win.maximize / win.close / win.drag / win.state

	log.Printf("debug URL: %s", core.URL())
	win.Wait() // 窗口关闭才返回
	core.Shutdown(context.Background())
}
```

前端 `index.html`，三行接入：

```html
<link rel="stylesheet" href="/_omni/omni.css">
<script src="/_omni/omni.js"></script>
<script src="app.js" defer></script>
```

`app.js` 里就是普通的 `await`：

```js
const pong = await omni.call('app.ping', 'hi');          // → { pong: true, echo: "hi" }
omni.on('sys:stats', (s) => render(s));                 // 服务端推送
omni.onStatus(({ state }) => document.body.dataset.conn = state); // connecting/connected/disconnected
```

`hello` 之前发起的 `omni.call` 会被排队，连上后按序发出——所以写在文件顶层的 `await` 也不会丢。

## API 速查

### `ui_core` —— 服务器与资产

| 名称 | 说明 |
| --- | --- |
| `New(Options) *Server` | `Options{Assets fs.FS, Title, DevMode, Address, Token, LowEnd, Version, OnLog}`；`Assets` 为 `nil` 时用内置占位页 |
| `(*Server) Start() error` | 绑定 `Address`（默认 `127.0.0.1:0`）、生成 token、开始服务 |
| `(*Server) Origin() string` | `http://127.0.0.1:<port>` |
| `(*Server) URL() string` | `Origin() + "/?k=<token>"`，**这个才是要丢给窗口的** |
| `(*Server) Port() int` / `Token() string` | 调试与日志用 |
| `(*Server) Handle(pattern string, h http.Handler)` | 挂你自己的路由（`/ws`、`/api/*` 都走这里） |
| `(*Server) Shutdown(ctx) error` | 优雅停机：关监听、关 socket、等在途请求 |
| `(*Server) ServeHTTP(w, r)` | `Server` 本身就是 `http.Handler`，方便单测与 `httptest` |
| `Assets() fs.FS` | 框架内嵌资产：`/_omni/omni.js`、`/_omni/omni.css` |

### `ui_bind` —— RPC 与推送

| 名称 | 说明 |
| --- | --- |
| `New(opts ...Option) *Hub` | 可选：`WithDebug`、`WithMaxFrame`、`WithTitle`、`WithPerf`、`WithFeature`、`WithCheckOrigin`、`WithLogger`、`WithHello` |
| `WithPerf("full"\|"lite")` | 决定 `hello.perf`，前端据此降级样式 |
| `WithFeature(name)` | 可重复，写进 `hello.features`（如 `"frameless"`） |
| `(*Hub) Handle(name string, fn any)` | 反射绑定：`func()`、`func(T)`、`func(...) (R)`、`func(...) (R, error)`，可带前导 `*ui_bind.Context` |
| `(*Hub) WebSocketHandler() http.Handler` | 挂到 `core.Handle("/ws", ...)` |
| `(*Hub) Methods() []string` | 已注册方法名，`hello` 里的就是它 |
| `(*Hub) Broadcast(event string, data any) int` | 向所有 socket 推事件，返回实际写入的 socket 数 |
| `(*Hub) Clients() int` | 当前连接数（后台任务靠它决定还要不要采样） |
| `(*Hub) OnConnect(func(*Context))` / `OnDisconnect(...)` | 连接生命周期钩子 |
| `(*Hub) Close() error` | 关闭全部连接并停止 Hub |
| `(*Context) Emit(event string, data any) int` | 只推给这一个客户端 |
| `Context{ClientID, Remote}` | 单连接上下文，`Handle` 可选首参 |

### `ui_window` —— 浏览器窗口

| 名称 | 说明 |
| --- | --- |
| `Detect() (Browser, error)` | 探测已安装的 Chromium 系浏览器；`Browser{Name, Path, Kind, Version}` |
| `Open(Options) (*Window, error)` | 拉起窗口并**立刻返回**；`Options{URL, Title, Width, Height, X, Y, Frameless, Browser, ProfileDir, Debug, SoftwareRendering, Kiosk}` |
| `(*Window) Wait() error` | 阻塞到窗口进程退出——这就是你的 `main` 结尾 |
| `(*Window) Close() error` | 主动关窗 + 清理进程树与临时 profile |
| `(*Window) PID() int` / `Browser() Browser` | 诊断用 |
| `(*Window) RegisterRPC(hub *ui_bind.Hub)` | 注册 `win.*` 一组方法 |
| `(*Window) Minimize()` / `Maximize()`（切换）/ `Restore()` | 窗口控制 |

注意：`ui_window` 没有 `New` 构造器，窗口是 `Open` 的产物；探测函数叫 `Detect`，不是 `NewBrowser`。

### `demo` —— 示例应用

| 名称 | 说明 |
| --- | --- |
| `Assets() fs.FS` | 仪表盘前端资产 |
| `Stats() (Stats, error)` | 一次系统采样：CPU、内存、交换、运行时长、goroutine 数等 |
| `Processes(limit int) ([]Proc, error)` | 进程排行，Windows 上走 Toolhelp32 |

`sys.*` / `app.*` / `win.*` 各方法的参数与返回结构，全部在 **[docs/PROTOCOL.md §6](docs/PROTOCOL.md)**。别在这里找第二份。

## Demo：系统信息监视器

`go run .` 默认就是这个：

- 深色玻璃质感的仪表盘，圆环 + 折线 + 进程排行，全部是 CSS/SVG，没有一个 canvas 依赖 GPU。
- 后端每 1 秒采一次，通过 `sys:stats` 推给所有已连接窗口（没人连着就不采，`hub.Clients()` 直接短路）。
- 进程排行用 Toolhelp32 快照，按内存占用降序，只取前 N 个——老机器上这点很关键。
- CPU 连续 3 帧 > 85% 或内存 > 90% 时推 `sys:alert`，前端右上角弹出告警 toast。
- 自定义标题栏：`-webkit-app-region` 在这里没用（那是 Electron 的私有实现），拖动走 `win.drag("start" | "end")` + Win32 光标轮询。
- 开了 `OMNI_LOWEND=1` 就自动切到 `perf=lite`：玻璃变纯色，动画只保留功能性的过渡。

## Windows 7 注意事项（这一节最值钱）

- **引擎版本**：Chrome 109 和 Edge 109 是最后一批支持 Win7 的版本。Chromium 60 以下（IE11、360 兼容模式）明确不支持。启动器探测到 IE 会拒绝启动并打印提示。
- **只绑环回口**：监听 `0.0.0.0` 会触发 Windows 防火墙弹窗；`127.0.0.1` 不会。这一条同时决定了安全模型能立得住。
- **不碰图形 API**：整个框架不链接 OpenGL、不调 DirectX，合成全交给 Chromium 的软件路径。老驱动花屏/黑屏时加 `--disable-gpu`（`SoftwareRendering: true` 或 `OMNI_LOWEND=1`）。
- **`CalculateNativeWinOcclusion`**：这个特性在 Win7 + 老驱动上会让被遮挡的窗口停止重绘，恢复时留下黑块。启动参数里直接 `--disable-features=CalculateNativeWinOcclusion`。
- **无边框是可选路径**：`EnumWindows` 按 PID 找到窗口 → `SetWindowLongPtr(GWL_STYLE)` 去掉 `WS_CAPTION | WS_THICKFRAME` → `SetWindowPos(SWP_FRAMECHANGED)`。32 位构建上 `SetWindowLongPtrW` 并不导出，得用 `SetWindowLongW`。拖动不是 `-webkit-app-region`，而是 60 Hz 轮询 `GetCursorPos` 算位移再 `SetWindowPos`——粗糙但可靠。有任何不对劲就把 `Frameless: false` 关掉，退回系统标题栏，功能不受影响。
- **MIME 不要信注册表**：Win7 上 `.js` 被注册成 `text/plain` 是常见事故，`mime.TypeByExtension` 会把脚本顶掉。框架对 `.js` / `.css` 硬编码 `text/javascript` / `text/css` + `charset=utf-8`。
- **Go 版本**：Go 1.20 是最后一条支持 Win7 的官方线（1.21 起要求 Win10+）。这也是 `go.mod` 写死 `go 1.20` 的原因。
- **32 位**：`GOARCH=386` 直接可构建，Win7 32-bit + 2 GB 内存是明确的支持目标，别在代码里假设 64 位指针。
- **临时 profile**：`--user-data-dir` 指向 `%TEMP%` 下的一次性目录，退出即删。放在漫游配置目录里会显著拖慢启动。

## 局限与路线图

诚实的部分：

- **不内嵌 WebView2**。那需要 COM + CGO（或者一大堆 `syscall` 封装），直接违背零 CGO 的前提。
- **没有托盘图标、没有全局热键**。要做得干净得再挂一个消息循环，v1 不承诺。
- **无边框拖动只在 Windows 生效**，macOS/Linux 上是 no-op（可以 `Frameless: false`）。
- **实例之间没有 IPC**。开两个就是两个独立进程、两个端口、两个 profile。
- **Chromium profile 每次运行都是隔离的**：不会污染用户的书签/扩展，代价是首次启动多花两三百毫秒，且不能复用登录态。
- **体积取决于你的资产**，不是"永远 5 MB"。demo 约 5 MB，塞 30 MB 的图片进去就是 30 MB。
- 没有自动更新，没有移动端，没有多窗口。

路线图：托盘 + 全局热键（纯 `user32` 调用）→ 多窗口共享一个 Hub → Linux 无边框（`_MOTIF_WM_HINTS`）→ 实例间 IPC（带 token 的本机 socket）→ 构建期预压缩资产。

## 项目结构

```
omni-go-ui/
├── main.go                  # 入口：串起 core / bind / window，最后阻塞在 win.Wait()
├── go.mod                   # module github.com/omnigo/omni-go-ui，go 1.20，零依赖
├── README.md                # 你正在读的
├── LICENSE                  # MIT
├── ui_core/                 # 第一层：embed.FS + net/http + token/cookie 门禁
│   └── webassets/           # 框架自身资产，挂在 /_omni/ 下
│       ├── omni.js          # window.omni：RPC 客户端，ES5 安全语法
│       └── omni.css         # 基础样式 + [data-perf="lite"] 降级规则
├── ui_bind/                 # 第二层：手写 RFC 6455 + JSON-RPC 分发 + 广播
├── ui_window/               # 第三层：浏览器探测、--app 启动、无边框、win.* RPC
├── demo/                    # 示例应用：系统信息监视器
│   └── web/                 # 仪表盘前端（由 demo.Assets() 嵌入）
└── docs/
    ├── PROTOCOL.md          # 线上契约，唯一事实来源
    └── ARCHITECTURE.md      # 设计深潜：资产、HTTP、WS、RPC、窗口、安全
```

## 发布到 GitHub

```bash
git init -b main
git add .
git commit -m "feat: Omni-Go-UI - Zero-CGO modern desktop GUI framework in pure Go"
git remote add origin https://github.com/YOUR_NAME/omni-go-ui.git
git branch -M main
git push -u origin main
```

装了 `gh` 的话，一条就够（自动建仓 + 推送）：

```bash
gh repo create omni-go-ui --public --source=. --push
```

走 SSH 的话，只换 remote 那一行：

```bash
git remote add origin git@github.com:YOUR_NAME/omni-go-ui.git
```

## 许可证

MIT —— 见 [LICENSE](LICENSE)。商用、闭源分发、改完不公开，都行。

贡献：先开 issue 说清楚要动哪一层（`ui_core` / `ui_bind` / `ui_window` / `demo`），改动必须保持 `go.mod` 里没有 `require`。零依赖不是口号，是验收标准。

Built with [DeepSeek Harness](https://github.com/deepseek-ai).

---

## English

# Omni-Go-UI — A Zero-CGO Desktop GUI Framework in Pure Go

> Embed the frontend with `//go:embed`, serve it from a loopback `net/http` server, speak JSON-RPC over a hand-written RFC 6455 WebSocket, and let the Chrome/Edge already installed on the machine open a chromeless `--app` window.
> No CGO. No OpenGL. No Node. No `node_modules`.

![Go](https://img.shields.io/badge/Go-1.20%2B-00ADD8?style=flat-square&logo=go&logoColor=white)
![CGO](https://img.shields.io/badge/CGO-none%20required-2ea44f?style=flat-square)
![Dependencies](https://img.shields.io/badge/dependencies-0-2ea44f?style=flat-square)
![Platform](https://img.shields.io/badge/platform-Windows%20%7C%20macOS%20%7C%20Linux-6f42c1?style=flat-square)
![Binary](https://img.shields.io/badge/binary-single%20file-e0a000?style=flat-square)
![License](https://img.shields.io/badge/license-MIT-blue?style=flat-square)

[中文](#omni-go-ui--纯-go-零-cgo-的桌面-gui-框架) · [English](#english)

---

> “Omni-Go-UI: no CGO, no GPU, no excuses — reimagining the modern desktop experience on Windows 7 with Go and web technology.”

---

### Why another Go GUI?

Because every existing option, on a Windows 7 box with 8 GB of RAM and an ancient integrated GPU, either refuses to install, refuses to run, or asks the user to download a 100 MB runtime first.

| Approach | What the user carries | CGO | GPU / OpenGL | Win7 | Size |
| --- | --- | --- | --- | --- | --- |
| Electron | A whole Chromium + Node runtime | No (Node side) | Required; software path is slow | Stops at v22 (Chromium 108) | 100 MB+ |
| Wails / webview_go | The system WebView2 runtime | **Required** | Not required | WebView2 dropped Win7 after 109 | 10 MB + runtime |
| Fyne / Gio | Nothing | **Required** (GLFW/cgo) | **Requires** OpenGL | Effectively unusable | ~20 MB |
| **Omni-Go-UI** | Reuses the Chromium already installed | **None** | **None** (plain HTML/CSS compositing) | **Works** | **~5 MB** |

Let's be honest about the premise: we are not reimplementing a browser. We assume there is already a modern Chromium on the machine, and we borrow exactly two things from it — the window and the renderer — while state and logic stay in Go. The price of admission is a Chrome or Edge 109+ install. That is the whole compromise. There is no second, hidden one.

### Architecture

Three packages, one job each:

```
                    ┌────────────────────────────────────────────────┐
                    │  main.go — your app (package main)             │
                    │  ui_core  +  ui_bind  +  ui_window  +  demo    │
                    └──────┬───────────────────────────────┬─────────┘
      //go:embed assets    │                               │ exec.Command
                           ▼                               ▼
     ┌──────────────────────────────────┐   ┌──────────────────────────────────┐
     │ ui_core                          │   │ ui_window                        │
     │  fs.FS (embed.FS)                │   │  detect Chrome/Edge/Brave/360se  │
     │  net/http  @ 127.0.0.1:0         │   │  chrome --app=<url>              │
     │  GET /            ▸ index.html   │◄──┤  --user-data-dir=<temp>          │
     │  GET /_omni/*     ▸ framework    │HTTP│  --disable-features=...          │
     │  GET /ws          ▸ ui_bind.Hub  │   │  SetWindowLongPtr (frameless)    │
     └───────────────┬──────────────────┘   └──────────────────────────────────┘
                     │  WebSocket (RFC 6455, text frames)
                     │  JSON-RPC: call / result / error / event
                     ▼
     ┌──────────────────────────────────┐       a chromeless app window
     │ ui_bind.Hub                      │       (drawn by Chromium itself)
     │  Handle("sys.stats", fn) ▸ reflect│
     │  Broadcast("sys:stats", v) ▸ fan-out
     └──────────────────────────────────┘
```

The launch sequence, six steps, none of them reorderable:

1. `main` assembles `ui_core.Options`: `Assets` points at your `//go:embed`ed `fs.FS` (`index.html` at the root), plus `Title` and `LowEnd`.
2. `ui_core.New(opts)` returns a `*Server`; nothing is listening yet. Create the Hub with `ui_bind.New(...)`, register methods via `hub.Handle(name, fn)`, then mount the socket with `core.Handle("/ws", hub.WebSocketHandler())`.
3. `core.Start()` binds an ephemeral port on `127.0.0.1`, mints a 128-bit random token, and starts serving both assets and `/ws`. Only now do `Origin()`, `URL()` and `Port()` mean anything.
4. `ui_window.Detect()` finds a browser (registry App Paths on Windows; well-known paths and `PATH` lookup elsewhere), and `ui_window.Open(...)` launches `--app=<URL>?k=<token>` with an isolated profile. It returns a `*Window` immediately and never blocks.
5. The browser requests `/?k=<token>`; the server sets an `HttpOnly; SameSite=Strict` `omni_id` cookie and serves `index.html`. The page loads `/_omni/omni.js`, connects to `ws://127.0.0.1:<port>/ws`, and receives a `hello` frame (method table, `perf`, `features`).
6. `win.RegisterRPC(hub)` adds `win.minimize` / `win.maximize` / `win.close` / `win.drag` / `win.state` to the method table. `win.Wait()` blocks until the window closes, then `core.Shutdown(ctx)` tears everything down.

There is exactly one wire contract — **[docs/PROTOCOL.md](docs/PROTOCOL.md)**. This README deliberately does not restate it, because a second copy always goes stale.

### Quick start

```bash
# bash / zsh / Git Bash
cd omni-go-ui
go run .
```

```bat
:: cmd / PowerShell
cd omni-go-ui
go run .
```

A window with no address bar and no tabs appears. The console prints the debug URL and token — **if the window is blank, or you want DevTools, paste that URL into a normal browser tab**; behaviour is identical.

Build a single file:

```bash
go build -o omnigoui .                                   # ~6 MB
go build -trimpath -ldflags "-s -w" -o omnigoui .        # ~5 MB, symbols and DWARF stripped
```

```bat
go build -o omnigoui.exe .
go build -trimpath -ldflags "-s -w" -o omnigoui.exe .
```

32-bit target (Win7 32-bit with 2 GB of RAM is a supported target):

```bat
set GOARCH=386 && go build -trimpath -ldflags "-s -w" -o omnigoui-386.exe .
```

Three environment variables override the defaults:

| Variable | Value | Effect |
| --- | --- | --- |
| `OMNI_DEV` | `1` | Serve assets (including `/_omni/*`) from disk, and auto-issue a fresh token for any `GET /`. Edit, refresh, done — no restart. |
| `OMNI_LOWEND` | `1` | Lite profile: pass `--disable-gpu` and set `hello.perf = "lite"`; the frontend drops `backdrop-filter`, gradients and animation. |
| `OMNI_BROWSER` | path to executable | Skip detection and use this browser (e.g. pin Chrome 109). |

```bat
set OMNI_LOWEND=1 && go run .
set "OMNI_BROWSER=C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe" && go run .
```

```bash
OMNI_LOWEND=1 go run .
OMNI_BROWSER="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" go run .
```

### Minimal hello world

Put any page at `web/index.html`, then:

```go
package main

import (
	"context"
	"embed"
	"io/fs"
	"log"

	"github.com/omnigo/omni-go-ui/ui_bind"
	"github.com/omnigo/omni-go-ui/ui_core"
	"github.com/omnigo/omni-go-ui/ui_window"
)

//go:embed web
var webFS embed.FS

func main() {
	app, _ := fs.Sub(webFS, "web")
	core := ui_core.New(ui_core.Options{Title: "Hello Omni", Assets: app})
	hub := ui_bind.New(ui_bind.WithDebug(true))

	hub.Handle("app.ping", func(echo string) (map[string]any, error) {
		return map[string]any{"pong": true, "echo": echo}, nil
	})
	core.Handle("/ws", hub.WebSocketHandler())

	if err := core.Start(); err != nil {
		log.Fatal(err)
	}
	win, err := ui_window.Open(ui_window.Options{URL: core.URL(), Title: "Hello Omni", Width: 900, Height: 640})
	if err != nil {
		log.Fatal(err)
	}
	win.RegisterRPC(hub) // win.minimize / win.maximize / win.close / win.drag / win.state

	log.Printf("debug URL: %s", core.URL())
	win.Wait() // returns when the window closes
	core.Shutdown(context.Background())
}
```

Frontend `index.html`, three lines of wiring:

```html
<link rel="stylesheet" href="/_omni/omni.css">
<script src="/_omni/omni.js"></script>
<script src="app.js" defer></script>
```

And in `app.js`, plain `await`:

```js
const pong = await omni.call('app.ping', 'hi');          // → { pong: true, echo: "hi" }
omni.on('sys:stats', (s) => render(s));                 // server push
omni.onStatus(({ state }) => document.body.dataset.conn = state); // connecting/connected/disconnected
```

Calls made before `hello` are queued and flushed once connected, so a top-level `await` in your entry script is safe.

### API cheatsheet

#### `ui_core` — server and assets

| Name | Notes |
| --- | --- |
| `New(Options) *Server` | `Options{Assets fs.FS, Title, DevMode, Address, Token, LowEnd, Version, OnLog}`; `nil` assets fall back to a built-in placeholder page |
| `(*Server) Start() error` | Bind `Address` (default `127.0.0.1:0`), mint the token, start serving |
| `(*Server) Origin() string` | `http://127.0.0.1:<port>` |
| `(*Server) URL() string` | `Origin() + "/?k=<token>"` — **this is what you hand to the window** |
| `(*Server) Port() int` / `Token() string` | For logs and debugging |
| `(*Server) Handle(pattern string, h http.Handler)` | Mount your own routes (`/ws`, `/api/*`) |
| `(*Server) Shutdown(ctx) error` | Graceful stop: close listener, close sockets, drain in-flight requests |
| `(*Server) ServeHTTP(w, r)` | The `Server` is itself an `http.Handler` — easy to unit-test with `httptest` |
| `Assets() fs.FS` | Embedded framework assets: `/_omni/omni.js`, `/_omni/omni.css` |

#### `ui_bind` — RPC and push

| Name | Notes |
| --- | --- |
| `New(opts ...Option) *Hub` | Options: `WithDebug`, `WithMaxFrame`, `WithTitle`, `WithPerf`, `WithFeature`, `WithCheckOrigin`, `WithLogger`, `WithHello` |
| `WithPerf("full"\|"lite")` | Sets `hello.perf`; the frontend degrades from it |
| `WithFeature(name)` | Repeatable; goes into `hello.features` (e.g. `"frameless"`) |
| `(*Hub) Handle(name string, fn any)` | Reflection binding: `func()`, `func(T)`, `func(...) (R)`, `func(...) (R, error)`, optionally with a leading `*ui_bind.Context` |
| `(*Hub) WebSocketHandler() http.Handler` | Mount at `core.Handle("/ws", ...)` |
| `(*Hub) Methods() []string` | Registered method names — exactly what `hello` advertises |
| `(*Hub) Broadcast(event string, data any) int` | Push an event to every socket; returns how many were written |
| `(*Hub) Clients() int` | Current connection count (use it to skip sampling when nobody is listening) |
| `(*Hub) OnConnect(func(*Context))` / `OnDisconnect(...)` | Connection lifecycle hooks |
| `(*Hub) Close() error` | Close all sockets and stop the Hub |
| `(*Context) Emit(event string, data any) int` | Push to that one client only |
| `Context{ClientID, Remote}` | Per-connection context, optional leading `Handle` parameter |

#### `ui_window` — the browser window

| Name | Notes |
| --- | --- |
| `Detect() (Browser, error)` | Find an installed Chromium-family browser; `Browser{Name, Path, Kind, Version}` |
| `Open(Options) (*Window, error)` | Launch and **return immediately**; `Options{URL, Title, Width, Height, X, Y, Frameless, Browser, ProfileDir, Debug, SoftwareRendering, Kiosk}` |
| `(*Window) Wait() error` | Block until the window process exits — this is the end of your `main` |
| `(*Window) Close() error` | Close the window, kill the process tree, delete the temp profile |
| `(*Window) PID() int` / `Browser() Browser` | Diagnostics |
| `(*Window) RegisterRPC(hub *ui_bind.Hub)` | Register the `win.*` method set |
| `(*Window) Minimize()` / `Maximize()` (toggle) / `Restore()` | Window control |

Note: there is no `New` constructor for a window — `Open` is the factory, and detection is `Detect`, not `NewBrowser`.

#### `demo` — the sample app

| Name | Notes |
| --- | --- |
| `Assets() fs.FS` | Dashboard frontend assets |
| `Stats() (Stats, error)` | One system sample: CPU, memory, swap, uptime, goroutine count |
| `Processes(limit int) ([]Proc, error)` | Process ranking; Toolhelp32 on Windows |

Parameter and payload shapes for every `sys.*` / `app.*` / `win.*` method live in **[docs/PROTOCOL.md §6](docs/PROTOCOL.md)** — not here.

### Demo: system monitor

`go run .` runs this by default:

- A dark glass dashboard: rings, a line chart, a process ranking — pure CSS/SVG, no GPU-dependent canvas.
- The backend samples once per second and pushes `sys:stats` to every connected window (it skips sampling entirely when `hub.Clients()` is zero).
- The process ranking comes from a Toolhelp32 snapshot, sorted by memory descending, top-N only — which matters on old hardware.
- Three consecutive samples above 85% CPU or 90% memory push `sys:alert`, and the frontend raises a toast in the top-right corner.
- Custom titlebar: `-webkit-app-region` is a no-op here (that is an Electron-only implementation), so dragging goes through `win.drag("start" | "end")` plus a Win32 cursor-polling loop in Go.
- With `OMNI_LOWEND=1` it switches to `perf=lite`: glass becomes flat colour, and only functional transitions survive.

### Windows 7 notes and pitfalls

- **Engine version**: Chrome 109 and Edge 109 are the last builds that run on Win7. Anything below Chromium 60 (IE11, 360 in IE mode) is unsupported; the launcher refuses to start IE and prints a hint instead.
- **Loopback only**: binding `0.0.0.0` triggers the Windows firewall prompt; `127.0.0.1` does not. This one decision is also what makes the security model hold.
- **No graphics API**: the framework links no OpenGL and calls no DirectX; compositing is Chromium's software path. On broken old drivers, add `--disable-gpu` (`SoftwareRendering: true` or `OMNI_LOWEND=1`).
- **`CalculateNativeWinOcclusion`**: on Win7 with old drivers, this feature stops repainting occluded windows and leaves black rectangles behind. The launcher passes `--disable-features=CalculateNativeWinOcclusion`.
- **Frameless is opt-in**: `EnumWindows` → match by PID → `SetWindowLongPtr(GWL_STYLE)` to strip `WS_CAPTION | WS_THICKFRAME` → `SetWindowPos(SWP_FRAMECHANGED)`. On 32-bit builds `SetWindowLongPtrW` is not exported, so it falls back to `SetWindowLongW`. Dragging is not `-webkit-app-region` but a 60 Hz `GetCursorPos` delta loop feeding `SetWindowPos` — crude, but it holds. If anything feels off, `Frameless: false` restores the native titlebar with zero functional loss.
- **Don't trust the registry for MIME**: on Win7, `.js` is frequently registered as `text/plain`, and `mime.TypeByExtension` will happily break your scripts. The server hardcodes `text/javascript` / `text/css` with `charset=utf-8`.
- **Go version**: Go 1.20 is the last official line supporting Win7 (1.21+ requires Win10+). That is why `go.mod` pins `go 1.20`.
- **32-bit**: `GOARCH=386` builds and runs; Win7 32-bit with 2 GB of RAM is an explicit target, so don't assume 64-bit pointers.
- **Temp profile**: `--user-data-dir` points at a one-shot directory under `%TEMP%`, deleted on exit. Putting it on a roaming profile directory noticeably slows startup.

### Limitations and roadmap

The honest list:

- **No WebView2 embedding.** That needs COM + CGO (or a pile of `syscall` wrappers) and would break the zero-CGO premise outright.
- **No tray icon, no global hotkeys yet.** Doing it properly needs another message loop; v1 doesn't promise it.
- **Frameless dragging is Windows-only**; it is a no-op on macOS/Linux (`Frameless: false` there).
- **No IPC between instances.** Two instances are two processes, two ports, two profiles.
- **The Chromium profile is isolated per run**: it won't touch the user's bookmarks or extensions, at the cost of a few hundred ms of first-launch time and no shared login state.
- **Size depends on your assets**, it is not "always 5 MB". The demo is ~5 MB; ship 30 MB of images and you ship 30 MB.
- No auto-update, no mobile, no multi-window.

Roadmap: tray + global hotkeys (plain `user32` calls) → multiple windows sharing one Hub → frameless on Linux (`_MOTIF_WM_HINTS`) → cross-instance IPC (a token-guarded local socket) → pre-compressed assets at build time.

### Project layout

```
omni-go-ui/
├── main.go                  # entry point: wires core / bind / window, then blocks on win.Wait()
├── go.mod                   # module github.com/omnigo/omni-go-ui, go 1.20, zero requires
├── README.md                # this file
├── LICENSE                  # MIT
├── ui_core/                 # layer 1: embed.FS + net/http + token/cookie gate
│   └── webassets/           # framework assets, served under /_omni/
│       ├── omni.js          # window.omni — the RPC client, ES5-safe syntax
│       └── omni.css         # base styles + [data-perf="lite"] degradation rules
├── ui_bind/                 # layer 2: hand-written RFC 6455 + JSON-RPC dispatch + broadcast
├── ui_window/               # layer 3: browser detection, --app launch, frameless, win.* RPC
├── demo/                    # the sample app: system monitor
│   └── web/                 # dashboard frontend (embedded by demo.Assets())
└── docs/
    ├── PROTOCOL.md          # the wire contract, single source of truth
    └── ARCHITECTURE.md      # design deep-dive: assets, HTTP, WS, RPC, window, security
```

### Publish to GitHub

```bash
git init -b main
git add .
git commit -m "feat: Omni-Go-UI - Zero-CGO modern desktop GUI framework in pure Go"
git remote add origin https://github.com/YOUR_NAME/omni-go-ui.git
git branch -M main
git push -u origin main
```

With the `gh` CLI, one line creates the repo and pushes:

```bash
gh repo create omni-go-ui --public --source=. --push
```

SSH remotes only change the one line:

```bash
git remote add origin git@github.com:YOUR_NAME/omni-go-ui.git
```

### License

MIT — see [LICENSE](LICENSE). Commercial use, closed-source distribution, no obligation to publish changes.

Contributing: open an issue naming the layer you intend to touch (`ui_core` / `ui_bind` / `ui_window` / `demo`). Any change must keep `go.mod` free of `require` lines. Zero dependencies is not a slogan here; it is the acceptance criterion.

Built with [DeepSeek Harness](https://github.com/deepseek-ai).
