const prefix = "/__p2page/";
const P = {start:1, data:2, end:3, headers:4, failure:5, credit:6, text:7, binary:8, close:9, cookies:10};
const protocolVersion = 2;
const sessionKey = "__p2page_session_v2";
const returnPathKey = "__p2page_return_path";
const chunk = 32768, windowSize = 32, maxWS = 16 * 1024 * 1024;
const encoder = new TextEncoder(), decoder = new TextDecoder();
const frame = document.getElementById("app");
const panel = document.getElementById("connection");
const orb = document.getElementById("orb");
const retry = document.getElementById("retry");
let connection, control, initialized = false;
let cookieState = {revision:0, cookies:[]};
let cookieID = 0;
const pendingCookies = new Map();

function status(state, detail, failed = false) {
  document.getElementById("state").textContent = state;
  document.getElementById("detail").textContent = detail;
  retry.hidden = !failed;
  orb.textContent = failed ? "!" : state === "Connected" ? "✓" : "···";
  if (failed) setPanel(true);
}
function setPanel(shown) {
  panel.hidden = !shown;
  orb.setAttribute("aria-expanded", String(shown));
}
retry.onclick = () => location.reload();
function appRoute() {
  const current = location.pathname + location.search + location.hash;
  try {
    const pending = JSON.parse(sessionStorage.getItem(returnPathKey));
    sessionStorage.removeItem(returnPathKey);
    const path = pending?.path;
    if (current === "/" && pending?.expires > Date.now() && typeof path === "string" &&
        path.startsWith("/") && !path.startsWith("//") && !/[\\\r\n]/.test(path)) {
      const target = new URL(path, location.origin);
      if (target.origin === location.origin && !target.pathname.startsWith(prefix)) {
        return target.pathname + target.search + target.hash;
      }
    }
  } catch { /* Unavailable storage or an invalid saved route. */ }
  return current;
}
function mirrorRoute(path) {
  if (path.startsWith(prefix)) return;
  // The iframe navigation already creates an entry in the joint browser
  // history. Change that entry's displayed URL without pushing a duplicate.
  if (path !== location.pathname + location.search + location.hash) {
    history.replaceState(history.state, "", location.origin + path);
  }
}
function reportRoute(source) {
  if (source !== frame.contentWindow) return;
  try {
    const current = new URL(source.location.href);
    if (current.origin === location.origin) {
      // Use an absolute same-origin URL, even when the upstream path begins //.
      if (!current.pathname.startsWith(prefix) && current.href !== location.href) {
        history.replaceState(history.state, "", current.href);
      }
    }
  } catch { /* An external navigation is not mirrored into the application URL. */ }
}
window.p2pageRoute = reportRoute;
frame.addEventListener("load", () => {
  const child = frame.contentWindow;
  reportRoute(child);
  try {
    // Catch History API changes even when the upstream's CSP blocks the shim.
    child.navigation?.addEventListener("currententrychange", () => reportRoute(child));
  } catch { /* The frame may have navigated outside this origin. */ }
});
window.addEventListener("hashchange", () => {
  const desired = location.href;
  // A manually edited hash does not reload the top document. Let joint
  // history traversals finish first, then propagate any remaining change.
  setTimeout(() => {
    try {
      const child = frame.contentWindow;
      const current = new URL(child.location.href);
      const next = new URL(desired);
      if (location.href === desired && current.origin === next.origin &&
          current.pathname === next.pathname && current.search === next.search && current.hash !== next.hash) {
        child.location.replace(next.href);
      }
    } catch { /* Ignore external frame navigations. */ }
  }, 0);
});
let drag;
orb.onpointerdown = event => {
  const rect = orb.getBoundingClientRect();
  drag = {id:event.pointerId, x:event.clientX, y:event.clientY, left:rect.left, top:rect.top, moved:false};
  orb.setPointerCapture(event.pointerId);
};
orb.onpointermove = event => {
  if (!drag || drag.id !== event.pointerId) return;
  const dx = event.clientX - drag.x, dy = event.clientY - drag.y;
  if (!drag.moved && Math.hypot(dx,dy) < 4) return;
  drag.moved = true;
  orb.style.left = Math.max(0, Math.min(innerWidth - orb.offsetWidth, drag.left + dx)) + "px";
  orb.style.top = Math.max(0, Math.min(innerHeight - orb.offsetHeight, drag.top + dy)) + "px";
};
orb.onclick = event => {
  if (!drag?.moved || event.detail === 0) setPanel(panel.hidden);
  drag = null;
};
orb.onpointercancel = () => { drag = null; };

