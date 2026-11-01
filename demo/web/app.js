/* ==========================================================================
   Omni System Monitor — dashboard logic
   Vanilla ES5 (Win7-era Chromium safe: no optional chaining, no class
   fields, no template literals). Talks to the Go backend only through
   window.omni, which /_omni/omni.js provides. No inline handlers, no
   external URLs, nothing eval-shaped — the CSP forbids all three.
   ========================================================================== */
(function () {
  'use strict';

  var HISTORY = 90;         // chart points, matches the 90-second window
  var PROC_REFRESH_MS = 4000;
  var PING_MS = 5000;
  var MAX_TOASTS = 3;
  var TOAST_MS = 6000;
  var GAUGE_R = 70;
  var GAUGE_C = 2 * Math.PI * GAUGE_R;   // ≈ 439.82

  var $ = function (id) { return document.getElementById(id); };

  var els = {};
  ['card-cpu', 'm-cpu', 'm-cpus', 'm-mem', 'm-mem-pct', 'm-mem-total',
   'm-uptime', 'm-goroutines', 'm-go', 'lg-cpu', 'lg-mem', 'chart',
   'g-chip', 'gauge', 'gauge-arc', 'gauge-value', 'g-core', 'g-load', 'g-mem',
   'proc-count', 'proc-list', 'btn-refresh',
   'status-pill', 'status-zh', 'status-en', 'btn-pause', 'pause-zh', 'pause-en',
   'win-controls', 'win-min', 'win-max', 'win-close',
   'st-conn', 'st-conn-zh', 'st-conn-en', 'st-rpc', 'st-events', 'st-frames',
   'toasts'].forEach(function (id) {
    var key = id.replace(/-(\w)/g, function (_, c) { return c.toUpperCase(); });
    els[key] = $(id);
  });

  var state = {
    paused: false,
    events: 0,
    frames: 0,
    cpuSeries: [],        // oldest → newest, capped at HISTORY
    memSeries: [],
    lastCpu: null,        // for the trend arrow on the CPU card
    gaugeOk: false
  };

  // ── formatting helpers ────────────────────────────────────────────────
  function clampPct(v) { v = +v; if (!isFinite(v)) return 0; if (v < 0) return 0; if (v > 100) return 100; return v; }

  function fmtPct(v, digits) {
    if (digits === undefined) digits = 1;
    if (!isFinite(+v)) return '--';
    return (+v).toFixed(digits);
  }

  function fmtBytes(bytes) {                    // → human GiB/MiB
    var b = +bytes;
    if (!isFinite(b) || b < 0) return '--';
    if (b >= 1024 * 1024 * 1024) return (b / (1024 * 1024 * 1024)).toFixed(2);
    return (b / (1024 * 1024)).toFixed(0);
  }

  function fmtUptime(sec) {                     // → "1d 02:03:04"
    var s = Math.floor(+sec || 0);
    var d = Math.floor(s / 86400); s -= d * 86400;
    var h = Math.floor(s / 3600); s -= h * 3600;
    var m = Math.floor(s / 60); s -= m * 60;
    function p(n) { return (n < 10 ? '0' : '') + n; }
    return (d > 0 ? d + 'd ' : '') + p(h) + ':' + p(m) + ':' + p(s);
  }

  function setText(el, text) { if (el && el.textContent !== text) el.textContent = text; }

  // ── connection status: the pill and the status bar move together ──────
  var STATUS_TEXT = {
    connecting: ['连接中', 'Connecting'],
    connected: ['已连接', 'Online'],
    disconnected: ['已断开', 'Offline'],
    // aliases kept for hand-rolled callers
    online: ['已连接', 'Online'],
    offline: ['已断开', 'Offline'],
    reconnecting: ['重连中', 'Reconnecting']
  };

  function setStatus(kind) {
    var t = STATUS_TEXT[kind] || STATUS_TEXT.disconnected;
    if (els.statusPill) els.statusPill.setAttribute('data-state', kind === 'reconnecting' ? 'connecting' : kind);
    if (els.stConn) els.stConn.setAttribute('data-state', kind === 'reconnecting' ? 'connecting' : kind);
    setText(els.statusZh, t[0]);
    setText(els.statusEn, t[1]);
    setText(els.stConnZh, t[0]);
    setText(els.stConnEn, t[1]);
    if (els.stRpc && kind !== 'online') setText(els.stRpc, '—');
  }

  // ── gauge: the arc is one stroke-dashoffset tween per second ──────────
  function paintGauge(pct) {
    if (!els.gaugeArc) return;
    if (!state.gaugeOk) {
      els.gaugeArc.style.strokeDasharray = GAUGE_C.toFixed(2) + ' ' + GAUGE_C.toFixed(2);
      state.gaugeOk = true;
    }
    var p = clampPct(pct) / 100;
    els.gaugeArc.style.strokeDashoffset = (GAUGE_C * (1 - p)).toFixed(2);
    if (els.gauge) els.gauge.setAttribute('aria-label', 'CPU 使用率 ' + fmtPct(pct) + '%');
  }

  // ── rolling chart: grid + two polylines, device pixels ────────────────
  function setupCanvas() {
    var cv = els.chart;
    if (!cv) return null;
    var dpr = window.devicePixelRatio || 1;
    var w = cv.clientWidth, h = cv.clientHeight;
    if (w < 10 || h < 10) return null;
    if (cv.width !== Math.round(w * dpr) || cv.height !== Math.round(h * dpr)) {
      cv.width = Math.round(w * dpr);
      cv.height = Math.round(h * dpr);
    }
    var ctx = cv.getContext('2d');
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    return { ctx: ctx, w: w, h: h };
  }

  function cssVar(name, fallback) {
    var v = getComputedStyle(document.documentElement).getPropertyValue(name);
    v = v ? v.trim() : '';
    return v || fallback;
  }

  function paintChart() {
    var view = setupCanvas();
    if (!view) return;
    var ctx = view.ctx, w = view.w, h = view.h;
    var padL = 34, padR = 8, padT = 10, padB = 16;
    var iw = w - padL - padR, ih = h - padT - padB;
    ctx.clearRect(0, 0, w, h);

    ctx.strokeStyle = cssVar('--line', 'rgba(125,140,170,.14)');
    ctx.lineWidth = 1;
    ctx.font = '9px ' + cssVar('--mono', 'ui-monospace, monospace');
    ctx.fillStyle = cssVar('--text-3', '#5a6573');
    ctx.textAlign = 'left';
    for (var i = 0; i <= 4; i++) {              // gridlines at 0/25/50/75/100
      var y = padT + ih * i / 4;
      ctx.beginPath(); ctx.moveTo(padL, y); ctx.lineTo(w - padR, y); ctx.stroke();
      ctx.fillText(String(100 - i * 25), 6, y + 3);
    }

    function line(series, color, fill) {
      if (!series || series.length < 2) return;
      var step = iw / (HISTORY - 1);
      var fit = series.length > HISTORY ? series.length - HISTORY : 0;
      ctx.beginPath();
      for (var j = fit; j < series.length; j++) {
        var x = padL + (j - fit) * step;
        var y = padT + ih * (1 - clampPct(series[j]) / 100);
        if (j === fit) ctx.moveTo(x, y); else ctx.lineTo(x, y);
      }
      if (fill) {
        ctx.save();
        ctx.lineTo(padL + (series.length - 1 - fit) * step, padT + ih);
        ctx.lineTo(padL, padT + ih);
        ctx.closePath();
        ctx.fillStyle = fill;
        ctx.fill();
        ctx.restore();
        ctx.beginPath();                        // re-trace the stroke path
        for (j = fit; j < series.length; j++) {
          x = padL + (j - fit) * step;
          y = padT + ih * (1 - clampPct(series[j]) / 100);
          if (j === fit) ctx.moveTo(x, y); else ctx.lineTo(x, y);
        }
      }
      ctx.strokeStyle = color;
      ctx.lineWidth = 1.6;
      ctx.lineJoin = 'round';
      ctx.stroke();
    }

    line(state.cpuSeries, cssVar('--accent', '#35e0c9'), 'rgba(53,224,201,.08)');
    line(state.memSeries, cssVar('--accent-2', '#7c6cff'), 'rgba(124,108,255,.08)');
  }

  function pushSeries(cpu, mem) {
    state.cpuSeries.push(clampPct(cpu));
    state.memSeries.push(clampPct(mem));
    if (state.cpuSeries.length > HISTORY) state.cpuSeries.shift();
    if (state.memSeries.length > HISTORY) state.memSeries.shift();
  }

  // ── one sys:stats frame → every number on the dashboard ───────────────
  function applyStats(s) {
    if (!s) return;
    var cpu = clampPct(s.cpu);
    var mem = clampPct(s.memory);

    if (els.cardCpu) {
      var trend = 'flat';
      if (state.lastCpu !== null) {
        if (cpu > state.lastCpu + 1.5) trend = 'up';
        else if (cpu < state.lastCpu - 1.5) trend = 'down';
      }
      els.cardCpu.setAttribute('data-trend', trend);
    }
    state.lastCpu = cpu;

    setText(els.mCpu, fmtPct(cpu));
    setText(els.mCpus, s.numCPU != null ? String(s.numCPU) : '--');
    setText(els.mMem, fmtBytes(s.memUsed));
    setText(els.mMemPct, fmtPct(mem, 0) + '%');
    if (els.mMemPct) {
      els.mMemPct.style.color = mem > 90 ? cssVar('--crit', '#ff5d73') : '';
    }
    setText(els.mMemTotal, fmtBytes(s.memTotal));
    setText(els.mUptime, fmtUptime(s.uptimeSec));
    setText(els.mGoroutines, s.goroutines != null ? String(s.goroutines) : '--');
    setText(els.lgCpu, fmtPct(cpu) + '%');
    setText(els.lgMem, fmtPct(mem) + '%');

    paintGauge(cpu);
    setText(els.gaugeValue, fmtPct(cpu));
    setText(els.gCore, s.numCPU != null ? String(s.numCPU) : '--');
    setText(els.gLoad, fmtPct(cpu));
    setText(els.gMem, fmtPct(mem, 0) + '%');

    pushSeries(cpu, mem);
    paintChart();
  }

  // ── top processes ─────────────────────────────────────────────────────
  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"]/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c];
    });
  }

  function applyProcesses(list, total) {
    if (!els.procList) return;
    var rows = Array.isArray(list) ? list : [];
    var html = '';
    for (var i = 0; i < rows.length; i++) {
      var p = rows[i] || {};
      var cpu = clampPct(p.cpu);
      var hot = cpu > 60;
      var memPct = p.memory && total ? clampPct(p.memory / total * 100) : 0;
      html +=
        '<div class="proc-row' + (hot ? ' hot' : '') + '" role="listitem">' +
          '<span class="proc-name"><span class="nm">' + esc(p.name) + '</span></span>' +
          '<span class="proc-num ta-c">' + esc(p.pid) + '</span>' +
          '<span class="proc-num ta-c">' + esc(p.threads != null ? p.threads : '—') + '</span>' +
          '<span class="proc-pct"><span class="proc-bar"><i style="width:' + Math.round(memPct) + '%"></i></span>' +
            '<b class="num pct-clr">' + fmtPct(cpu) + '%</b></span>' +
          '<span class="proc-num ta-r">' + fmtBytes(p.memory) + '</span>' +
        '</div>';
    }
    els.procList.innerHTML = html;
    setText(els.procCount, total != null ? String(total) : String(rows.length));
  }

  function refreshProcesses() {
    omni.call('sys.processes', 7).then(function (res) {
      var list = res, total;
      if (res && Array.isArray(res.list)) { list = res.list; total = res.total; }
      if (res && typeof res.total === 'number' && Array.isArray(list)) total = res.total;
      if (total == null && window.__omniLastStats && window.__omniLastStats.procs != null) {
        total = window.__omniLastStats.procs;
      }
      applyProcesses(list, total);
    }, function () { /* next tick retries */ });
  }

  // ── toasts: at most three, each self-dismissing ───────────────────────
  function toast(alert) {
    var box = els.toasts;
    if (!box || !alert) return;
    while (box.children.length >= MAX_TOASTS) box.removeChild(box.firstChild);

    var el = document.createElement('div');
    el.className = 'toast';
    el.setAttribute('level', alert.level || 'info');
    var zh = alert.zh || alert.title || (' Alert: ' + fmtPct(alert.value));
    var en = alert.en || alert.kind || '';
    el.innerHTML =
      '<span class="t-ico" aria-hidden="true"></span>' +
      '<span class="t-zh"></span><span class="t-en"></span>';
    el.querySelector('.t-zh').textContent = zh;
    el.querySelector('.t-en').textContent = en;
    box.appendChild(el);

    setTimeout(function () {
      el.classList.add('leaving');
      setTimeout(function () { if (el.parentNode) el.parentNode.removeChild(el); }, 320);
    }, TOAST_MS);
  }

  // ── RPC latency probe: half round-trip on the server's own clock ──────
  function pingLoop() {
    function once() {
      var t0 = Date.now();
      omni.call('app.ping').then(function (res) {
        var t1 = Date.now();
        if (els.stRpc) {
          var server = res && res.t;
          var ms = isFinite(+server) ? Math.max(0, t1 - (+server)) : (t1 - t0) / 2;
          setText(els.stRpc, ms.toFixed(0) + ' ms');
        }
      }, function () { if (els.stRpc) setText(els.stRpc, '—'); });
    }
    once();
    setInterval(once, PING_MS);
  }

  // ── window chrome: only wired when the backend advertises it ──────────
  function wireWindowControls(features) {
    var canFrame = features && features.indexOf('frameless') !== -1;
    if (els.winControls) els.winControls.hidden = !canFrame;
    if (!canFrame) return;

    if (els.winMin) els.winMin.addEventListener('click', function () { omni.call('win.minimize'); });
    if (els.winMax) els.winMax.addEventListener('click', function () { omni.call('win.maximize'); });
    if (els.winClose) els.winClose.addEventListener('click', function () { omni.call('win.close'); });

    // drag: mousedown on the titlebar (but never on a button) starts a
    // native move; the whole loop lives in Go, we only open and close it.
    var bar = document.querySelector('.titlebar[data-drag]');
    if (!bar) return;
    bar.addEventListener('mousedown', function (ev) {
      if (ev.button !== 0) return;
      if (ev.target.closest('button')) return;
      omni.call('win.drag', 'start');
      var up = function () {
        window.removeEventListener('mouseup', up);
        omni.call('win.drag', 'end');
      };
      window.addEventListener('mouseup', up);
    });
  }

  // ── pause: front-end-side freeze; the sampler keeps broadcasting ──────
  function wirePause() {
    if (!els.btnPause) return;
    els.btnPause.addEventListener('click', function () {
      state.paused = !state.paused;
      els.btnPause.setAttribute('aria-pressed', state.paused ? 'true' : 'false');
      setText(els.pauseZh, state.paused ? '继续' : '暂停');
      setText(els.pauseEn, state.paused ? 'Resume' : 'Pause');
      if (els.btnPause) els.btnPause.blur();
    });
  }

  // ── boot ──────────────────────────────────────────────────────────────
  function boot() {
    wirePause();
    wireWindowControls(omni.hello && omni.hello.features);
    paintChart();

    omni.onStatus(function (info) {
      // omni.js hands status listeners { state, detail }; stay tolerant of
      // either shape so the pill never renders "[object Object]".
      var kind = info && info.state ? info.state : String(info);
      setStatus(kind);
    });

    omni.on('omni:hello', function () {
      wireWindowControls(omni.hello && omni.hello.features);
    });

    omni.on('sys:stats', function (s) {
      state.events++;
      state.frames++;
      window.__omniLastStats = s;
      if (!state.paused) applyStats(s);
      if (els.stEvents) setText(els.stEvents, String(state.events));
      if (els.stFrames) setText(els.stFrames, String(state.frames));
    });

    omni.on('sys:alert', function (a) {
      state.events++;
      if (els.stEvents) setText(els.stEvents, String(state.events));
      toast(a);
    });

    // warm-up: real numbers now, history now, then the 1s stream takes over
    omni.call('sys.stats').then(applyStats, function () {});
    omni.call('sys.series', HISTORY).then(function (samples) {
      if (!samples || !samples.length) return;
      state.cpuSeries = [];
      state.memSeries = [];
      for (var i = 0; i < samples.length; i++) {
        state.cpuSeries.push(clampPct(samples[i].cpu));
        state.memSeries.push(clampPct(samples[i].memory));
      }
      paintChart();
    }, function () {});

    if (els.btnRefresh) els.btnRefresh.addEventListener('click', refreshProcesses);
    refreshProcesses();
    setInterval(refreshProcesses, PROC_REFRESH_MS);
    pingLoop();

    window.addEventListener('resize', paintChart);
    document.addEventListener('visibilitychange', function () {
      if (!document.hidden) paintChart();
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', boot);
  } else {
    boot();
  }
})();
