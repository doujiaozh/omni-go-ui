/*!
 * Omni-Go-UI client runtime v1.0.0
 * Served by ui_core at /_omni/omni.js. Zero dependencies, no bundler.
 *
 * Protocol (docs/PROTOCOL.md): one WebSocket, JSON text frames.
 *   client -> server {"type":"call","id":N,"method":"sys.stats","params":[...]}
 *   server -> client {"type":"result","id":N,"data":...}
 *   server -> client {"type":"error","id":N,"error":{"code","message"}}
 *   server -> client {"type":"event","event":"sys:stats","data":...}
 *
 * Written conservatively (no optional chaining, no class fields) so it also runs
 * on the older V8 shipped inside Win7-era Chromium shells (360se, QQ Browser).
 */
(function () {
  'use strict';

  var VERSION = '1.0.0';
  var MIN_BACKOFF = 400;
  var MAX_BACKOFF = 5000;

  var ws = null;
  var state = 'connecting'; // 'connecting' | 'connected' | 'disconnected'
  var hello = null;
  var seq = 0;
  var calls = Object.create(null);      // id -> {resolve, reject, method}
  var handlers = Object.create(null);   // event -> [cb]
  var statusListeners = [];             // [{cb}]
  var pending = [];                     // call frames queued before the socket opens
  var backoff = MIN_BACKOFF;
  var reconnectTimer = 0;
  var unloading = false;

  var readyResolve = null;
  var readyPromise = new Promise(function (resolve) { readyResolve = resolve; });

  function socketURL() {
    return (window.location.protocol === 'https:' ? 'wss://' : 'ws://') +
      window.location.host + '/ws';
  }

  function setState(next, detail) {
    if (state === next) return;
    state = next;
    for (var i = 0; i < statusListeners.length; i++) {
      try { statusListeners[i].cb({ state: next, detail: detail || '' }); }
      catch (err) { /* a listener must never break the runtime */ }
    }
  }

  function connect() {
    if (reconnectTimer) { clearTimeout(reconnectTimer); reconnectTimer = 0; }
    try {
      ws = new WebSocket(socketURL());
    } catch (err) {
      scheduleReconnect();
      return;
    }
    ws.onopen = onOpen;
    ws.onmessage = onMessage;
    ws.onclose = onClose;
    ws.onerror = function () { /* onclose always follows; ignore */ };
    setState('connecting');
  }

  function scheduleReconnect() {
    if (unloading) return;
    if (reconnectTimer) clearTimeout(reconnectTimer);
    reconnectTimer = setTimeout(function () {
      backoff = Math.min(MAX_BACKOFF, Math.round(backoff * 1.7));
      connect();
    }, backoff);
  }

  function onOpen() {
    backoff = MIN_BACKOFF;
    if (pending.length) {
      var flushed = pending;
      pending = [];
      for (var i = 0; i < flushed.length; i++) send(flushed[i]);
    }
  }

  function onClose() {
    ws = null;
    // The reply will never arrive: fail every in-flight call with OFFLINE.
    var ids = Object.keys(calls);
    for (var i = 0; i < ids.length; i++) {
      var c = calls[ids[i]];
      delete calls[ids[i]];
      var err = new Error('disconnected before the reply arrived (RPC ' + c.method + ')');
      err.code = 'OFFLINE';
      try { c.reject(err); } catch (e) { /* ignore */ }
    }
    if (!unloading) {
      setState('disconnected', 'socket closed');
      scheduleReconnect();
    }
  }

  function onMessage(ev) {
    var msg;
    try { msg = JSON.parse(typeof ev.data === 'string' ? ev.data : ''); }
    catch (err) { return; }
    if (!msg || typeof msg !== 'object') return;

    switch (msg.type) {
      case 'hello':
        hello = msg;
        setState('connected');
        if (msg.perf && document.documentElement) {
          document.documentElement.dataset.perf = msg.perf;
        }
        if (readyResolve) {
          var resolve = readyResolve;
          readyResolve = null;
          try { resolve(msg); } catch (e) { /* ignore */ }
        }
        emit('omni:hello', msg);
        return;

      case 'result':
        finish(msg.id, msg.data, null);
        return;

      case 'error':
        finish(msg.id, null, msg.error || { code: 'INTERNAL', message: 'unknown error' });
        return;

      case 'event':
        if (msg.event) emit(msg.event, msg.data);
        return;

      case 'pong':
        // A raw transport ping echoed back; app-level latency uses app.ping.
        return;
    }
  }

  function finish(id, data, error) {
    var c = calls[id];
    if (!c) return;
    delete calls[id];
    if (error) {
      var err = new Error(error.message || ('RPC error ' + (error.code || 'INTERNAL')));
      err.code = error.code || 'INTERNAL';
      err.rpc = error;
      try { c.reject(err); } catch (e) { /* ignore */ }
    } else {
      try { c.resolve(data); } catch (e) { /* an app handler may throw; survive */ }
    }
  }

  function emit(event, data) {
    var list = handlers[event];
    if (!list || !list.length) return;
    var copy = list.slice();
    for (var i = 0; i < copy.length; i++) {
      try { copy[i](data, event); }
      catch (err) {
        if (window.console) console.error('omni: handler for "' + event + '" threw', err);
      }
    }
  }

  function send(frame) {
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify(frame));
      return true;
    }
    return false;
  }

  /**
   * omni.call(method, ...params) -> Promise<data>
   * Rejects with an Error carrying .code ('OFFLINE' | 'NOT_FOUND' | ...).
   */
  function call(method) {
    var params = [];
    for (var i = 1; i < arguments.length; i++) params.push(arguments[i]);
    var id = ++seq;
    var frame = { type: 'call', id: id, method: method, params: params };
    return new Promise(function (resolve, reject) {
      calls[id] = { resolve: resolve, reject: reject, method: method };
      if (!send(frame)) pending.push(frame);
    });
  }

  function on(event, cb) {
    if (!event || typeof cb !== 'function') return function () {};
    (handlers[event] || (handlers[event] = [])).push(cb);
    return function off() { remove(handlers[event], cb); };
  }

  function once(event, cb) {
    var off = on(event, function (data, ev) { off(); cb(data, ev); });
    return off;
  }

  function remove(list, cb) {
    if (!list) return;
    var i = list.indexOf(cb);
    if (i >= 0) list.splice(i, 1);
  }

  /** onStatus(cb) -> unsubscribe; cb is called immediately with the current state. */
  function onStatus(cb) {
    if (typeof cb !== 'function') return function () {};
    var entry = { cb: cb };
    statusListeners.push(entry);
    try { cb({ state: state, detail: '' }); } catch (e) { /* ignore */ }
    return function () {
      var i = statusListeners.indexOf(entry);
      if (i >= 0) statusListeners.splice(i, 1);
    };
  }

  function reconnect() {
    backoff = MIN_BACKOFF;
    connect();
  }

  var api = {
    version: VERSION,
    call: call,
    on: on,
    once: once,
    off: function (event, cb) { remove(handlers[event], cb); },
    onStatus: onStatus,
    reconnect: reconnect,
    ready: readyPromise
  };

  // Live views, so `omni.hello` / `omni.state` always reflect the socket.
  Object.defineProperty(api, 'hello', { get: function () { return hello; } });
  Object.defineProperty(api, 'state', { get: function () { return state; } });
  Object.defineProperty(api, 'connected', { get: function () { return state === 'connected'; } });

  window.omni = api;

  if (window.addEventListener) {
    window.addEventListener('beforeunload', function () {
      unloading = true;
      if (ws) { try { ws.close(); } catch (e) { /* ignore */ } }
    });
  }
  connect();
})();