function transportOpen() { return control?.readyState === "open" && connection?.connectionState === "connected"; }
function open() { return initialized && transportOpen(); }
async function checked(response) {
  if (!response.ok) throw new Error((await response.text()).trim() || `HTTP ${response.status}`);
  return response;
}
function timeout(ms, message) {
  let timer;
  return {promise:new Promise((_, reject) => { timer = setTimeout(() => reject(new Error(message)), ms); }),
    stop:() => clearTimeout(timer)};
}
async function until(test, target, event, ms, message) {
  if (test()) return;
  let listener;
  const limit = timeout(ms, message);
  try {
    await Promise.race([limit.promise, new Promise(resolve => {
      listener = () => { if (test()) resolve(); };
      target.addEventListener(event, listener);
      listener();
    })]);
  } finally { limit.stop(); target.removeEventListener(event, listener); }
}
async function connect() {
  if (!navigator.serviceWorker || !window.RTCPeerConnection) throw new Error("a browser with Service Workers and WebRTC on HTTPS is required");
  const settings = JSON.parse(frame.dataset.settings);
  const initialPath = appRoute();
  mirrorRoute(initialPath);
  const session = browserSession();
  const registration = navigator.serviceWorker.register(prefix + "worker.js", {scope:"/"});
  registration.catch(() => {}); // Report the original error when awaiting below.
  connection = new RTCPeerConnection({iceServers:settings.ice});
  control = connection.createDataChannel("control");
  control.binaryType = "arraybuffer";
  const peerEvents = new EventTarget();
  const peerChanged = () => peerEvents.dispatchEvent(new Event("change"));
  connection.addEventListener("connectionstatechange", peerChanged);
  control.addEventListener("open", peerChanged);
  control.addEventListener("close", peerChanged);
  control.onclose = () => {
    settleCookieWrites();
    status("Disconnected", "The WebRTC connection ended.", true);
  };
  control.onmessage = event => {
    try {
      const data = new Uint8Array(event.data);
      if (data[0] !== P.cookies) throw new Error("invalid control frame");
      const message = JSON.parse(decoder.decode(data.subarray(1)));
      if (message.type === "ready" && !initialized) {
        updateCookies(message.state);
        initialized = true;
        peerChanged();
      } else if (message.type === "ack") {
        updateCookies(message.state);
        finishCookieWrite(message.id);
      }
    } catch { control.close(); }
  };
  window.addEventListener("storage", event => {
    if (event.storageArea === localStorage && (event.key === null || (event.key === sessionKey && event.newValue !== session))) {
      connection.close();
    }
  });
  connection.onconnectionstatechange = () => {
    switch (connection.connectionState) {
      case "failed":
      case "closed":
        status("Disconnected", "The WebRTC connection failed or ended.", true);
        break;
      case "disconnected":
        status("Interrupted", "Waiting for the network to recover…", true);
        break;
      case "connected":
        if (open()) showConnected().catch(() => status("Connected","WebRTC is connected."));
        break;
    }
  };
  await connection.setLocalDescription(await connection.createOffer());
  await until(() => connection.iceGatheringState === "complete", connection, "icegatheringstatechange", 18000, "ICE gathering timed out. Check STUN/TURN settings.");
  const result = await checked(await fetch(prefix + "offer", {
    method:"POST", headers:{"Content-Type":"application/json"}, body:JSON.stringify({sdp:connection.localDescription.sdp, version:protocolVersion})
  })).then(r => r.json());
  await connection.setRemoteDescription({type:"answer", sdp:result.sdp});
  const failure = settings.mode === "stun" ? "Direct P2P failed (STUN-only; no TURN fallback)." :
    "WebRTC failed. Check TURN reachability and credentials.";
  await until(() => transportOpen() || ["failed","closed"].includes(connection.connectionState) || control.readyState === "closed",
    peerEvents, "change", 28000, failure);
  if (!transportOpen()) throw new Error(failure);
  control.send(JSON.stringify({type:"open", session}));
  const sessionFailure = "The Client did not open the application session. Check the Client version and retry.";
  await until(() => open() || control.readyState === "closed", peerEvents, "change", 10000, sessionFailure);
  if (!open()) throw new Error(sessionFailure);
  const sw = await registration;
  await until(() => !!navigator.serviceWorker.controller, navigator.serviceWorker, "controllerchange", 8000, "Service Worker did not take control.");
  // A hard reload can bypass Service Worker interception for the outer page.
  if (!sw.active) throw new Error("Service Worker installation failed");
  frame.src = location.origin + initialPath;
  setPanel(false);
  await showConnected();
}
async function showConnected() {
  let route = "WebRTC data channel";
  const stats = await connection.getStats();
  for (const stat of stats.values()) {
    if (stat.type !== "transport" || !stat.selectedCandidatePairId) continue;
    const pair = stats.get(stat.selectedCandidatePairId);
    if (!pair) continue;
    const types = [pair.localCandidateId,pair.remoteCandidateId].map(id => stats.get(id)?.candidateType);
    const relayed = types.includes("relay");
    route = relayed ? "TURN relay" : "Direct P2P";
    if (types.every(Boolean)) route += " · " + types.join(" ↔ ");
    if (Number.isFinite(pair.currentRoundTripTime)) {
      const ms = pair.currentRoundTripTime * 1000;
      route += " · ICE RTT " + (ms < 1 ? "<1" : Math.round(ms)) + " ms";
    }
    break;
  }
  if (open()) status("Connected", route);
}

