package exporter

import "html/template"

var htmlTemplate = template.Must(template.New("html").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
/* Semantic colors mirror the TUI palette (internal/tui/styles.go). The dark
   theme uses the exact TUI hex values; the light theme uses readable darker
   variants of the same hues. */
:root { color-scheme: light dark; --bg:#f8f8f5; --fg:#202124; --muted:#6b6f76; --line:#d8d9d3; --panel:#ffffff; --code:#f0f1ec; --accent:#0067c0; --req:#3b6fc4; --resp:#2e7d32; --notif:#5a51bf; --stderr:#6b6f76; --pending:#0e7c86; --warn:#8a6d00; --err:#c62828; --invalid:#b0177e; }
@media (prefers-color-scheme: dark) { :root { --bg:#171817; --fg:#eceee8; --muted:#aeb4ad; --line:#343832; --panel:#20221f; --code:#151714; --accent:#00afff; --req:#87afff; --resp:#87d787; --notif:#afafd7; --stderr:#8a8a8a; --pending:#5fd7d7; --warn:#d7af5f; --err:#ff5f5f; --invalid:#ff5faf; } }
* { box-sizing: border-box; }
body { margin:0; background:var(--bg); color:var(--fg); font:14px/1.45 ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
header { position:sticky; top:0; z-index:2; padding:18px 24px 14px; background:color-mix(in srgb, var(--bg) 92%, transparent); border-bottom:1px solid var(--line); backdrop-filter:blur(10px); }
h1 { margin:0 0 10px; font-size:22px; letter-spacing:0; }
.meta { display:flex; flex-wrap:wrap; gap:10px 18px; color:var(--muted); }
.meta b, .pill b { color:var(--fg); font-weight:600; }
.toolbar { margin-top:14px; display:flex; gap:10px; align-items:center; }
input { width:min(680px, 100%); padding:9px 11px; border:1px solid var(--line); border-radius:6px; background:var(--panel); color:var(--fg); font:inherit; }
main { padding:18px 24px 40px; max-width:1180px; margin:0 auto; }
.section-title { margin:22px 0 10px; color:var(--muted); font-size:12px; font-weight:700; letter-spacing:.08em; text-transform:uppercase; }
.grid { display:grid; grid-template-columns:repeat(auto-fit,minmax(160px,1fr)); gap:10px; }
.pill { padding:10px 12px; border:1px solid var(--line); border-radius:6px; background:var(--panel); }
.event { border:1px solid var(--line); border-left:3px solid var(--line); border-radius:6px; background:var(--panel); margin:10px 0; overflow:hidden; }
.event[hidden] { display:none; }
.head { display:grid; grid-template-columns:72px 110px minmax(120px,1fr) 105px 100px; gap:10px; align-items:center; padding:10px 12px; border-bottom:1px solid var(--line); }
.seq { color:var(--muted); font-variant-numeric:tabular-nums; }
.dir { font-family:ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; color:var(--muted); }
.method { overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
.status { text-transform:uppercase; font-size:12px; font-weight:700; color:var(--muted); }
.status.ok { color:var(--resp); }
.status.error { color:var(--err); }
.status.warn { color:var(--warn); }
.status.superseded { color:var(--warn); }
.status.pending { color:var(--pending); }
.status.bad { color:var(--invalid); }
/* Per-event tone by kind/status, matching the TUI stream colors. */
.tone-req { border-left-color:var(--req); } .tone-req .dir { color:var(--req); }
.tone-resp { border-left-color:var(--resp); } .tone-resp .dir { color:var(--resp); }
.tone-error { border-left-color:var(--err); } .tone-error .dir { color:var(--err); }
.tone-warn { border-left-color:var(--warn); } .tone-warn .dir { color:var(--warn); }
.tone-notif { border-left-color:var(--notif); } .tone-notif .dir { color:var(--notif); }
.tone-stderr { border-left-color:var(--stderr); } .tone-stderr .dir { color:var(--stderr); }
.tone-invalid { border-left-color:var(--invalid); } .tone-invalid .dir { color:var(--invalid); }
.time { color:var(--muted); font-variant-numeric:tabular-nums; }
details { padding:10px 12px; }
summary { cursor:pointer; color:var(--accent); user-select:none; }
pre { margin:10px 0 0; padding:12px; overflow:auto; border-radius:6px; background:var(--code); font:12px/1.45 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
.empty { color:var(--muted); padding:30px 0; }
@media (max-width:720px) { header, main { padding-left:14px; padding-right:14px; } .head { grid-template-columns:56px 70px 1fr; } .status, .time { display:none; } }
</style>
</head>
<body>
<header>
  <h1 id="title">mcpsnoop export</h1>
  <div class="meta" id="meta"></div>
  <div class="toolbar"><input id="q" type="search" placeholder="tool:echo  status:error  dir:s2c  kind:resp  id:7  ·  or plain text"></div>
</header>
<main>
  <div class="section-title">Summary</div>
  <div class="grid" id="summary"></div>
  <div class="section-title">Events</div>
  <div id="events"></div>
</main>
<script>
const data = {{.Data}};
const fmtTime = (s) => s ? new Date(s).toLocaleString() : "";
const textOf = (v) => v == null ? "" : (typeof v === "string" ? v : JSON.stringify(v, null, 2));
const compact = (v) => v == null ? "" : (typeof v === "string" ? v : JSON.stringify(v));
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, ch => ({ "&":"&amp;", "<":"&lt;", ">":"&gt;", '"':"&quot;", "'":"&#39;" }[ch]));
document.title = "mcpsnoop " + (data.session.label || data.session.id);
document.getElementById("title").textContent = data.session.label || data.session.id;
document.getElementById("meta").innerHTML = [
  "<span><b>" + esc(data.session.id) + "</b></span>",
  "<span>" + esc(fmtTime(data.session.first)) + "</span>",
  "<span>" + data.events.length + " frames</span>",
  "<span>" + data.calls.length + " calls</span>"
].join("");
document.getElementById("summary").innerHTML = [
  ["Requests", data.session.requests],
  ["Responses", data.session.responses],
  ["Notifications", data.session.notifications],
  ["Errors", data.session.errors],
  ["Pending", data.session.pending],
  ["Protocol", data.capabilities?.protocol_version || ""]
].map(([k,v]) => "<div class=\"pill\">" + esc(k) + "<br><b>" + esc(v) + "</b></div>").join("");
const calls = data.calls || [];
const events = data.events || [];
// Filter grammar mirrors the TUI stream filter, space-separated tokens (ANDed),
// where key:value matches a field and a bare token is a substring over the frame.
const norm = (s) => String(s ?? "").toLowerCase();
const matchDir = (dir, v) => {
  v = v.toLowerCase();
  if (["c2s", "client", "in", "req", "request", "->", "→"].includes(v)) return dir === "c2s";
  if (["s2c", "server", "out", "resp", "response", "<-", "←"].includes(v)) return dir === "s2c";
  if (["stderr", "err"].includes(v)) return dir === "stderr";
  return dir.toLowerCase() === v;
};
const matchKind = (kind, v) => {
  v = v.toLowerCase();
  if (["req", "request"].includes(v)) return kind === "request";
  if (["resp", "response"].includes(v)) return kind === "response";
  if (["notify", "notification", "ntf"].includes(v)) return kind === "notification";
  if (v === "stderr") return kind === "stderr";
  if (["invalid", "corrupt", "bad"].includes(v)) return kind === "invalid";
  return false;
};
const matchStatus = (ev, call, v) => {
  v = v.toLowerCase();
  if (v === "bad" || v === "invalid") return ev.kind === "invalid";
  if (v === "warn" || v === "warning") return !!ev.warning || !!ev.truncated || !!ev.deprecated;
  if (v === "mismatch") return !!ev.mismatch;
  if (!call) return false;
  if (["err", "error", "fail", "failed"].includes(v)) return call.status === "error";
  if (["pending", "pend", "inflight"].includes(v)) return call.status === "pending";
  if (["ok", "success"].includes(v)) return call.status === "ok";
  return false;
};
const bareMatch = (ev, call, q) =>
  norm(ev.method).includes(q) || norm(ev.id).includes(q) || norm(ev.text).includes(q) ||
  norm(compact(ev.raw)).includes(q) || (call ? norm(call.tool_name).includes(q) : false);
const matchToken = (ev, call, tok) => {
  const i = tok.indexOf(":");
  if (i > 0) {
    const k = tok.slice(0, i).toLowerCase(), v = tok.slice(i + 1);
    if (v) {
      switch (k) {
        case "tool": case "t": return call ? norm(call.tool_name).includes(v.toLowerCase()) : false;
        case "method": case "m": return norm(ev.method).includes(v.toLowerCase()) || (call ? norm(call.method).includes(v.toLowerCase()) : false);
        case "id": return norm(ev.id) === v.toLowerCase();
        case "dir": case "d": return matchDir(ev.direction, v);
        case "kind": case "k": return matchKind(ev.kind, v);
        case "status": case "s": return matchStatus(ev, call, v);
      }
    }
  }
  return bareMatch(ev, call, tok.toLowerCase());
};
const eventMatches = (ev, tokens) => {
  const call = ev.call_index == null ? null : calls[ev.call_index];
  return tokens.every((t) => matchToken(ev, call, t));
};
// toneOf and statusOf mirror the TUI, requests are blue, responses take their
// call's outcome (green ok, red error), notifications lavender, stderr gray. The
// status text shows on the response (or a pending request).
const toneOf = (ev, call) => {
  if (ev.kind === "stderr") return "stderr";
  if (ev.kind === "notification") return "notif";
  if (ev.kind === "invalid") return "invalid";
  if (ev.warning) return "warn";
  if (ev.truncated) return "warn";
  if (ev.deprecated) return "warn";
  if (ev.kind === "request") return "req";
  if (ev.kind === "response") {
    if (call && call.status === "error") return "error";
    // A cancelled task delivered no result but is not an error, so it reads as a
    // caution like the TUI row, reusing the warn tone rather than success green.
    if (call && call.status === "cancelled") return "warn";
    return "resp";
  }
  return "resp";
};
const statusOf = (ev, call) => {
  if (ev.kind === "invalid") return "bad";
  if (ev.warning) return "warn";
  if (ev.truncated) return "warn";
  if (ev.deprecated) return "warn";
  if (!call) return "";
  if (ev.kind === "response") {
    if (call.status === "error") return "error";
    return call.status;
  }
  return call.status === "pending" || call.status === "superseded" ? call.status : "";
};
const renderEvent = (ev) => {
  const call = ev.call_index == null ? null : calls[ev.call_index];
  const tone = toneOf(ev, call);
  const st = statusOf(ev, call);
  const raw = ev.text || textOf(ev.raw);
  const warning = ev.warning ? "<details open><summary>Warning</summary><pre>" + esc(ev.warning) + "</pre></details>" : "";
  const deprecated = ev.deprecated ? "<details open><summary>Deprecated</summary><pre>" + esc(ev.deprecated) + "</pre></details>" : "";
  const callBlock = call ? "<details><summary>Correlated call</summary><pre>" + esc(JSON.stringify(call, null, 2)) + "</pre></details>" : "";
  return "<article class=\"event tone-" + tone + "\">" +
    "<div class=\"head\">" +
      "<div class=\"seq\">#" + ev.seq + "</div>" +
      "<div class=\"dir\">" + esc(ev.direction) + " " + esc(ev.kind) + "</div>" +
      "<div class=\"method\">" + esc(ev.method || ev.id || ev.text || "") + "</div>" +
      "<div class=\"status " + esc(st) + "\">" + esc(st) + "</div>" +
      "<div class=\"time\">" + esc(fmtTime(ev.timestamp)) + "</div>" +
    "</div>" +
    warning +
    deprecated +
    "<details open><summary>Frame</summary><pre>" + esc(raw) + "</pre></details>" +
    callBlock +
  "</article>";
};
const list = document.getElementById("events");
list.innerHTML = events.length ? events.map(renderEvent).join("") : "<div class=\"empty\">No events</div>";
const rows = [...document.querySelectorAll(".event")];
document.getElementById("q").addEventListener("input", (e) => {
  const tokens = e.target.value.trim().split(/\s+/).filter(Boolean);
  events.forEach((ev, i) => {
    rows[i].hidden = tokens.length > 0 && !eventMatches(ev, tokens);
  });
});
</script>
</body>
</html>
`))
