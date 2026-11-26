import {spawn} from "node:child_process";
import {createInterface} from "node:readline";
import {once} from "node:events";
import {createHash} from "node:crypto";
import {readFile, readdir, mkdir} from "node:fs/promises";

const [url, port, spki, profile, byteCount] = process.argv.slice(2);
const browser = spawn("chromium", ["--headless", "--no-sandbox", "--disable-dev-shm-usage",
  "--no-proxy-server", "--disable-background-networking",
  `--host-resolver-rules=MAP example.test 127.0.0.1:${port}, MAP *.example.test 127.0.0.1:${port}, MAP outside.test 127.0.0.1:${port}`,
  "--ignore-certificate-errors-spki-list=" + spki, "--user-data-dir=" + profile + "/browser",
  "--remote-debugging-port=0", "about:blank"], {stdio:["ignore","ignore","pipe"]});
const log = createInterface({input:browser.stderr});
let ws;
const pending = new Map();
const contexts = new Map();
let id = 0;
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
function cdp(method,params = {}, sessionId) {
  return new Promise((resolve,reject) => {
    pending.set(++id,{resolve,reject});
    ws.send(JSON.stringify({id,method,params,sessionId}));
  });
}
async function evaluate(expression, contextId) {
  const r = await cdp("Runtime.evaluate",{expression, contextId, awaitPromise:true, returnByValue:true,userGesture:true});
  if (r.exceptionDetails) throw Error(r.exceptionDetails.exception?.description || r.exceptionDetails.text);
  return r.result.value;
}
async function ready(marker = "fixture") {
  for (let i = 0; i < 200; i++) {
    try {
      const value = await evaluate(`({fixture:!!document.querySelector('#app')?.contentDocument?.getElementById(${JSON.stringify(marker)}),
        status:document.querySelector('#state')?.textContent, details:document.querySelector('#detail')?.textContent,
        frame:document.querySelector('#app')?.contentDocument?.body?.textContent?.slice(0,120)})`);
      if (value.fixture) return;
      if (value.status === "Connection failed" || value.frame?.startsWith("p2page:") ||
          value.frame?.startsWith("login requires Content-Length")) throw Error(JSON.stringify(value));
      if (i === 199) throw Error(JSON.stringify(value));
    } catch (err) {
      if (!String(err).includes("context")) throw err;
    }
    await sleep(200);
  }
  throw Error("application did not render");
}
async function waitRoute(expected) {
  let urls;
  for (let i = 0; i < 100; i++) {
    try {
      urls = await evaluate(`(() => {
        const path = l => l.pathname + l.search + l.hash;
        return [path(location),path(document.querySelector('#app').contentWindow.location)];
      })()`);
      if (urls.every(url => url === expected)) return;
    } catch {}
    await sleep(50);
  }
  throw Error("expected both URLs to be " + expected + ": " + JSON.stringify(urls));
}
async function frameNavigation(code) {
  return evaluate(`(() => { const child = document.querySelector('#app').contentWindow; ${code} })()`);
}
try {
  const endpoint = await Promise.race([
    new Promise((resolve,reject) => {
      browser.once("error",reject);
      browser.once("exit",code => reject(Error("Chromium exited " + code)));
      log.on("line",line => { const m = line.match(/DevTools listening on (ws:\/\/[^/]+)/); if (m) resolve(m[1]); });
    }),
    new Promise((_,reject) => setTimeout(() => reject(Error("Chromium startup timed out")),10000).unref())
  ]);
  const targets = await (await fetch(endpoint.replace("ws:","http:")+"/json/list")).json();
  ws = new WebSocket(targets.find(t => t.type === "page").webSocketDebuggerUrl);
  await once(ws,"open");
  ws.addEventListener("message",event => {
    const result = JSON.parse(event.data);
    if (result.method === "Runtime.executionContextCreated") {
      const c = result.params.context;
      if (c.auxData?.isDefault) contexts.set(c.auxData.frameId,c.id);
    }
    const cb = pending.get(result.id);
    if (!cb) return;
    pending.delete(result.id);
    if (result.error) cb.reject(Error(result.error.message));
    else cb.resolve(result.result);
  });
  await cdp("Page.enable");
  await cdp("Runtime.enable");
  await cdp("Page.navigate",{url});
  for (let i=0; i<50; i++) {
    try { if (await evaluate(`!!document.querySelector('input[name=password]')`)) break; } catch {}
    await sleep(100);
  }
  await evaluate(`(async () => {
    for (let n=0; n<40 && !document.querySelector('form').action.endsWith('#first-native'); n++) {
      await new Promise(resolve => setTimeout(resolve,50));
    }
    document.querySelector('input[name=name]').value = 'admin';
    document.querySelector('input[name=password]').value = 'initial passphrase';
    document.querySelector('form').requestSubmit();
  })()`);
  await ready();
  await waitRoute("/web/#first-native");
  if (await evaluate("document.title") !== "Fixture" || !await evaluate(`!!document.head.querySelector('link[rel=icon]')`))
    throw Error("the application title or favicon was not mirrored");
  const downloadDir = profile + "/downloads";
  await mkdir(downloadDir);
  await cdp("Browser.setDownloadBehavior",{behavior:"allow",downloadPath:downloadDir});
  async function verifyDownload(name) {
    for (let i = 0; i < 200; i++) {
      if ((await readdir(downloadDir)).includes(name)) {
        const bytes = await readFile(downloadDir + "/" + name);
        const digest = data => createHash("sha256").update(data).digest("hex");
        if (digest(bytes) !== digest("0123456789abcdef".repeat(Number(byteCount)/16)))
          throw Error("download attribute saved the wrong contents: " + bytes.length + " bytes");
        return;
      }
      await sleep(50);
    }
    throw Error("download did not finish: " + name);
  }
  await frameNavigation(`child.document.querySelector('#save').click()`);
  await verifyDownload("fixture.bin");
  const point = await evaluate(`(() => {
    const frame = document.querySelector('#app');
    const link = frame.contentDocument.querySelector('#save');
    link.download = "mouse.bin";
    const {x,y,width,height} = link.getBoundingClientRect(), rect = frame.getBoundingClientRect();
    return {x:rect.x+x+width/2,y:rect.y+y+height/2};
  })()`);
  await cdp("Input.dispatchMouseEvent",{...point,type:"mousePressed",button:"left",clickCount:1});
  await cdp("Input.dispatchMouseEvent",{...point,type:"mouseReleased",button:"left",clickCount:1});
  await verifyDownload("mouse.bin");
  console.log("private application login handoff, direct WebRTC and chained iframe redirects ready");
  const tests = String.raw`(async () => {
    const assert = (ok, message) => { if (!ok) throw new Error(message); };
    const cookies = await (await fetch("/cookie")).text();
    assert(cookies === "persisted" && !document.cookie.includes("appSession"), "HttpOnly cookie forwarding");
    for (let i=0; i<50 && document.body.dataset.staticAsset !== "loaded"; i++) {
      await new Promise(resolve => setTimeout(resolve, 100));
    }
    assert(document.body.dataset.staticAsset === "loaded", "proxied script did not execute");
    const styles = await Promise.all(Array.from({length:88}, (_,i) => new Promise(resolve => {
      const link = document.createElement("link");
      link.rel = "stylesheet";
      link.href = "/style-burst/" + i + ".css";
      link.onload = () => resolve(!!link.sheet?.cssRules.length);
      link.onerror = () => resolve(false);
      document.head.append(link);
    })));
    assert(styles.every(Boolean), "parallel CSS failure: " + styles.filter(ok => !ok).length + " of " + styles.length);
    if (navigator.serviceWorker) {
      let error;
      try { await navigator.serviceWorker.register("/app-worker.js?client-only=worker"); }
      catch (err) { error = err; }
      assert(error?.name === "NotSupportedError", "app worker registration was not isolated");
    }
    assert((await (await fetch("/assets/api")).text()) === "persisted", "API under asset directory lost its cookies");
    assert((await fetch("/omit", {credentials:"omit"})).status === 204 && !document.cookie.includes("ignored="), "credential omission");
    assert((await fetch("/set-visible")).status === 204 && document.cookie.includes("visible=from-response"), "non-HttpOnly response Cookie");
    document.cookie = "appSession=overwritten; Path=/";
    assert((await (await fetch("/cookie")).text()) === "persisted" && !document.cookie.includes("appSession"), "script was able to overwrite HttpOnly");
    assert((await (await fetch("/redirect")).text()) === "persisted", "redirect");
    const bytes = Number(BYTE_COUNT);
    const transfer = await Promise.all([
      fetch("/upload", {method:"POST", body:new Blob([new Uint8Array(bytes)])}).then(r => r.text()),
      fetch("/download").then(r => r.arrayBuffer())
    ]);
    const downloaded = new Uint8Array(transfer[1]);
    assert(transfer[0] === String(bytes) && downloaded.length === bytes && downloaded.at(-1) === 102, "streamed upload/download");
    const range = await fetch("/download",{headers:{Range:"bytes=60-63"}});
    assert(range.status === 206 && await range.text() === "cdef", "video range request");
    const html = await fetch("/web/");
    assert(html.statusText === "OK" && html.headers.has("Content-Length") && !(await html.text()).includes("/__p2page/socket.js"),
      "fetch did not return the original HTML and HTTP metadata");
    const multipart = new FormData();
    multipart.append("data", "value");
    const [origin, referrer, length] = (await (await fetch("/request-metadata",{method:"POST",body:multipart})).text()).split("|");
    assert(origin.startsWith("https://127.0.0.1:") && referrer === origin + "/web/" && Number(length) > 0,
      "multipart fetch lost Origin, Referer or Content-Length");
    const canvas = document.createElement("canvas");
    canvas.width = canvas.height = 32;
    const paint = canvas.getContext("2d");
    const capture = canvas.captureStream(15);
    const recording = new MediaRecorder(capture,{mimeType:"video/webm;codecs=vp8"});
    const parts = [];
    recording.ondataavailable = event => parts.push(event.data);
    const stopped = new Promise(resolve => recording.onstop = resolve);
    recording.start();
    for (let n=0; n<12; n++) {
      paint.fillStyle = n%2 ? "red" : "blue";
      paint.fillRect(0,0,32,32);
      await new Promise(resolve => setTimeout(resolve,75));
    }
    recording.stop();
    await stopped;
    capture.getTracks().forEach(track => track.stop());
    const videoUpload = await fetch("/video",{method:"POST",body:new Blob(parts,{type:"video/webm"})});
    assert(videoUpload.status === 204,"video fixture upload");
    const video = document.createElement("video");
    video.muted = true;
    video.src = "/video";
    document.body.append(video);
    const decoded = new Promise(resolve => video.requestVideoFrameCallback(resolve));
    await video.play();
    await decoded;
    assert(video.videoWidth === 32,"WebM video did not decode");
    video.pause();
    video.remove();
    await new Promise((resolve,reject) => {
      const s = new EventSource("/sse");
      let first;
      s.onerror = () => { s.close(); reject(new Error("SSE transport error")); };
      s.onmessage = e => {
        if (e.data === "first") first = performance.now();
        else {
          s.close();
          try { assert(e.data === "second" && performance.now()-first >= 250, "SSE messages were buffered"); resolve(); }
          catch (err) { reject(err); }
        }
      };
    });
    await new Promise((resolve,reject) => {
      const s = new WebSocket("/ws","demo");
      s.binaryType = "arraybuffer";
      s.onerror = () => reject(new Error("WebSocket error"));
      s.onopen = () => {
        try { assert(s.protocol === "demo","WebSocket subprotocol"); s.send("test"); }
        catch (err) { reject(err); }
      };
      s.onmessage = e => {
        if (e.data === "test") s.send(new Uint8Array(65537).fill(73));
        else {
          try { assert(e.data instanceof ArrayBuffer && e.data.byteLength === 65537 && new Uint8Array(e.data).at(-1) === 73, "WebSocket binary"); s.close(); }
          catch (err) { reject(err); }
        }
      };
      s.onclose = e => { if (e.code === 1000) resolve(); else reject(new Error("WebSocket close " + e.code)); };
    });
    document.cookie = "saved=1; Path=/; Secure";
    assert(document.cookie.includes("saved=1"), "document.cookie setter not visible");
    assert((await (await fetch("/script-cookie")).text()) === "1", "script cookie did not reach upstream");
    return "fetch, redirect, upload, download, range, video playback, SSE, WebSocket and HttpOnly cookie passed";
  })()`.replace("BYTE_COUNT",byteCount);
  const tree = (await cdp("Page.getFrameTree")).frameTree;
  const context = contexts.get(tree.childFrames[0].frame.id);
  if (!context) throw Error("iframe execution context is missing");
  console.log(await evaluate(tests, context));
  await cdp("Page.reload");
  await ready();
  const persisted = await evaluate(`document.querySelector('#app').contentWindow.document.cookie`);
  if (!persisted.includes("saved=1")) throw Error("application cookie was not preserved after reload");
  console.log("reconnect and cookie persistence passed");
  await frameNavigation(`child.history.pushState({app:1},"","/web/jellyfin?view=grid#/first")`);
  await waitRoute("/web/jellyfin?view=grid#/first");
  await frameNavigation(`child.history.replaceState({app:2},"","/web/jellyfin?view=list#/second")`);
  await waitRoute("/web/jellyfin?view=list#/second");
  const spaRoute = "/web/opencode/session?client-only=spa#details";
  await frameNavigation(`child.history.pushState({app:3},"",${JSON.stringify(spaRoute)})`);
  await waitRoute(spaRoute);
  await evaluate("history.back()");
  await waitRoute("/web/jellyfin?view=list#/second");
  await evaluate("history.forward()");
  await waitRoute(spaRoute);
  await frameNavigation(`child.location.hash = "#from-iframe"`);
  await waitRoute("/web/opencode/session?client-only=spa#from-iframe");
  await evaluate(`location.hash = "#edited-in-address-bar"`);
  const reloadRoute = "/web/opencode/session?client-only=spa#edited-in-address-bar";
  await waitRoute(reloadRoute);
  await cdp("Page.reload");
  await ready();
  await waitRoute(reloadRoute);
  console.log("normal URL, SPA push/replace, back/forward, hashes and deep reload passed");
  await cdp("Network.deleteCookies",{name:"__Host-p2page-grant",url:new URL(url).origin + "/"});
  await cdp("Page.reload");
  await ready();
  await waitRoute(reloadRoute);
  console.log("private authorization renewed without losing the normal URL");
  await evaluate(`document.querySelector('#app').contentDocument.querySelector('form').requestSubmit()`);
  await ready("login-ok");
  await waitRoute("/cgi-bin/luci/admin/?tab=home");
  await evaluate("history.back()");
  await ready();
  await waitRoute(reloadRoute);
  await evaluate("history.forward()");
  await ready("login-ok");
  await waitRoute("/cgi-bin/luci/admin/?tab=home");
  await cdp("Page.reload");
  await ready("login-ok");
  await waitRoute("/cgi-bin/luci/admin/?tab=home");
  console.log("iframe login POST, redirect and path-scoped HttpOnly cookie across reconnect passed");
  await evaluate("history.back()");
  await ready();
  await frameNavigation(`const form = child.document.querySelector('form'); form.target = "_blank"; form.requestSubmit()`);
  let popup;
  for (let i = 0; i < 100 && !popup; i++) {
    const tabs = await (await fetch(endpoint.replace("ws:","http:")+"/json/list")).json();
    popup = tabs.find(t => t.type === "page" && t.id !== targets.find(t => t.type === "page").id);
    if (!popup) await sleep(100);
  }
  if (!popup) throw Error("target=_blank did not open an application tab");
  const {sessionId} = await cdp("Target.attachToTarget",{targetId:popup.id,flatten:true});
  for (let i = 0; i < 100; i++) {
    const {result} = await cdp("Runtime.evaluate",{expression:`location.pathname + location.search === "/cgi-bin/luci/admin/?tab=home" &&
      !!document.querySelector('#app')?.contentDocument?.getElementById('login-ok')`,returnByValue:true},sessionId);
    if (result.value) break;
    if (i === 99) throw Error("target=_blank did not display the POST result");
    await sleep(100);
  }
  await cdp("Target.closeTarget",{targetId:popup.id});
  await frameNavigation(`const form = child.document.querySelector('form'); form.target = "_top"; form.submit()`);
  await ready("login-ok");
  await waitRoute("/cgi-bin/luci/admin/?tab=home");
  console.log("top-level and new-tab form POSTs retained their application shell");
  await cdp("Page.navigate",{url:"https://outside.test/"});
  for (let i = 0; i < 100; i++) {
    if (await evaluate(`location.host === "outside.test" && !!document.querySelector('a')`).catch(() => false)) break;
    if (i === 99) throw Error("cross-site fixture did not open");
    await sleep(50);
  }
  await evaluate(`document.querySelector('a').click()`);
  await ready("strict-check");
  await waitRoute("/strict-end");
  if (await evaluate(`document.querySelector('#app').contentDocument.querySelector('#strict-check').textContent`) !== "withheld")
    throw Error("SameSite=Strict cookie was sent on cross-site entry through a redirect");
  await frameNavigation(`child.location.assign("/strict-end?from=same-site")`);
  await waitRoute("/strict-end?from=same-site");
  await ready("strict-check");
  if (await evaluate(`document.querySelector('#app').contentDocument.querySelector('#strict-check').textContent`) !== "sent")
    throw Error("SameSite=Strict cookie was lost on a subsequent same-site navigation");
  await frameNavigation(`child.location.assign("/frame-denied")`);
  for (let i = 0; i < 100; i++) {
    if (await evaluate(`document.querySelector('#state')?.textContent === "Application frame unavailable"`)) break;
    if (i === 99) throw Error("an X-Frame-Options failure was not shown");
    await sleep(100);
  }
} finally {
  ws?.close();
  browser.kill();
  log.close();
}