function send(dc, kind, payload = new Uint8Array()) {
  const data = new Uint8Array(payload.length + 1);
  data[0] = kind;
  data.set(payload, 1);
  dc.send(data);
}
function json(dc, kind, data) {
  const b = encoder.encode(JSON.stringify(data));
  if (b.length >= 60 * 1024) throw new Error("request metadata too large");
  send(dc, kind, b);
}
function browserSession() {
  const bytes = crypto.getRandomValues(new Uint8Array(32));
  const fresh = btoa(String.fromCharCode(...bytes)).replaceAll("+","-").replaceAll("/","_").replace(/=$/,"");
  try {
    const stored = localStorage.getItem(sessionKey);
    if (stored && /^[A-Za-z0-9_-]{42}[AEIMQUYcgkosw048]$/.test(stored)) return stored;
    localStorage.setItem(sessionKey, fresh);
  } catch { /* Storage unavailable: this tab uses an ephemeral session. */ }
  return fresh;
}

function updateCookies(state) {
  if (!state || !Array.isArray(state.cookies) || !Number.isSafeInteger(state.revision)) throw new Error("invalid cookie snapshot");
  if (state.revision >= cookieState.revision) cookieState = state;
}
function finishCookieWrite(id) {
  const write = pendingCookies.get(id);
  if (write) {
    pendingCookies.delete(id);
    clearTimeout(write.timer);
    write.resolve();
  }
}
function settleCookieWrites() {
  for (const id of pendingCookies.keys()) finishCookieWrite(id);
}
async function waitForCookieWrites() {
  if (pendingCookies.size) await Promise.all([...pendingCookies.values()].map(write => write.done));
}
function cookiePathMatches(path, scope) {
  return path === scope || path.startsWith(scope.endsWith("/") ? scope : scope + "/");
}
function previewCookie(path, line) {
  const [pair, ...attributes] = line.split(";");
  const index = pair.indexOf("=");
  if (index <= 0) return;
  const name = pair.slice(0,index).trim(), value = pair.slice(index+1).trim();
  if (!/^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/.test(name) || /^__host-p2page-/i.test(name)) return;
  const opts = new Map(attributes.map(attr => {
    const eq = attr.indexOf("=");
    return eq < 0 ? [attr.trim().toLowerCase(),""] : [attr.slice(0,eq).trim().toLowerCase(),attr.slice(eq+1).trim()];
  }));
  if (opts.has("httponly") || (name.startsWith("__Secure-") && !opts.has("secure")) ||
      (name.startsWith("__Host-") && (!opts.has("secure") || opts.has("domain") || opts.get("path") !== "/"))) return;
  const scope = opts.get("path")?.startsWith("/") ? opts.get("path") : path.slice(0,path.lastIndexOf("/")) || "/";
  const age = opts.get("max-age");
  const expires = age && /^-?\d+$/.test(age) ? Date.now() + Number(age) * 1000 : Date.parse(opts.get("expires")) || 0;
  return {name, value, path:scope, expires};
}
window.p2pageCookies = {
  read(path) {
    const entries = new Map(cookieState.cookies.map(c => [c.name + "\0" + c.path,c]));
    for (const {preview} of pendingCookies.values()) if (preview) entries.set(preview.name + "\0" + preview.path,preview);
    return [...entries.values()].filter(c => (!c.expires || c.expires > Date.now()) && cookiePathMatches(path,c.path))
      .sort((a,b) => b.path.length - a.path.length).map(c => `${c.name}=${c.value}`).join("; ");
  },
  write(path, value) {
    const line = String(value);
    if (!open() || encoder.encode(line).length > 4096 || pendingCookies.size >= 32) return;
    const id = ++cookieID;
    const payload = JSON.stringify({type:"set",id,path,line});
    if (encoder.encode(payload).length > 8192) return;
    let resolve;
    const done = new Promise(r => { resolve = r; });
    const timer = setTimeout(() => finishCookieWrite(id), 5000);
    pendingCookies.set(id,{done,resolve,timer,preview:previewCookie(path,line)});
    try { control.send(payload); }
    catch { finishCookieWrite(id); }
  }
};

