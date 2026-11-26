(() => {
  "use strict";
  let bridge, cookies, reportRoute;
  try {
    bridge = window.parent !== window && window.parent.p2pageSocket;
    cookies = window.parent !== window && window.parent.p2pageCookies;
    reportRoute = window.parent !== window && window.parent.p2pageRoute;
  }
  catch { return; }
  if (reportRoute) {
    const changed = () => reportRoute(window);
    if (window.navigation) {
      window.navigation.addEventListener("currententrychange", changed);
    } else {
      for (const method of ["pushState","replaceState"]) {
        const original = history[method];
        history[method] = function(...args) {
          const result = Reflect.apply(original,this,args);
          changed();
          return result;
        };
      }
      window.addEventListener("popstate", changed);
      window.addEventListener("hashchange", changed);
    }
    changed();
  }
  if (cookies) {
    Object.defineProperty(document,"cookie",{
      configurable:true,
      get() { return cookies.read(location.pathname); },
      set(value) { cookies.write(location.pathname,value); }
    });
  }
  if ((bridge || cookies) && navigator.serviceWorker) {
    // Registration fetches bypass Service Workers. Do not send the app's
    // worker URL or replace the worker that carries the WebRTC transport.
    navigator.serviceWorker.register = () => Promise.reject(
      new DOMException("Application Service Workers are not supported through p2page","NotSupportedError"));
    window.addEventListener("click", event => {
      if (event.defaultPrevented || event.button !== 0) return;
      const link = event.composedPath().find(node => node instanceof HTMLAnchorElement && node.hasAttribute("download"));
      if (!link?.hasAttribute("href")) return;
      const target = URL.parse(link.href);
      if (target?.origin !== location.origin || target.pathname.startsWith("/__p2page/")) return;
      event.preventDefault();
      // Chromium's native download request bypasses the controlling worker.
      // Use an iframe navigation and an attachment response instead; the
      // browser still streams the file directly to its download manager.
      const frame = document.createElement("iframe");
      frame.hidden = true;
      frame.referrerPolicy = link.referrerPolicy;
      frame.src = "/__p2page/download?" + new URLSearchParams({url:target.href, name:link.download});
      frame.onload = () => frame.remove();
      document.body.append(frame);
      setTimeout(() => frame.remove(), 2 * 60 * 60 * 1000);
    });
  }
  if (!bridge) return;
  const Native = window.WebSocket;
  const encoder = new TextEncoder();
  class Proxied extends EventTarget {
    #bridge;
    #state = 0;
    #protocol = "";
    #url;
    #binary = "blob";
    #leave = () => { if (this.#state < 2) this.close(1000, "Page unloaded"); };
    constructor(url, protocols = []) {
      super();
      const u = new URL(url, location.href);
      if (u.protocol === "http:") u.protocol = "ws:";
      if (u.protocol === "https:") u.protocol = "wss:";
      if (u.host !== location.host || !["ws:","wss:"].includes(u.protocol) || u.pathname.startsWith("/__p2page/"))
        return new Native(url, protocols);
      if (u.hash || u.username || u.password || (location.protocol === "https:" && u.protocol === "ws:"))
        throw new DOMException("Invalid WebSocket URL", "SyntaxError");
      const ps = typeof protocols === "string" ? [protocols] : Array.from(protocols, String);
      if (new Set(ps).size !== ps.length || ps.some(p => !/^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/.test(p)))
        throw new DOMException("Invalid WebSocket subprotocol", "SyntaxError");
      this.#url = u.href;
      this.#bridge = bridge(u.pathname+u.search, ps, (kind, value) => {
        if (kind === "open" && this.#state === 0) {
          this.#state = 1;
          this.#protocol = value;
          this.dispatchEvent(new Event("open"));
        }
        if (kind === "message" && this.#state === 1) {
          let data = value;
          if (typeof data !== "string") data = this.#binary === "blob" ? new Blob([data]) : new Uint8Array(new Uint8Array(data)).buffer;
          this.dispatchEvent(new MessageEvent("message",{data,origin:u.origin}));
        }
        if (kind === "close") queueMicrotask(() => {
          if (this.#state === 3) return;
          this.#state = 3;
          window.removeEventListener("pagehide", this.#leave);
          if (value.code === 1006) this.dispatchEvent(new Event("error"));
          this.dispatchEvent(new CloseEvent("close",{...value,wasClean:value.code !== 1006}));
        });
      });
      window.addEventListener("pagehide", this.#leave, {once: true});
    }
    get readyState() { return this.#state; }
    get url() { return this.#url; }
    get protocol() { return this.#protocol; }
    get extensions() { return ""; }
    get bufferedAmount() { return this.#bridge.buffered(); }
    get binaryType() { return this.#binary; }
    set binaryType(value) { if (value === "blob" || value === "arraybuffer") this.#binary = value; }
    send(value) {
      if (this.#state === 0) throw new DOMException("Still connecting", "InvalidStateError");
      if (this.#state !== 1) return;
      if (value instanceof Blob) this.#bridge.send(value,false);
      else if (value instanceof ArrayBuffer) this.#bridge.send(value.slice(0),false);
      else if (ArrayBuffer.isView(value)) this.#bridge.send(new Uint8Array(value.buffer,value.byteOffset,value.byteLength).slice().buffer,false);
      else this.#bridge.send(encoder.encode(String(value)).buffer,true);
    }
    close(code = 1000, reason = "") {
      code = Number(code);
      reason = String(reason);
      if (code !== 1000 && !(Number.isInteger(code) && code >= 3000 && code <= 4999))
        throw new DOMException("Invalid close code", "InvalidAccessError");
      if (encoder.encode(reason).length > 123) throw new DOMException("Close reason too long", "SyntaxError");
      if (this.#state >= 2) return;
      this.#state = 2;
      this.#bridge.close(code,reason);
    }
  }
  for (const [name, value] of [["CONNECTING",0],["OPEN",1],["CLOSING",2],["CLOSED",3]]) {
    Object.defineProperty(Proxied,name,{value});
    Object.defineProperty(Proxied.prototype,name,{value});
  }
  for (const kind of ["open","message","error","close"]) {
    const handlers = new WeakMap();
    Object.defineProperty(Proxied.prototype,"on"+kind,{
      get() { return handlers.get(this) || null; },
      set(handler) {
        const old = handlers.get(this);
        if (old) this.removeEventListener(kind,old);
        handlers.delete(this);
        if (typeof handler === "function") { handlers.set(this,handler); this.addEventListener(kind,handler); }
      }
    });
  }
  window.WebSocket = Proxied;
})();
