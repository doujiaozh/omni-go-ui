# Omni-Go-UI Protocol Specification `v1`

> The wire contract between the Go backend and the browser frontend.
> This file is the single source of truth: `ui_core`, `ui_bind` and any app frontend
> must agree with it. Last reviewed for `omni-go-ui v1.0.0`.

---

## 1. Transport

```
                    ┌──────────────────────────────┐
  Chrome/Edge --app │  HTTP  GET /  /app.css  ...  │  static assets, from embed.FS
        │           │                              │
        │           │  WS    GET /ws               │  JSON text frames only
        └──────────►└──────────────────────────────┘
                    127.0.0.1:<ephemeral port>
```

| Property        | Value                                                              |
| --------------- | ------------------------------------------------------------------ |
| Bind address    | `127.0.0.1` only (never `0.0.0.0`)                                  |
| Port            | ephemeral, chosen by the kernel (`:0`)                              |
| Static assets   | `GET /`, `GET /app.*`, framework files under `/_omni/*`             |
| WebSocket       | `GET /ws`, RFC 6455, **text frames only** (`opcode 0x1`)            |
| Compression     | none (`permessage-deflate` is not negotiated)                       |
| Max frame       | 1 MiB in, 1 MiB out                                                |

### 1.1 Handshake that never leaks to other pages

The server mints a random session token at startup and the launcher opens:

```
http://127.0.0.1:<port>/?k=<token>
```

* `GET /?k=<token>` → sets cookie `omni_id=<token>` (`Path=/`, `HttpOnly`,
  `SameSite=Strict`, no `Max-Age`, so it dies with the app window) and serves
  the app's `index.html`.
* Every later request must carry that cookie. `/ws` additionally verifies that
  the `Origin` header's host equals the request `Host`, which defeats
  cross-site WebSocket hijacking from any other page in the same browser.
* A request without a valid token/cookie gets `403` and a minimal HTML page.
* Dev mode (`OMNI_DEV=1`) auto-issues a fresh token for any `GET /` so a plain
  browser tab can be used for hot reload.

Security model in one line: **loopback bind + random token + strict cookie +
same-origin WebSocket check**, i.e. a stranger's web page cannot reach the app.

---

## 2. Message envelope

All messages are JSON objects with a `type` discriminator.

| Direction       | `type`   | Purpose                                     |
| --------------- | -------- | ------------------------------------------- |
| server → client | `hello`  | first frame on every connection              |
| client → server | `call`   | invoke a registered Go method                |
| server → client | `result` | successful reply to a `call`                 |
| server → client | `error`  | failed reply to a `call`                     |
| server → client | `event`  | server-initiated push (no request behind it) |
| client → server | `ping`   | liveness probe handled in the transport      |
| server → client | `pong`   | reply to `ping`                              |

### 2.1 `hello` (server → client, first frame)

```json
{
  "type": "hello",
  "version": "1.0.0",
  "clientId": "9f2c1ab4",
  "methods": ["app.info", "app.ping", "sys.stats", "win.minimize"],
  "platform": "windows",
  "arch": "amd64",
  "goVersion": "go1.20.14",
  "title": "Omni System Monitor",
  "perf": "full",
  "features": ["frameless", "processes", "series"]
}
```

| Field       | Meaning                                                              |
| ----------- | -------------------------------------------------------------------- |
| `methods`   | callable method names; the frontend uses it to hide dead UI          |
| `perf`      | `"full"` or `"lite"` (see §5) — set `document.documentElement.dataset.perf` |
| `features`  | capability flags, e.g. `frameless` decides whether window chrome shows |
| `clientId`  | stable id for this socket, useful in logs                            |

### 2.2 `call` / `result` / `error`

```json
{ "type": "call",   "id": 7, "method": "sys.processes", "params": [8] }
{ "type": "result", "id": 7, "data": [ { "pid": 1234, "name": "chrome.exe" } ] }
{ "type": "error",  "id": 7, "error": { "code": "NOT_FOUND", "message": "unknown method: nope" } }
```

* `id` is chosen by the client (number or string), echoed verbatim, unique per socket.
* `params` is **always an array** (may be omitted for zero-arg calls). Positional
  mapping: `params[0]` → first Go parameter.
* Exactly one `result` **or** `error` is sent per `call`.

Error codes:

| Code           | When                                                      |
| -------------- | --------------------------------------------------------- |
| `BAD_REQUEST`  | malformed envelope / unknown `type`                       |
| `NOT_FOUND`    | no method registered under that name                      |
| `BAD_PARAMS`   | params cannot be decoded into the Go signature            |
| `INTERNAL`     | handler returned a non-nil error                          |
| `TOO_LARGE`    | frame above the 1 MiB limit                               |
| `UNAUTHORIZED` | reserved for future per-method guards                     |
| `OFFLINE`       | **client-side only**: connection dropped before the reply |

### 2.3 `event` (server → client push)

```json
{ "type": "event", "event": "sys:stats", "data": { "cpu": 12.4 } }
```

Events are fire-and-forget: no `id`, no acknowledgement, never replayed.
A slow consumer is dropped, not buffered without bound.

---

## 3. JavaScript runtime (`/_omni/omni.js`)

Loaded by the app's `index.html` before `app.js`:

```html
<script src="/_omni/omni.js"></script>
```

`window.omni` is created immediately (before `DOMContentLoaded`) and connects
to `ws://<host>/ws` automatically, retrying with exponential backoff
(500 ms → 5 s) and re-sending nothing (calls in flight are rejected `OFFLINE`).