// Asset bursts can easily exceed the Client's 64 active channels per peer.
// Queue them before opening a channel, leaving room for SCTP channel teardown
// and giving long-lived streams their own budgets.
function channelSlots(limit, maxQueued) {
  let active = 0;
  const queue = new Set();
  function reserve() {
    active++;
    let released = false;
    return () => {
      if (released) return;
      released = true;
      active--;
      queue.values().next().value?.();
    };
  }
  return async function acquire(signal) {
    if (signal.aborted) throw signal.reason;
    if (active < limit) return reserve();
    if (queue.size >= maxQueued) throw new Error("too many queued WebRTC requests");
    return new Promise((resolve, reject) => {
      const remove = () => {
        queue.delete(start);
        signal.removeEventListener("abort", cancel);
      };
      const start = () => { remove(); resolve(reserve()); };
      const cancel = () => { remove(); reject(signal.reason); };
      queue.add(start);
      signal.addEventListener("abort", cancel, {once:true});
    });
  };
}
const httpSlots = channelSlots(32, 256);
const eventSlots = channelSlots(8, 32);
const socketSlots = channelSlots(16, 16);

function channel() {
  if (!open()) throw new Error("WebRTC connection unavailable");
  const dc = connection.createDataChannel("request");
  dc.binaryType = "arraybuffer";
  dc.bufferedAmountLowThreshold = 256 * 1024;
  return dc;
}
async function writable(dc) {
  if (dc.readyState !== "open") throw new Error("data channel closed");
  if (dc.bufferedAmount <= 1024 * 1024) return;
  await until(() => dc.bufferedAmount <= dc.bufferedAmountLowThreshold, dc, "bufferedamountlow", 20000, "data channel stalled");
}

