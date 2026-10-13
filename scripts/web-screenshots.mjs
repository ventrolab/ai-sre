// Screenshots of console pages with a logged-in session (see scripts/web-screenshots.sh).
// Usage: node web-screenshots.mjs <session cookie> <outdir> <path>...
import { spawn } from "node:child_process";
const [cookie, outdir, ...paths] = process.argv.slice(2);
const chrome = spawn("google-chrome", ["--headless=new", "--remote-debugging-port=9333", "--window-size=1440,1000",
  "--hide-scrollbars", "--force-device-scale-factor=1", `--user-data-dir=${outdir}/profile`, "about:blank"], { stdio: "ignore" });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let ws;
for (let i = 0; i < 50; i++) {
  try { const v = await (await fetch("http://127.0.0.1:9333/json/version")).json(); ws = v.webSocketDebuggerUrl; break; } catch { await sleep(200); }
}
const sock = new WebSocket(ws); await new Promise((r) => sock.addEventListener("open", r));
let id = 0; const pending = new Map(); const events = [];
sock.addEventListener("message", (m) => { const d = JSON.parse(m.data); if (d.id && pending.has(d.id)) { pending.get(d.id)(d); pending.delete(d.id); } else events.push(d); });
const send = (method, params = {}, sessionId) => new Promise((r) => { const i = ++id; pending.set(i, r); sock.send(JSON.stringify({ id: i, method, params, sessionId })); });
const { result: { targetId } } = await send("Target.createTarget", { url: "about:blank" });
const { result: { sessionId } } = await send("Target.attachToTarget", { targetId, flatten: true });
await send("Network.enable", {}, sessionId);
await send("Network.setCookie", { name: "aisre_session", value: cookie, domain: new URL(process.env.WEB_URL || "http://localhost:8088").hostname, path: "/" }, sessionId);
await send("Emulation.setDeviceMetricsOverride", { width: 1440, height: 1000, deviceScaleFactor: 1, mobile: false }, sessionId);
for (const p of paths) {
  await send("Page.navigate", { url: `${process.env.WEB_URL || "http://localhost:8088"}${p}` }, sessionId);
  await sleep(2500);
  const { result: { cssContentSize } } = await send("Page.getLayoutMetrics", {}, sessionId);
  const h = Math.min(Math.ceil(cssContentSize.height), 3000);
  const shot = await send("Page.captureScreenshot", { format: "png", captureBeyondViewport: true, clip: { x: 0, y: 0, width: 1440, height: h, scale: 1 } }, sessionId);
  const name = (p === "/" ? "dashboard" : p.replace(/^\//, "").replace(/[/?=&]/g, "_")) + ".png";
  (await import("node:fs")).writeFileSync(`${outdir}/${name}`, Buffer.from(shot.result.data, "base64"));
  console.log("saved", name, h);
}
sock.close(); chrome.kill();
