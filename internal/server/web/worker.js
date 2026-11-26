"use strict";
const prefix = "/__p2page/";
const pendingNavigations = new Map(), iframeNavigations = new Map();
self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", event => event.waitUntil(self.clients.claim()));

function localURL(value) {
  const url = URL.parse(value);
  return url?.origin === self.location.origin && !url.username && !url.password && !url.pathname.startsWith(prefix) ? url : null;
}

self.addEventListener("message", event => {
  const {source, data, ports:[port]} = event;
  if (data?.type !== "initial-navigation" || !port || source?.type !== "window" || source.frameType === "nested") return;
  let entry = pendingNavigations.get(source.id);
  const target = localURL(data.url);
  const referrer = URL.parse(data.referrer);
  if (!entry && target && referrer && /^https?:$/.test(referrer.protocol) && referrer.origin !== self.location.origin) {
    if (pendingNavigations.size >= 32) {
      port.postMessage({error:"Too many pending application navigations"});
      port.close();
      return;
    }
    // A cross-origin entry is loaded inside a same-origin iframe. Preserve
    // the source site for cookie selection on that navigation and its redirects.
    entry = rememberNavigation(event, {shell:source.id, url:target.href, entryOrigin:referrer.origin});
  }
  port.postMessage({path:entry ? prefix + "entry/" + entry.token : null, post:!!entry?.req});
  port.close();
});

function networkURL(value) {
  const url = new URL(value);
  url.hash = "";
  return url.href;
}

self.addEventListener("fetch", event => {
  const req = event.request;
  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return;
  const top = req.mode === "navigate" && req.destination === "document";
  const frame = req.mode === "navigate" && req.destination === "iframe";
  const carried = frame && iframeNavigations.get(event.resultingClientId);
  let response;
  if (carried && networkURL(req.url) === networkURL(carried.url)) {
    const post = carried.req;
    if (post) carried.release();
    response = route(post || req, event.clientId, {shell:carried.shell, entryOrigin:carried.entryOrigin}).then(res => {
      const location = res.headers.get("Location");
      // The carrier is a GET; a method-preserving redirect must never
      // silently turn the original write into a GET.
      if (post && [307,308].includes(res.status) && location) {
        res.body?.cancel().catch(() => {});
        return problem(502, "Method-preserving redirects are not supported for top-level form submissions");
      }
      if (!post) {
        const next = [301,302,303,307,308].includes(res.status) && location && URL.parse(location, req.url);
        if (next && localURL(next.href)) carried.url = next.href;
        else carried.release();
      }
      return res;
    }, err => {
      carried.release();
      throw err;
    });
  } else if (url.pathname.startsWith(prefix + "entry/")) {
    const entry = frame && event.resultingClientId &&
      [...pendingNavigations.values()].find(n => url.pathname === prefix + "entry/" + n.token);
    if (entry && req.method === "GET" && localURL(req.referrer) && !entry.frame) {
      entry.frame = event.resultingClientId;
      iframeNavigations.set(entry.frame, entry);
      response = Promise.resolve(Response.redirect(entry.url));
    } else response = Promise.resolve(problem(404, "Application navigation is no longer available"));
  } else if (url.pathname === prefix + "download") {
    const target = localURL(url.searchParams.get("url"));
    response = frame && req.method === "GET" && target ?
      route(req, event.clientId, {target, download:url.searchParams.get("name") || ""}) :
      Promise.resolve(problem(404, "Invalid download"));
  } else if (url.pathname.startsWith(prefix)) return;
  else if (top && req.method === "POST") response = stagePost(event);
  else if (top && req.method !== "GET") response = Promise.resolve(problem(405, "Method not allowed"));
  else response = top ? loadShell() : route(req, event.clientId);
  event.respondWith(response.catch(err =>
    problem(502, "p2page: " + err.message)));
});

function problem(status, message) {
  return new Response(message, {status, headers:{
    "Content-Type":"text/plain; charset=utf-8", "Cache-Control":"no-store"
  }});
}

function stagePost(event) {
  const {request:req, resultingClientId:shell} = event;
  if (!shell || req.headers.get("origin") !== self.location.origin)
    return Promise.resolve(problem(403, "Cross-origin top-level form submission is not allowed"));
  if (pendingNavigations.size >= 32) return Promise.resolve(problem(503, "Too many pending form submissions"));
  // The old tab may unload when the shell commits, so deliver the original
  // request exactly once using the new shell's connection. Never serialize
  // the body into the shell, a URL, or persistent storage.
  const post = rememberNavigation(event, {req, shell, url:req.url});
  return loadShell().then(async res => {
    if (!res.ok) {
      post.release();
      return res;
    }
    // Mark this fixed bootstrap, not the request URL, so a worker update or
    // timeout cannot silently turn a lost POST into an ordinary initial GET.
    const headers = new Headers(res.headers);
    for (const name of ["Content-Length","Content-Encoding","ETag"]) headers.delete(name);
    return new Response((await res.text()) + '\n<meta name="p2page-post">', {headers});
  }).catch(err => {
    post.release();
    throw err;
  });
}