navigator.serviceWorker?.addEventListener("message", event => {
  const port = event.ports[0];
  if (!port) return;
  if (event.data?.type === "probe") {
    if (open()) port.postMessage("ready");
    port.close();
  } else if (event.data?.type === "http") {
    serveHTTP(event.data.metadata, event.data.body, port);
  }
});

function serveHTTP(req, body, port) {
  let dc, reader, controller, finished = false, headersSent = false, outstanding = 0, uploadCredits = 0, uploadWake;
  const abort = new AbortController();
  let release;
  function finish(error) {
    if (finished) return;
    finished = true;
    abort.abort();
    if (!headersSent) {
      port.postMessage({error: error?.message || "HTTP stream closed before headers"});
      body?.cancel().catch(() => {});
    } else if (controller) {
      if (error) controller.error(error);
      else controller.close();
    }
    if (reader) reader.cancel().catch(() => {});
    uploadWake?.();
    dc?.close();
    release?.();
    port.close();
  }
  function credit() {
    if (finished || dc?.readyState !== "open" || !controller) return;
    const n = Math.max(0, Math.min(windowSize, Math.floor(controller.desiredSize || 0)) - outstanding);
    if (n) {
      outstanding += n;
      try { send(dc, P.credit, Uint8Array.of(n)); } catch (err) { finish(err); }
    }
  }
  port.onmessage = () => finish(new Error("request canceled"));
  const stream = new ReadableStream({
    start(c) { controller = c; }, pull: credit, cancel: () => {
      controller = undefined;
      finish(new Error("response canceled"));
    }
  }, {highWaterMark:windowSize});
  async function upload() {
    reader = body.getReader();
    while (!finished) {
      const {value, done} = await reader.read();
      if (finished) return;
      if (done) { send(dc, P.end); return; }
      for (let off = 0; off < value.length && !finished; off += chunk) {
        while (!uploadCredits && !finished) await new Promise(resolve => { uploadWake = resolve; });
        if (finished) return;
        uploadCredits--;
        await writable(dc);
        send(dc, P.data, value.subarray(off, off+chunk));
      }
    }
  }
  async function start() {
    if (req.credentials && pendingCookies.size) await waitForCookieWrites();
    if (finished) return;
    const eventStream = req.headers.accept?.some(value => value.includes("text/event-stream"));
    release = await (eventStream ? eventSlots : httpSlots)(abort.signal);
    if (finished) { release(); return; }
    dc = channel();
    dc.onclose = () => finish(new Error("data channel closed"));
    dc.onerror = () => finish(new Error("data channel error"));
    dc.onopen = () => {
      try {
        req.headers["user-agent"] ??= [navigator.userAgent];
        json(dc, P.start, {...req, cookieRevision:cookieState.revision}); credit(); if (body) upload().catch(finish);
      }
      catch (err) { finish(err); }
    };
    // Keep state updates ordered with headers and redirect responses.
    let queue = Promise.resolve();
    dc.onmessage = event => {
      const bytes = new Uint8Array(event.data), kind = bytes[0], payload = bytes.subarray(1);
      queue = queue.then(async () => {
        if (finished) return;
        switch (kind) {
          case P.credit:
            uploadCredits += payload[0];
            uploadWake?.(); uploadWake = null;
            break;
          case P.headers: {
            const info = JSON.parse(decoder.decode(payload));
            const canStream = req.method !== "HEAD" && ![204,205,304].includes(info.status);
            port.postMessage({status:info.status, headers:info.headers, stream:canStream ? stream : null}, canStream ? [stream] : []);
            headersSent = true;
            if (!canStream) finish();
            break;
          }
          case P.cookies: updateCookies(JSON.parse(decoder.decode(payload))); break;
          case P.data:
            if (!headersSent || --outstanding < 0) throw new Error("invalid response frame");
            controller.enqueue(payload);
            credit();
            break;
          case P.end: finish(); break;
          case P.failure: throw new Error(decoder.decode(payload));
          default: throw new Error("invalid response frame");
        }
      }).catch(finish);
    };
    dc.onclose = () => { queue.then(() => finish(new Error("data channel closed"))); };
  }
  start().catch(finish);
}

