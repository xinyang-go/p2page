"use strict";
const prefix = "/__p2page/";
self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", event => event.waitUntil(self.clients.claim()));

self.addEventListener("fetch", event => {
  const req = event.request;
  const url = new URL(req.url);
  if (url.origin !== self.location.origin || url.pathname.startsWith(prefix)) return;
  const top = req.mode === "navigate" && req.destination === "document";
  if (top && req.method !== "GET") return;
  const response = top ? loadShell() : route(req, event.clientId);
  event.respondWith(response.catch(err =>
    new Response("p2page: " + err.message, {
      status: 502, headers: {"Content-Type": "text/plain; charset=utf-8", "Cache-Control": "no-store"}
    })));
});

function loadShell() {
  // Keep the navigation URL in the browser. The public server only serves the
  // fixed bootstrap document, even when refreshing a deep app route.
  return fetch(prefix + "shell", {
    credentials:"same-origin", cache:"no-store", redirect:"error", referrerPolicy:"no-referrer"
  });
}

async function chooseShell(clientID) {
  const windows = await self.clients.matchAll({type: "window", includeUncontrolled: true});
  const candidates = windows.filter(client => client.frameType !== "nested");
  if (!candidates.length) throw new Error("no application tab is connected");
  // An iframe's client ID is not always available on its initial navigation.
  // Probe reachable windows rather than relying on worker-global state.
  return new Promise((resolve, reject) => {
    const probes = [];
    function clear() {
      clearTimeout(timer);
      for (const port of probes) port.close();
    }
    const timer = setTimeout(() => { clear(); reject(new Error("WebRTC connection unavailable")); }, 2500);
    candidates.sort((a, b) => Number(b.id === clientID || b.focused) - Number(a.id === clientID || a.focused));
    for (const client of candidates) {
      const {port1, port2} = new MessageChannel();
      probes.push(port1);
      port1.onmessage = event => {
        if (event.data !== "ready") return;
        clear();
        resolve(client);
      };
      client.postMessage({type: "probe"}, [port2]);
    }
  });
}

async function route(req, clientID) {
  if (req.mode === "navigate") {
    const source = clientID ? await self.clients.get(clientID) : null;
    const referrer = req.referrer;
    if (!source && (!referrer || !referrer.startsWith(self.location.origin + "/"))) {
      return new Response("External iframe navigation is not allowed", {
        status: 403, headers: {"Content-Security-Policy":"frame-ancestors 'none'", "Cache-Control":"no-store"}
      });
    }
  }
  const shell = await chooseShell(clientID);
  const {port1, port2} = new MessageChannel();
  const url = new URL(req.url);
  // Use a transfer rather than reading or cloning the body in the worker.
  const body = req.body;
  const metadata = {method: req.method, path: url.pathname + url.search, headers: Object.create(null),
    credentials: req.credentials !== "omit", body: !!body};
  req.headers.forEach((v, k) => { metadata.headers[k] = [v]; });
  return new Promise((resolve, reject) => {
    // The upstream transport enforces its own response-header timeout after
    // the upload finishes. Do not apply the short GET timeout to a large body.
    const timeout = setTimeout(() => abort(new Error("upstream response timed out")), body ? 2 * 60 * 60 * 1000 : 55000);
    function clear() {
      clearTimeout(timeout);
      req.signal.removeEventListener("abort", onAbort);
      port1.close();
    }
    function abort(err) { port1.postMessage("abort"); clear(); reject(err); }
    function onAbort() { abort(new Error("request canceled")); }
    req.signal.addEventListener("abort", onAbort, {once: true});
    port1.onmessage = event => {
      clear();
      const {error, status, headers, stream} = event.data;
      if (error) { reject(new Error(error)); return; }
      try {
        const out = new Headers();
        for (const [k, values] of Object.entries(headers || {})) {
          for (const v of values) out.append(k, v);
        }
        resolve(new Response(stream || null, {status, headers: out}));
      } catch (err) {
        if (stream) stream.cancel().catch(() => {});
        reject(err);
      }
    };
    try { shell.postMessage({type: "http", metadata, body}, body ? [port2, body] : [port2]); }
    catch (err) { clear(); reject(err); }
  });
}