function rememberNavigation(event, entry) {
  entry.token = crypto.randomUUID();
  let resolve;
  event.waitUntil(new Promise(r => { resolve = r; }));
  const timer = setTimeout(() => {
    entry.release();
    entry.req?.body?.cancel().catch(() => {});
  }, 75000);
  entry.release = () => {
    clearTimeout(timer);
    if (pendingNavigations.get(entry.shell) === entry) pendingNavigations.delete(entry.shell);
    iframeNavigations.delete(entry.frame);
    resolve();
  };
  pendingNavigations.set(entry.shell, entry);
  return entry;
}

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

async function route(req, clientID, options = {}) {
  if (req.mode === "navigate" && !options.shell) {
    const source = clientID ? await self.clients.get(clientID) : null;
    const referrer = req.referrer;
    if (!source && (!referrer || !referrer.startsWith(self.location.origin + "/"))) {
      return new Response("External iframe navigation is not allowed", {
        status: 403, headers: {"Content-Security-Policy":"frame-ancestors 'none'", "Cache-Control":"no-store"}
      });
    }
  }
  const shell = options.shell ? await self.clients.get(options.shell) : await chooseShell(clientID);
  if (!shell) throw new Error("application tab is no longer available");
  const {port1, port2} = new MessageChannel();
  const url = options.target || new URL(req.url);
  // Use a transfer rather than reading or cloning the body in the worker.
  const body = req.body;
  const metadata = {method: req.method, path: url.pathname + url.search, headers: Object.create(null),
    credentials: req.credentials !== "omit", body: !!body,
    raw: req.mode !== "navigate" || options.download !== undefined, keepalive: req.keepalive,
    entryOrigin:options.entryOrigin};
  req.headers.forEach((v, k) => { metadata.headers[k] = [v]; });
  const referrer = localURL(req.referrer);
  if (referrer) metadata.headers.referer = [referrer.origin + referrer.pathname + referrer.search];
  if (req.mode !== "navigate" && !["GET","HEAD"].includes(req.method) && !metadata.headers.origin) {
    // Chromium exposes the referrer as a Request property and may omit the
    // network-generated Origin header on non-navigation FetchEvents.
    if (referrer) metadata.headers.origin = [self.location.origin];
    else if (req.referrerPolicy === "no-referrer" && req.mode !== "cors") metadata.headers.origin = ["null"];
  }
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
      const {error, status, statusText, headers, stream} = event.data;
      if (error) { reject(new Error(error)); return; }
      try {
        const out = new Headers();
        for (const [k, values] of Object.entries(headers || {})) {
          for (const v of values) out.append(k, v);
        }
        if (options.download !== undefined) downloadHeaders(url, options.download, status, out);
        const reason = typeof statusText === "string" ? statusText.replace(/[^\t\x20-\x7e\x80-\xff]/g, "") : "";
        resolve(new Response(stream || null, {status, statusText:reason, headers: out}));
      } catch (err) {
        if (stream) stream.cancel().catch(() => {});
        reject(err);
      }
    };
    try { shell.postMessage({type: "http", metadata, body}, body ? [port2, body] : [port2]); }
    catch (err) { clear(); reject(err); }
  });
}

function downloadHeaders(target, name, status, headers) {
  if ([301,302,303,307,308].includes(status) && headers.has("Location")) {
    const next = URL.parse(headers.get("Location"), target);
    if (next && localURL(next.href)) {
      headers.set("Location", prefix + "download?" + new URLSearchParams({url:next.href, name}));
    } else if (next) headers.set("Location", next.href);
  } else if (status >= 200 && status < 300) {
    const disposition = headers.get("Content-Disposition") || "";
    if (/^\s*(attachment|inline)\s*;/i.test(disposition) && /;\s*filename\*?\s*=/i.test(disposition)) {
      headers.set("Content-Disposition", disposition.replace(/^\s*inline/i, "attachment"));
    } else {
      if (!name) {
        const base = target.pathname.split("/").pop();
        try { name = decodeURIComponent(base); } catch { name = base; }
      }
      const encoded = encodeURIComponent(name.toWellFormed()).replace(/['()*]/g, c =>
        "%" + c.charCodeAt(0).toString(16));
      headers.set("Content-Disposition", encoded ? "attachment; filename*=UTF-8''" + encoded : "attachment");
    }
  } else {
    // An unsuccessful download is shown in a hidden frame, never as an
    // executable same-origin application page.
    headers.set("Content-Security-Policy", "default-src 'none'; sandbox");
  }
}