// Used by the injected WebSocket compatibility shim in same-origin frames.
window.p2pageSocket = function(path, protocols, notify) {
  let dc, closed = false, total = 0, pieces = [], queued = 0, outbound = Promise.resolve();
  let remote = Promise.resolve();
  const abort = new AbortController();
  let release;
  const finish = (code = 1006, reason = "") => {
    if (closed) return;
    closed = true;
    abort.abort();
    dc?.close();
    release?.();
    notify("close", {code, reason});
  };
  (async () => {
    if (pendingCookies.size) await waitForCookieWrites();
    if (closed) return;
    // Native WebSocket has no request timeout; don't leave CONNECTING forever
    // if the existing sockets keep every slot occupied.
    const timer = setTimeout(() => abort.abort(), 15000);
    try { release = await socketSlots(abort.signal); }
    finally { clearTimeout(timer); }
    if (closed) { release(); return; }
    dc = channel();
    dc.onopen = () => {
      try { json(dc, P.start, {method:"GET", path, headers:{}, credentials:true, cookieRevision:cookieState.revision, ws:true, protocols}); }
      catch { finish(); }
    };
    dc.onerror = () => finish();
    dc.onclose = () => { remote.then(() => finish()); };
    dc.onmessage = event => {
      const b = new Uint8Array(event.data), kind = b[0], data = b.subarray(1);
      remote = remote.then(async () => {
        if (closed) return;
        if (kind === P.headers) {
          const info = JSON.parse(decoder.decode(data));
          if (!closed) notify("open",info.protocol || "");
        } else if (kind === P.cookies) {
          updateCookies(JSON.parse(decoder.decode(data)));
        } else if (kind === P.data || kind === P.text || kind === P.binary) {
          total += data.length;
          if (total > maxWS) throw new Error("WebSocket message too large");
          pieces.push(data);
          if (kind !== P.data) {
            const out = new Uint8Array(total);
            let offset = 0;
            for (const piece of pieces) { out.set(piece, offset); offset += piece.length; }
            total = 0; pieces = [];
            notify("message", kind === P.text ? decoder.decode(out) : out.buffer);
          }
        } else if (kind === P.close) {
          const end = JSON.parse(decoder.decode(data)); finish(end.code,end.reason);
        } else { finish(); }
      }).catch(() => finish());
    };
  })().catch(() => finish());
  return {
    buffered: () => queued + (dc?.bufferedAmount || 0),
    send(value, text) {
      const size = value.size ?? value.byteLength;
      if (size > maxWS || queued + size > 2 * maxWS) { finish(1009,"Message or send buffer too large"); return; }
      queued += size;
      outbound = outbound.then(async () => {
        const b = new Uint8Array(value.arrayBuffer ? await value.arrayBuffer() : value);
        let offset = 0;
        while (b.length - offset > chunk) {
          await writable(dc);
          send(dc, P.data, b.subarray(offset, offset+chunk));
          offset += chunk;
        }
        await writable(dc);
        send(dc, text ? P.text : P.binary, b.subarray(offset));
      }).catch(() => finish()).finally(() => { queued -= size; });
    },
    close(code, reason) {
      if (dc?.readyState !== "open") { finish(); return; }
      outbound.then(() => { if (!closed) json(dc,P.close,{code,reason}); }).catch(() => finish());
      setTimeout(() => finish(), 6000);
    }
  };
};
window.addEventListener("pagehide", () => {
  control?.close();
  connection?.close();
});
window.addEventListener("pageshow", event => { if (event.persisted) location.reload(); });
connect().catch(err => {
  if (connection) {
    connection.onconnectionstatechange = null;
    if (control) control.onclose = null;
    connection.close();
  }
  status("Connection failed", err.message, true);
});