```js
// RPC: resolves with `data`, rejects with an Error carrying .code
const stats = await omni.call('sys.stats');
const procs = await omni.call('sys.processes', 8);   // → params: [8]
const echo  = await omni.call('app.echo', { a: 1 });

// Server push
const off = omni.on('sys:stats', (data) => render(data));
omni.once('hello-adjacent-event', fn);   // auto-unsubscribes after one call
off();                                   // unsubscribe manually

// Connection state
omni.onStatus(({ state }) => {});        // 'connecting' | 'connected' | 'disconnected'
omni.state;                              // current state string
const hello = await omni.ready;          // resolves once, with the hello payload
omni.hello;                              // null until the first hello
omni.connected;                          // boolean
omni.reconnect();                        // force an immediate reconnect
omni.version;                            // framework version string
```

Guarantees:

* `omni.call` before `hello` is queued and flushed once connected (no race for
  scripts that run on load).
* Unknown event names are ignored; handler exceptions are caught and logged,
  never breaking the dispatch loop.
* No globals other than `window.omni`.

---

## 4. Go side (`ui_bind`)

```go
hub := ui_bind.New(ui_bind.Options{Debug: true})
hub.Handle("sys.stats", func() (demo.Stats, error) { ... })          // 0 args
hub.Handle("sys.processes", func(limit int) ([]demo.Proc, error))     // 1 arg
hub.Handle("win.drag", func(ctx *ui_bind.Context, phase string) error) // conn-aware
hub.Broadcast("sys:stats", stats)                                     // → all clients
hub.HandleFunc("/ws")                                                 // http.Handler
```

* `Handle` accepts `func()` / `func(T)` / `func(...) (R)` / `func(...) (R, error)`,
  optionally with a leading `*ui_bind.Context` parameter.
* `T` / `R` are any JSON-marshalable types, decoded/re-encoded with `encoding/json`.
* A panicking handler is recovered and reported as `INTERNAL`; the socket survives.
* `Broadcast` is safe for concurrent use and returns the number of sockets written.

---

## 5. Performance profile (`perf`)

`hello.perf` is negotiated by the backend, not sniffed in the browser:

| Value  | Meaning                                                     | Frontend must |
| ------ | ----------------------------------------------------------- | ------------- |
| `full` | normal desktop; blur, gradients and canvas glow are enabled | keep the rich look |
| `lite` | software rendering / old GPU / low RAM (set `OMNI_LOWEND=1`) | drop `backdrop-filter`, gradients fall back to flat colors, no animation except functional transitions |

The frontend applies it as `document.documentElement.dataset.perf = hello.perf;`
and CSS keys off `:root[data-perf="lite"]`. See `ui_core/webassets/omni.css`.

---

## 6. Demo method catalog (v1.0.0)

`Stats` — the flat shape carried by every `sys:stats` push. Bytes are raw,
percentages are 0–100 floats, `updatedAt` is a unix timestamp in seconds:

```json
{
  "cpu": 12.4,
  "memory": 62.5,
  "memTotal": 8589934592,
  "memUsed": 5368709120,
  "swapTotal": 8589934592,
  "swapUsed": 1073741824,
  "procs": 150,
  "numCPU": 4,
  "goroutines": 14,
  "uptimeSec": 3600,
  "updatedAt": 1730000000
}
```

| Method | Params | Result |
| ------ | ------ | ------ |
| `sys.stats` | — | `Stats` (single sample) |
| `sys.processes` | `[limit?: number]` (default 8) | `[{pid, name, threads, cpu, memory}]` — sampled machine-wide, sorted by `memory` (bytes) desc, top `limit` kept |
| `sys.series` | `[seconds?: number]` (default 90) | `[{cpu, memory, at}]` — newest samples, oldest first, for chart backfill |
| `app.info` | — | `{framework, version, title, go, perf, frameless, browser:{name,path,kind,version}}` |
| `app.ping` | — | `{t: <server unix ms>}` — the dashboard does `Date.now() - t` for a latency probe |
| `app.echo` | `[any]` | the same JSON value |
| `app.quit` | — | `{status:"ok"}`, then the window closes and the process tears down |
| `win.minimize` | — | `null` |
| `win.maximize` | — | `{maximized: boolean}` (toggles) |
| `win.close` | — | `null` (same exit path as `app.quit`) |
| `win.drag` | `["start" \| "end"]` | `null` — native move loop, Windows only; no-op elsewhere |
| `win.state` | — | `{frameless, maximized, minimized, width, height, left, top}` |

Pushed events:

| Event | Payload | Cadence |
| ----- | ------- | ------- |
| `sys:stats` | `Stats` | every 1 s while ≥1 client is connected |
| `sys:alert` | `{kind:"cpu"\|"mem", level:"warn"\|"crit", value:number, zh:string, en:string}` | on sustained CPU > 85 % or memory > 90 % (3 consecutive samples); deduplicated until the condition clears |

---

## 7. Compatibility notes

* **Windows 7**: Chrome/Edge 109 is the newest engine that runs there. The
  protocol uses no `EventSource`, no `structuredClone`, no top-level `await`,
  no optional chaining in transport code paths, and ES5-compatible syntax for
  `omni.js`, so it survives the older V8 in embedded/legacy Chromium shells
  (360se, QQ browser).
* Anything older than Chromium 60 (IE11, 360 in IE mode) is explicitly
  unsupported; the launcher refuses to open IE and prints a hint instead.

---

## 8. Versioning

`hello.version` follows semver. Additive message types/fields are minor bumps;
renames or semantic changes to existing frames are major bumps.
