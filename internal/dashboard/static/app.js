// fleetreeve console. Plain DOM, no dependencies. All text goes in through
// textContent, so nothing a host reports can inject markup.
"use strict";

const STATES = {
  ok:      { label: "In sync",       color: "var(--ok)",      icon: "M4 10.5 8 14.5 16 6" },
  changed: { label: "Changed",       color: "var(--changed)", icon: "M15.5 8A6 6 0 0 0 5 6.5L3.5 8M4.5 12a6 6 0 0 0 10.5 1.5l1.5-1.5M3.5 4v4h4M16.5 16v-4h-4" },
  drift:   { label: "Drift",         color: "var(--drift)",   icon: "M10 3.5 17.5 16h-15zM10 8.5v3.5M10 14.2h.01" },
  failed:  { label: "Failed",        color: "var(--failed)",  icon: "M10 17a7 7 0 1 0 0-14 7 7 0 0 0 0 14zM7.5 7.5l5 5M12.5 7.5l-5 5" },
  stale:   { label: "Stale",         color: "var(--stale)",   icon: "M10 17a7 7 0 1 0 0-14 7 7 0 0 0 0 14zM10 6v4.5l3 1.5" },
  unknown: { label: "Never reported", color: "var(--unknown)", icon: "M10 17a7 7 0 1 0 0-14 7 7 0 0 0 0 14zM8 8a2 2 0 1 1 3 1.7c-.6.4-1 .8-1 1.6M10 13.8h.01" },
};
const ORDER = ["ok", "changed", "drift", "failed", "stale", "unknown"];
const RANK = { failed: 0, stale: 1, drift: 2, unknown: 3, changed: 4, ok: 5 };
const EXPLAIN = {
  ok: "Its last run found everything as declared.",
  changed: "Its last run had to correct something, and did.",
  drift: "A dry run found differences that have not been applied.",
  failed: "Its last run could not bring the host into its desired state.",
  stale: "It missed at least two check-ins. It may be down or unable to reach the server.",
  unknown: "It is known to the server but has never reported.",
};

const $ = (id) => document.getElementById(id);

// Dates and times are shown the same for everyone, whatever the browser's
// language: 24-hour clock, day before month (21-11-2026 15:04).
const two = (n) => String(n).padStart(2, "0");
const fmtTime = (t, seconds) => { const d = new Date(t); return `${two(d.getHours())}:${two(d.getMinutes())}${seconds ? ":" + two(d.getSeconds()) : ""}`; };
const fmtDate = (t) => { const d = new Date(t); return `${two(d.getDate())}-${two(d.getMonth() + 1)}-${d.getFullYear()}`; };
const fmtDateTime = (t) => `${fmtDate(t)} ${fmtTime(t)}`;
const fmtDay = (t) => new Date(t).toLocaleDateString("en-GB", { weekday: "long", day: "numeric", month: "long", year: "numeric" });
const SVG = "http://www.w3.org/2000/svg";

function el(tag, attrs, ...kids) {
  const node = tag === "svg" || tag === "path" || tag === "g" || tag === "line" || tag === "circle" || tag === "text" || tag === "rect"
    ? document.createElementNS(SVG, tag) : document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === false || v == null) continue;
    if (k === "class") node.setAttribute("class", v);
    else if (k === "text") node.textContent = v;
    else if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
    else if (k === "style") for (const [p, val] of Object.entries(v)) node.style.setProperty(p, val);
    else node.setAttribute(k, v === true ? "" : v);
  }
  for (const kid of kids.flat()) if (kid != null && kid !== false) node.append(kid);
  return node;
}

function stateIcon(state) {
  const s = STATES[state] || STATES.unknown;
  return el("svg", { class: "state-icon", viewBox: "0 0 20 20", "aria-hidden": "true", style: { "--c": s.color } }, el("path", { d: s.icon }));
}
const pill = (state) => el("span", { class: "pill" }, stateIcon(state), (STATES[state] || STATES.unknown).label);

function ago(seconds) {
  if (seconds == null) return "never";
  if (seconds < 90) return Math.max(0, Math.round(seconds)) + " s ago";
  if (seconds < 5400) return Math.round(seconds / 60) + " min ago";
  if (seconds < 172800) return Math.round(seconds / 3600) + " h ago";
  return Math.round(seconds / 86400) + " d ago";
}
const every = (s) => !s ? "on demand" : s % 3600 === 0 ? `every ${s / 3600} h` : `every ${Math.round(s / 60)} min`;
const plural = (n, one, many) => `${n} ${n === 1 ? one : many || one + "s"}`;

// ---- state ----
let data = null;
let hosts = [];
const view = { q: "", state: "", sort: "state", asc: true, all: false };
const PAGE = 20; // rows shown before "Show all"

function hostState(h) {
  const rank = { ok: 1, changed: 2, drift: 3, stale: 4, failed: 5, error: 5 };
  let s = "unknown";
  for (const r of h.reports || []) if ((rank[r.state] || 0) > (rank[s] || 0)) s = r.state;
  return s === "error" ? "failed" : s;
}

function digest(o) {
  hosts = (o.hosts || []).map((h) => {
    const reports = h.reports || [];
    // The run that decides the host's state is the one worth showing.
    const state = hostState(h);
    const lead = reports.slice().sort((a, b) => (RANK[a.state === "error" ? "failed" : a.state] ?? 9) - (RANK[b.state === "error" ? "failed" : b.state] ?? 9))[0];
    return {
      name: h.name, groups: h.groups || [], source: h.source, state, lead,
      manifest: reports.map((r) => r.manifest || "–").join(", "),
      age: lead ? lead.age_seconds : null,
      total: lead ? lead.summary.total : 0,
      changed: reports.reduce((n, r) => n + r.summary.changed + r.summary.pending, 0),
      failed: reports.reduce((n, r) => n + r.summary.failed, 0),
      held: reports.reduce((n, r) => n + (r.summary.held || 0), 0),
    };
  });
}

function counts() {
  const c = Object.fromEntries(ORDER.map((s) => [s, 0]));
  for (const h of hosts) c[h.state]++;
  return c;
}

// ---- render ----
function render() {
  const o = data, c = counts(), n = hosts.length;
  const healthy = c.ok + c.changed;
  $("server-name").textContent = o.demo ? "demo data" : (o.server || "–").replace(/^https?:\/\//, "");
  $("server-name").title = o.server || "";
  $("server-version").textContent = o.version || "–";
  $("demo-banner").hidden = !o.demo;
  $("error-banner").hidden = !o.error;
  $("error-banner").textContent = o.error ? `The fleetreeve server cannot be reached, so this may be out of date: ${o.error}` : "";
  $("live").classList.toggle("is-down", !!o.error);
  $("live-text").textContent = o.error ? "Server unreachable" : `Live, updated every ${o.poll_seconds} s`;

  const attention = c.failed + c.stale;
  $("lede").textContent = n === 0 ? "The server does not know any hosts yet. Enrol one to see it here."
    : attention ? `${plural(attention, "host needs", "hosts need")} attention out of ${n}.`
    : c.drift ? `Nothing is failing. ${plural(c.drift, "host has", "hosts have")} unapplied differences.`
    : `All ${n} reporting hosts are in their desired state.`.replace(`All ${n} reporting`, c.unknown ? `${healthy} of ${n}` : `All ${n}`);
  $("nav-hosts").textContent = n;
  $("nav-attention").hidden = !attention;
  $("nav-attention").textContent = attention;

  // Hero figure.
  const pct = n ? (healthy / n) * 100 : 0;
  $("compliance").textContent = n ? (pct === 100 ? "100" : pct.toFixed(1)) : "–";
  $("compliance-note").textContent = n ? `${healthy} of ${plural(n, "host")} matched their manifest on the last run, or were corrected by it.` : "";

  renderMatrix();
  renderTiles(c, n);
  renderTrend(o.history || []);
  renderSegments(c, n);
  renderAttention();
  renderChips(c, n);
  renderTable();
}

function legendItems(target, withCounts, c) {
  target.replaceChildren(...ORDER.filter((s) => !withCounts || c[s] > 0).map((s) =>
    el("li", {}, withCounts ? stateIcon(s) : el("span", { class: "cell", "data-state": s, style: { "--c": STATES[s].color } }),
      el("span", { text: STATES[s].label }), withCounts && el("b", { text: c[s] }))));
}

function renderMatrix() {
  const groups = new Map();
  for (const h of hosts) {
    const g = h.groups[0] || "ungrouped";
    if (!groups.has(g)) groups.set(g, []);
    groups.get(g).push(h);
  }
  const blocks = [...groups.entries()].sort((a, b) => b[1].length - a[1].length || a[0].localeCompare(b[0])).map(([name, list]) => {
    list.sort((a, b) => RANK[a.state] - RANK[b.state] || a.name.localeCompare(b.name));
    const bad = list.filter((h) => h.state === "failed" || h.state === "stale").length;
    return el("div", { role: "listitem" },
      el("div", { class: "group-name" }, name, el("span", { text: bad ? `${bad} of ${list.length} ${bad === 1 ? "needs" : "need"} attention` : plural(list.length, "host") })),
      el("div", { class: "cells" }, list.map((h) => el("button", {
        class: "cell", type: "button", "data-state": h.state, style: { "--c": STATES[h.state].color },
        "aria-label": `${h.name}: ${STATES[h.state].label}`,
        onclick: () => openHost(h.name),
        onmouseenter: (e) => tip(e.currentTarget, h.name, `${STATES[h.state].label}${h.lead ? ", last run " + ago(h.age) : ""}`),
        onmouseleave: hideTip, onfocus: (e) => tip(e.currentTarget, h.name, STATES[h.state].label), onblur: hideTip,
      }))));
  });
  $("matrix").replaceChildren(...blocks);
  legendItems($("matrix-legend"), false);
}

function renderTiles(c, n) {
  const tiles = [
    { key: "", label: "Hosts", value: n, note: "known to the server" },
    { key: "ok", label: "In sync", value: c.ok, note: "nothing to correct" },
    { key: "changed", label: "Changed", value: c.changed, note: "corrected on the last run" },
    { key: "drift", label: "Drift", value: c.drift, note: "differences not applied" },
    { key: "failed", label: "Failed", value: c.failed, note: "could not converge" },
    { key: "stale", label: "Stale", value: c.stale, note: "missed check-ins" },
    { key: "unknown", label: "Never reported", value: c.unknown, note: "enrolled, no run yet" },
  ];
  $("tiles").replaceChildren(...tiles.map((t) => el("button", {
    class: "tile", type: "button", "aria-pressed": String(view.state === t.key && t.key !== ""),
    onclick: () => { view.state = view.state === t.key ? "" : t.key; render(); if (t.key) $("hosts").scrollIntoView({ behavior: "smooth" }); },
  }, el("span", { class: "tile-label" }, t.key && stateIcon(t.key), t.label),
     el("span", { class: "tile-value", text: t.value }),
     el("span", { class: "tile-note", text: t.note }))));
}

// One series, so no legend: the panel title names it. Hover shows the
// values at that moment.
function renderTrend(history) {
  const box = $("trend");
  const pts = history.filter((s) => s.hosts > 0).map((s) => ({ t: s.t * 1000, v: (s.healthy / s.hosts) * 100, s }));
  if (pts.length < 2) {
    box.replaceChildren(el("div", { class: "chart-empty", text: "The trend starts when this dashboard does. It will appear after the next update." }));
    $("trend-range").textContent = "";
    return;
  }
  const W = Math.max(420, box.clientWidth || 640), H = 230, m = { l: 40, r: 56, t: 14, b: 26 };
  const lo = Math.max(0, Math.floor((Math.min(...pts.map((p) => p.v)) - 4) / 5) * 5), hi = 100;
  const t0 = pts[0].t, t1 = pts[pts.length - 1].t;
  const x = (t) => m.l + ((t - t0) / (t1 - t0 || 1)) * (W - m.l - m.r);
  const y = (v) => m.t + (1 - (v - lo) / (hi - lo || 1)) * (H - m.t - m.b);
  const clock = (t) => fmtTime(t);
  const path = pts.map((p, i) => (i ? "L" : "M") + x(p.t).toFixed(1) + " " + y(p.v).toFixed(1)).join("");
  const last = pts[pts.length - 1];
  const svg = el("svg", { viewBox: `0 0 ${W} ${H}`, role: "img", "aria-label": `Share of hosts in their desired state, now ${last.v.toFixed(1)} percent` });
  for (const g of [lo, (lo + hi) / 2, hi]) {
    svg.append(el("line", { class: "grid", x1: m.l, x2: W - m.r, y1: y(g), y2: y(g) }),
      el("text", { class: "axis", x: m.l - 8, y: y(g) + 4, "text-anchor": "end", text: Math.round(g) + "%" }));
  }
  svg.append(el("text", { class: "axis", x: m.l, y: H - 6, text: clock(t0) }),
    el("text", { class: "axis", x: W - m.r, y: H - 6, "text-anchor": "end", text: clock(t1) }),
    el("path", { class: "area", d: `${path}L${x(t1)} ${y(lo)}L${x(t0)} ${y(lo)}Z` }),
    el("path", { class: "line", d: path }),
    el("circle", { class: "dot", cx: x(last.t), cy: y(last.v), r: 4.5 }),
    el("text", { class: "end-label", x: x(last.t) + 9, y: y(last.v) + 4.5, text: last.v.toFixed(1) + "%" }));
  const cross = el("line", { class: "cross", y1: m.t, y2: H - m.b, visibility: "hidden" });
  const mark = el("circle", { class: "dot", r: 4.5, visibility: "hidden" });
  const hit = el("rect", { x: m.l, y: m.t, width: W - m.l - m.r, height: H - m.t - m.b, fill: "transparent" });
  hit.addEventListener("mousemove", (e) => {
    const r = svg.getBoundingClientRect(), px = ((e.clientX - r.left) / r.width) * W;
    const p = pts.reduce((a, b) => (Math.abs(x(b.t) - px) < Math.abs(x(a.t) - px) ? b : a));
    cross.setAttribute("x1", x(p.t)); cross.setAttribute("x2", x(p.t)); cross.setAttribute("visibility", "visible");
    mark.setAttribute("cx", x(p.t)); mark.setAttribute("cy", y(p.v)); mark.setAttribute("visibility", "visible");
    const s = p.s, rest = [s.drift && `${s.drift} drift`, s.failed && `${s.failed} failed`, s.stale && `${s.stale} stale`, s.unknown && `${s.unknown} never reported`].filter(Boolean);
    tipAt(e.clientX, e.clientY, `${p.v.toFixed(1)}% at ${clock(p.t)}`, `${s.healthy} of ${s.hosts} hosts${rest.length ? "; " + rest.join(", ") : ""}`);
  });
  hit.addEventListener("mouseleave", () => { cross.setAttribute("visibility", "hidden"); mark.setAttribute("visibility", "hidden"); hideTip(); });
  svg.append(cross, mark, hit);
  box.replaceChildren(svg);
  const span = (t1 - t0) / 60000;
  $("trend-range").textContent = span < 90 ? `Last ${Math.max(1, Math.round(span))} min, since this dashboard started` : `Last ${(span / 60).toFixed(1)} h`;
}

function renderSegments(c, n) {
  const bar = $("segbar");
  bar.setAttribute("aria-label", ORDER.filter((s) => c[s]).map((s) => `${c[s]} ${STATES[s].label.toLowerCase()}`).join(", "));
  bar.replaceChildren(...ORDER.filter((s) => c[s] > 0).map((s) => el("span", {
    "data-state": s, style: { "--c": STATES[s].color, flex: `${c[s]} 0 0` },
    onmouseenter: (e) => tip(e.currentTarget, `${c[s]} ${STATES[s].label.toLowerCase()}`, `${((c[s] / n) * 100).toFixed(1)}% of hosts. ${EXPLAIN[s]}`),
    onmouseleave: hideTip,
  })));
  legendItems($("seg-legend"), true, c);
  const reports = (data.status && data.status.reports) || [];
  const sum = (f) => reports.reduce((a, r) => a + f(r), 0);
  const facts = [
    ["Resources under management", sum((r) => r.summary.total).toLocaleString()],
    ["Corrected on the last runs", sum((r) => r.summary.changed).toLocaleString()],
    ["Failed resources", sum((r) => r.summary.failed).toLocaleString()],
    ["Pushed from a controller", plural(reports.filter((r) => r.source === "push").length, "run")],
  ];
  $("facts").replaceChildren(...facts.flatMap(([k, v]) => [el("dt", { text: k }), el("dd", { text: v })]));
}

function renderAttention() {
  const list = hosts.filter((h) => h.state === "failed" || h.state === "stale").sort((a, b) => RANK[a.state] - RANK[b.state] || a.name.localeCompare(b.name));
  if (!list.length) {
    $("attention-list").replaceChildren(el("p", { class: "all-clear" }, stateIcon("ok"), hosts.length ? "No host is failing or has gone quiet." : "Nothing to show yet."));
    return;
  }
  $("attention-list").replaceChildren(...list.slice(0, 8).map((h) => el("button", { class: "issue", type: "button", onclick: () => openHost(h.name) },
    stateIcon(h.state), el("strong", { text: h.name }),
    el("span", { text: h.state === "stale" ? `No report for more than twice its interval (${every(h.lead.interval_seconds)}).` : (h.lead.error || `${plural(h.failed, "resource")} failed on ${h.lead.manifest || "its manifest"}.`) }),
    el("time", { text: ago(h.age) }))),
    ...(list.length > 8 ? [el("p", { class: "muted", style: { "padding-top": "10px" }, text: `And ${list.length - 8} more. Filter the host list by state to see them all.` })] : []));
}

function renderChips(c, n) {
  $("chips").replaceChildren(el("button", { class: "chip", type: "button", "aria-pressed": String(view.state === ""), onclick: () => { view.state = ""; render(); } }, "All ", el("b", { text: n })),
    ...ORDER.filter((s) => c[s] > 0).map((s) => el("button", { class: "chip", type: "button", "aria-pressed": String(view.state === s),
      onclick: () => { view.state = view.state === s ? "" : s; render(); } }, stateIcon(s), STATES[s].label + " ", el("b", { text: c[s] }))));
}

function renderTable() {
  const q = view.q.trim().toLowerCase();
  let rows = hosts.filter((h) => (!view.state || h.state === view.state) &&
    (!q || (h.name + " " + h.groups.join(" ") + " " + h.manifest + " " + STATES[h.state].label).toLowerCase().includes(q)));
  const key = { state: (h) => RANK[h.state], name: (h) => h.name, groups: (h) => h.groups.join(","), manifest: (h) => h.manifest,
    age: (h) => h.age ?? Infinity, total: (h) => h.total, changed: (h) => h.changed, failed: (h) => h.failed }[view.sort];
  rows.sort((a, b) => { const x = key(a), y = key(b); return (x < y ? -1 : x > y ? 1 : a.name.localeCompare(b.name)) * (view.asc ? 1 : -1); });
  const num = (n, bad) => el("td", { class: "num" + (n ? (bad ? " bad" : "") : " zero"), text: n });
  const shown = view.all || view.q || view.state ? rows : rows.slice(0, PAGE);
  $("more").hidden = shown.length === rows.length;
  $("more").textContent = `Show all ${rows.length} hosts`;
  $("rows").replaceChildren(...shown.map((h) => el("tr", { tabindex: "0", onclick: () => openHost(h.name), onkeydown: (e) => { if (e.key === "Enter") openHost(h.name); } },
    el("td", {}, pill(h.state)), el("td", { class: "host-name" }, h.name, h.held > 0 && el("span", { class: "tag is-held", title: "Rolled back by hand and left alone until released", text: `${h.held} on hold` })),
    el("td", {}, h.groups.map((g) => el("span", { class: "tag", text: g }))),
    el("td", { text: h.manifest || "–" }), el("td", { class: "num", text: ago(h.age) }),
    num(h.total), num(h.changed), num(h.failed, true))));
  $("empty").hidden = rows.length > 0;
  $("hosts-count").textContent = rows.length === hosts.length ? plural(hosts.length, "host") : `${rows.length} of ${hosts.length} hosts`;
  for (const th of document.querySelectorAll("th[data-sort]")) {
    if (th.dataset.sort === view.sort) th.setAttribute("aria-sort", view.asc ? "ascending" : "descending"); else th.removeAttribute("aria-sort");
  }
}

// ---- host drawer ----
async function openHost(name) {
  hideTip();
  $("drawer-title").textContent = name;
  $("drawer-sub").textContent = "Loading…";
  $("drawer-body").replaceChildren();
  $("drawer").classList.add("is-open"); $("drawer").setAttribute("aria-hidden", "false"); $("scrim").hidden = false; $("drawer").focus();
  let d;
  try {
    const r = await fetch("api/hosts/" + encodeURIComponent(name));
    if (r.status === 401) { location.replace("login"); return; }
    d = await r.json();
    if (!r.ok) throw new Error(d.error || r.statusText);
  } catch (e) {
    $("drawer-sub").textContent = "";
    $("drawer-body").replaceChildren(el("p", { class: "err", text: `This host's details could not be loaded: ${e.message}` }));
    return;
  }
  const h = d.host, state = d.state === "error" ? "failed" : d.state;
  $("drawer-sub").textContent = EXPLAIN[state];
  const body = [el("dl", { class: "kv" },
    el("dt", { text: "State" }), el("dd", {}, pill(state)),
    el("dt", { text: "Roles" }), el("dd", {}, h.groups.length ? h.groups.map((g) => el("span", { class: "tag", text: g })) : "none"),
    el("dt", { text: "Known from" }), el("dd", { text: h.source === "registered" ? `Enrolment, on ${fmtDate(h.registered)}` : "The server's inventory file" }),
    el("dt", { text: "History" }), el("dd", {}, el("button", { class: "link", type: "button", text: "Every run of this host", onclick: () => openLog(name) })))];
  if (d.can_act) body.push(el("div", { class: "host-actions" },
    el("button", { class: "button", type: "button", text: "Apply now", onclick: async () => { await ask(name, "apply"); } }),
    el("button", { class: "button is-quiet", type: "button", text: "Dry run", onclick: async () => { await ask(name, "plan"); } }),
    el("button", { class: "link", type: "button", text: canExec ? "Run a command…" : "More…", onclick: () => openRun(name) })));
  const pending = h.pending_actions || [];
  if (pending.length) body.push(el("section", { class: "run" },
    el("div", { class: "run-head" }, el("strong", { text: "Waiting for the host" })),
    pending.map((a) => el("div", { class: "res" },
      el("div", { class: "res-id" }, stateIcon("stale"), describe(a) + " ", a.resource && el("code", { text: a.resource }), a.command && el("code", { text: a.command })),
      el("div", { class: "change", text: `Asked ${ago((Date.now() - Date.parse(a.queued)) / 1000)}${a.requested_by ? " by " + a.requested_by : ""}. The host usually picks this up within a minute.` }),
      d.can_act && el("div", { class: "actions" }, el("button", { class: "link", type: "button", text: "Withdraw this request",
        onclick: () => act(name, "DELETE", "/" + encodeURIComponent(a.id)) }))))));
  if (!d.reports.length) body.push(el("p", { class: "muted", text: "No run has been reported for this host yet." }));
  for (const rep of d.reports) {
    const s = rep.summary, entry = (h.reports || []).find((e) => (e.manifest || "") === (rep.manifest || "")) || {};
    const notable = (rep.results || []).filter((r) => r.status !== "ok" || r.refresh || r.held);
    const stat = (n, label) => el("span", {}, el("b", { text: n }), " " + label);
    body.push(el("section", { class: "run" },
      el("div", { class: "run-head" }, el("strong", { text: rep.manifest || "Unnamed manifest" }), pill(entry.state === "error" ? "failed" : entry.state || "ok")),
      el("div", { class: "run-stats" },
        el("span", { text: `${rep.mode === "plan" ? "Dry run" : "Applied"} ${ago(entry.age_seconds)}, ${rep.source === "push" ? "pushed from a controller" : "pulled " + every(rep.interval)}` })),
      el("div", { class: "run-stats" }, stat(s.total, "resources"), stat(s.changed + s.pending, rep.mode === "plan" ? "to change" : "changed"), stat(s.failed, "failed"), stat(s.skipped, "skipped"), stat(s.refreshed, "refreshed"),
        el("span", { text: `${(s.ms / 1000).toFixed(1)} s` })),
      rep.error && el("div", { class: "res" }, el("div", { class: "err", text: rep.error })),
      rep.cached && el("div", { class: "quiet", text: "The server could not be reached, so the host enforced its cached catalog." }),
      notable.length ? notable.map((r) => el("div", { class: "res" },
        el("div", { class: "res-id" }, stateIcon({ changed: "changed", pending: "drift", failed: "failed", rolledback: "failed", skipped: "stale", ok: "ok" }[r.status] || "unknown"), el("code", { text: r.id })),
        (r.changes || []).map((c) => el("div", { class: "change" }, `${c.prop}: `, el("code", { text: c.from }), " to ", el("code", { text: c.to }))),
        r.refresh && el("div", { class: "change", text: rep.mode === "plan" ? "Would be restarted or re-run." : "Restarted or re-run because something it depends on changed." }),
        r.drifted && el("div", { class: "change", text: "Changed on the host by something other than fleetreeve." }),
        r.status === "rolledback" && el("div", { class: "change", text: "This change was undone automatically." }),
        r.held && el("div", { class: "change", text: "Held after a manual rollback: not changed until it is released." }),
        r.orphan && el("div", { class: "change", text: r.orphan === "remove" ? "Removed because it left the manifest." : "No longer in the manifest; left in place." }),
        r.error && el("div", { class: "err", text: r.error }),
        r.origin && el("small", { text: `Declared in ${r.origin}` })))
        : el("div", { class: "quiet", text: s.total ? `All ${s.total} resources were already in their desired state.` : "This run managed no resources." }),
      applied(rep),
      (rep.actions || []).map((a) => el("div", { class: "res" },
        el("div", { class: "res-id" }, stateIcon(a.status === "failed" ? "failed" : "ok"), `${a.kind === "release" ? "Release" : "Rollback"} of `, el("code", { text: a.resource })),
        el("div", { class: a.status === "failed" ? "err" : "change", text: `${a.status === "failed" ? "Failed" : a.status === "released" ? "Released" : "Restored"}${a.requested_by ? ", asked by " + a.requested_by : ""}. ${a.detail || ""}` }))),
      versions(name, rep, entry, pending, d.can_act)));
  }
  if (!d.can_act && d.reports.some((r) => (r.restorable || []).length)) {
    body.push(el("p", { class: "muted", text: "You can look but not roll back here: that needs an account with the operator role, on a console set up for it." }));
  }
  $("drawer-body").replaceChildren(...body);
}
// ---- everything applied on a host ----
// Where a resource is declared: a module, a role, or another manifest.
function sourceOf(origin) {
  if (!origin) return { name: "Left the manifest", kind: "" };
  const path = origin.replace(/:\d+$/, "");
  const mod = path.match(/(?:^|\/)modules\/([^/]+)\//);
  if (mod) return { name: mod[1], kind: "module" };
  return { name: path.split("/").pop().replace(/\.ya?ml$/, ""), kind: /(^|\/)roles\//.test(path) ? "role" : "manifest" };
}
const TYPE_WORDS = { file: "Files", line: "Lines in files", package: "Packages", service: "Services", exec: "Commands", user: "Users", group: "Groups" };
let appliedBy = "source";
try { appliedBy = localStorage.getItem("fleetreeve-applied-by") || "source"; } catch (e) { /* storage is optional */ }

// The full list of what the host is kept to, from its last report:
// grouped by where it is declared or by type, and filterable.
function applied(rep) {
  const all = (rep.results || []).filter((r) => !r.orphan).map((r) => {
    const cut = r.id.indexOf("["), type = cut < 0 ? r.id : r.id.slice(0, cut);
    return { r, type, title: cut < 0 ? "" : r.id.slice(cut + 1, -1), source: sourceOf(r.origin) };
  });
  if (!all.length) return null;
  const list = el("div", { class: "applied-groups" });
  const input = el("input", { type: "search", placeholder: "Filter by name, type or source", autocomplete: "off", "aria-label": "Filter the resources of this host" });
  const modes = [["source", "By source"], ["type", "By type"]].map(([mode, label]) =>
    el("button", { class: "chip", type: "button", "aria-pressed": String(appliedBy === mode), text: label, onclick: () => {
      appliedBy = mode;
      try { localStorage.setItem("fleetreeve-applied-by", mode); } catch (e) { /* storage is optional */ }
      modes.forEach((b, i) => b.setAttribute("aria-pressed", String(["source", "type"][i] === mode)));
      draw();
    } }));
  const when = ({ r }) => {
    if (r.status === "failed") return "Failed in the last run";
    if (r.status === "skipped") return "Skipped in the last run";
    if (r.status === "pending") return "Differs from the manifest";
    if (r.held) return "On hold";
    if (!r.since) return "Already as declared";
    return `${r.created ? "Created" : "Changed"} by fleetreeve ${ago((Date.now() - Date.parse(r.since)) / 1000)}`;
  };
  const icon = ({ r }) => stateIcon({ changed: "changed", pending: "drift", failed: "failed", rolledback: "failed", skipped: "stale" }[r.status] || (r.held ? "drift" : "ok"));
  function draw() {
    const q = input.value.trim().toLowerCase();
    const shown = all.filter((x) => !q || `${x.r.id} ${x.source.name} ${x.source.kind}`.toLowerCase().includes(q));
    const groups = new Map();
    for (const x of shown) {
      const key = appliedBy === "type" ? x.type : x.source.name;
      if (!groups.has(key)) groups.set(key, { key, kind: appliedBy === "type" ? "" : x.source.kind, items: [] });
      groups.get(key).items.push(x);
    }
    const sorted = [...groups.values()].sort((a, b) => b.items.length - a.items.length || a.key.localeCompare(b.key));
    list.replaceChildren(...sorted.map((g) => {
      const off = g.items.filter((x) => ["failed", "skipped", "pending"].includes(x.r.status)).length;
      const group = el("details", { class: "applied-group" },
        el("summary", {},
          el("span", { class: "applied-name", text: appliedBy === "type" ? TYPE_WORDS[g.key] || g.key : g.key }),
          g.kind && el("span", { class: "tag", text: g.kind }),
          off ? el("span", { class: "applied-off", text: `${off} not as declared` }) : null,
          el("span", { class: "applied-count", text: g.items.length })),
        g.items.map((x) => el("div", { class: "applied-row", title: x.r.origin ? `Declared in ${x.r.origin}` : "" },
          icon(x),
          el("span", { class: "applied-what" },
            appliedBy !== "type" && el("span", { class: "applied-type", text: x.type }),
            el("code", { text: x.title || x.r.id }),
            appliedBy === "type" && el("span", { class: "applied-from", text: x.source.name })),
          el("span", { class: "applied-when", text: when(x) }))));
      group.open = !!(q || off);
      return group;
    }));
    if (!shown.length) list.append(el("div", { class: "quiet", text: "Nothing on this host matches that filter." }));
  }
  draw();
  input.addEventListener("input", draw);
  const sources = new Set(all.map((x) => x.source.name)).size;
  return el("div", { class: "applied" },
    el("div", { class: "versions-head", text: `Everything applied on this host: ${plural(all.length, "resource")} from ${plural(sources, "source")}` }),
    el("div", { class: "applied-tools" }, el("label", { class: "search" }, input), el("div", { class: "chips" }, modes)),
    list);
}

// What the host says it can put back, with the means to ask for it.
function versions(host, rep, entry, pending, canAct) {
  const list = rep.restorable || [];
  if (!list.length) return null;
  const waiting = (kind, id) => pending.some((a) => a.kind === kind && a.resource === id && a.manifest === rep.manifest);
  const within = entry.interval_seconds ? `within ${Math.max(1, Math.round(entry.interval_seconds / 60))} minutes` : "the next time it runs";
  return el("div", { class: "versions" },
    el("div", { class: "versions-head", text: "Earlier versions kept on the host" }),
    list.map((v) => {
      const held = !!v.held, kind = held ? "release" : "rollback";
      const row = el("div", { class: "res" });
      const ask = () => {
        const question = held
          ? `Release ${v.id}? The host's next run brings it back to what the manifest says.`
          : `Roll ${v.id} back on ${host}? It is restored to how it was before the change of ${fmtDateTime(v.time)}, and then left alone until you release it.`;
        const confirmRow = el("div", { class: "confirm" }, el("p", { text: `${question} The host does this ${within}.` }),
          el("button", { class: "button is-danger", type: "button", text: held ? "Release" : "Roll back",
            onclick: () => act(host, "POST", "", { manifest: rep.manifest, kind, resource: v.id }) }),
          el("button", { class: "link", type: "button", text: "Keep as it is", onclick: () => confirmRow.remove() }));
        row.querySelector(".confirm")?.remove();
        row.append(confirmRow);
      };
      row.append(
        el("div", { class: "res-id" }, stateIcon(held ? "drift" : "changed"), el("code", { text: v.id })),
        el("div", { class: "change", text: held
          ? `On hold since ${fmtDateTime(v.held)}: rolled back by hand, and not changed by runs until released.`
          : `Last changed ${fmtDateTime(v.time)}${(v.changes || []).length ? " (" + v.changes.map((c) => c.prop).join(", ") + ")" : ""}. ${plural(v.remaining, "earlier version")} kept.` }),
        waiting(kind, v.id) ? el("div", { class: "change", text: "A request for this is waiting for the host." })
          : canAct && (held || v.remaining > 0) && el("div", { class: "actions" }, el("button", { class: "link", type: "button", text: held ? "Release…" : "Roll back…", onclick: ask })));
      return row;
    }));
}

// Ask one host for a run or dry run, then show it afresh.
async function ask(host, kind) {
  try { await call("POST", "api/run", { hosts: [host], kind }); } catch (e) { alert(`That did not work: ${e.message}`); }
  openHost(host);
}

// Send a request to the console's own backend, then show the host afresh.
async function act(host, method, suffix, body) {
  try {
    const r = await fetch("api/hosts/" + encodeURIComponent(host) + "/actions" + suffix, {
      method, headers: { "Content-Type": "application/json", "X-Fleetreeve-Console": "1" }, body: body ? JSON.stringify(body) : undefined });
    if (r.status === 401) { location.replace("login"); return; }
    if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error || r.statusText);
  } catch (e) {
    alert(`That did not work: ${e.message}`);
  }
  openHost(host);
}

function closeDrawer() { $("drawer").classList.remove("is-open"); $("drawer").setAttribute("aria-hidden", "true"); $("scrim").hidden = true; }

// ---- tooltip ----
function tipAt(x, y, title, text) {
  const t = $("tip");
  t.replaceChildren(el("b", { text: title }), text || "");
  t.hidden = false;
  const r = t.getBoundingClientRect();
  t.style.left = Math.max(8, Math.min(window.innerWidth - r.width - 8, x - r.width / 2)) + "px";
  t.style.top = (y - r.height - 14 < 8 ? y + 18 : y - r.height - 14) + "px";
}
function tip(target, title, text) { const r = target.getBoundingClientRect(); tipAt(r.left + r.width / 2, r.top, title, text); }
function hideTip() { $("tip").hidden = true; }

// ---- who is signed in ----
async function whoami() {
  try {
    const me = await (await fetch("api/me")).json();
    canExec = !!me.can_exec;
    $("nav-run").hidden = !me.can_act;
    if (!me.login) return; // a console without accounts
    $("who").hidden = false;
    $("who-name").textContent = me.user;
    $("who-role").textContent = { admin: "Manages accounts", operator: "May roll back" }[me.role] || "May look";
    $("change-password").hidden = false;
    $("nav-accounts").hidden = !me.can_manage;
    minPassword = me.min_password || 12;
  } catch (_) { /* shown as unreachable by load() */ }
}

// ---- run: apply now, dry run, command ----
let canExec = false;

// What a request asks for, in words.
function describe(a) {
  return { apply: "Apply the configuration", plan: "Dry run", rollback: "Roll back", release: "Release", command: "Command" }[a.kind] || a.kind;
}

// One request and what became of it. `r` is its result, once there is one.
function requestRow(a, r, waitingNote) {
  const state = !r ? "stale" : r.status === "failed" ? "failed" : "ok";
  const head = el("div", { class: "res-id" }, stateIcon(state), el("strong", { text: a.host }), " ", describe(a),
    a.resource && el("code", { text: a.resource }));
  const row = el("div", { class: "res" }, head);
  if (a.command) row.append(el("div", { class: "change" }, el("code", { text: a.command })));
  if (!r) {
    row.append(el("div", { class: "change", text: waitingNote || "Waiting for the host. It usually picks this up within a minute." }));
    return row;
  }
  const when = ago((Date.now() - Date.parse(r.done)) / 1000);
  const verdict = a.kind === "command" ? (r.exit === 0 && r.status !== "failed" ? "Finished" : `Exit status ${r.exit}`) : r.status === "failed" ? "Failed" : "Done";
  row.append(el("div", { class: r.status === "failed" ? "err" : "change", text: `${verdict} ${when}${a.requested_by ? ", asked by " + a.requested_by : ""}. ${r.detail || ""}` }));
  if (r.output) row.append(el("pre", { class: "output", tabindex: "0", text: r.output }));
  else if (a.kind === "command" && !canExec) row.append(el("div", { class: "change", text: "Commands and their output are shown to admins only." }));
  return row;
}

let runTimer;
async function openRun(preset) {
  clearInterval(runTimer);
  openPanel("Run", "Have hosts act now instead of at their next scheduled run.");
  const roles = [...new Set(hosts.flatMap((h) => h.groups))].sort();
  const scope = el("select", { "aria-label": "Which hosts" },
    el("option", { value: "all", text: `All hosts (${hosts.length})` }),
    roles.map((g) => el("option", { value: "role:" + g, text: `Role ${g} (${hosts.filter((h) => h.groups.includes(g)).length})` })),
    hosts.map((h) => h.name).sort().map((n) => el("option", { value: "host:" + n, selected: preset === n, text: n })));
  const targets = () => scope.value === "all" ? hosts.map((h) => h.name)
    : scope.value.startsWith("role:") ? hosts.filter((h) => h.groups.includes(scope.value.slice(5))).map((h) => h.name)
    : [scope.value.slice(5)];
  const kinds = [["apply", "Apply the configuration now", "Each host does what its next scheduled run would do."],
    ["plan", "Dry run", "Nothing is changed. Shows what a run would do."]];
  if (canExec) kinds.push(["command", "Run a command", "As root, on every chosen host. Admins only; every command is logged."]);
  const command = el("textarea", { rows: "3", placeholder: "for example: systemctl status nginx --no-pager", spellcheck: "false", "aria-label": "Command" });
  const timeout = el("select", { "aria-label": "Stop the command after" }, [[30, "30 seconds"], [60, "1 minute"], [300, "5 minutes"], [600, "10 minutes"]].map(([v, t]) => el("option", { value: v, selected: v === 60, text: "Stop after " + t })));
  const commandBox = el("div", { class: "command-box", hidden: true }, command, timeout);
  const kindOf = () => form.querySelector("input[name=kind]:checked").value;
  const choices = kinds.map(([value, label, note], i) => el("label", { class: "choice" },
    el("input", { type: "radio", name: "kind", value, checked: i === 0, onchange: () => { commandBox.hidden = kindOf() !== "command"; } }),
    el("span", {}, el("strong", { text: label }), el("small", { text: note }))));
  const error = el("p", { class: "signin-error", role: "alert", hidden: true });
  const live = el("section", { class: "run", hidden: true }, el("div", { class: "run-head" }, el("strong", { text: "This request" })), el("div", { id: "run-live" }));
  const recent = el("section", { class: "run", hidden: true }, el("div", { class: "run-head" }, el("strong", { text: "Requests carried out lately" }),
    el("button", { class: "link", type: "button", text: "Every run is in the log", onclick: () => openLog() })), el("div", { id: "run-recent" }));
  const form = el("form", { class: "run-form", onsubmit: (e) => {
    e.preventDefault();
    error.hidden = true;
    const list = targets(), kind = kindOf(), cmd = command.value.trim();
    if (kind === "command" && !cmd) { error.textContent = "Enter a command."; error.hidden = false; return; }
    const n = plural(list.length, "host");
    const question = kind === "apply" ? `Apply the configuration on ${n} now?`
      : kind === "plan" ? `Do a dry run on ${n}? Nothing is changed. Until its next real run, each host is shown with what the dry run found.`
      : `Run this command as root on ${n}? ${cmd}`;
    confirmIn(form, question, kind === "command" ? "Run the command" : kind === "plan" ? "Do the dry run" : "Apply now", async () => {
      form.querySelector(".confirm")?.remove();
      try {
        const r = await call("POST", "api/run", { hosts: list, kind, command: cmd, timeout: Number(timeout.value) });
        const refused = Object.entries(r.failed || {});
        if (refused.length) { error.textContent = `Not asked of ${refused.map(([h, m]) => `${h} (${m})`).join(", ")}.`; error.hidden = false; }
        watch(r.queued || []);
      } catch (err) { error.textContent = err.message; error.hidden = false; }
    });
  } }, el("label", { class: "field" }, "On", scope), el("div", { class: "choices", role: "radiogroup", "aria-label": "What to do" }, choices), commandBox, error,
    el("button", { class: "button", type: "submit", text: "Continue…" }));
  $("drawer-body").replaceChildren(form, live, recent);

  let mine = [];
  const refresh = async () => {
    let data;
    try { data = await call("GET", "api/actions"); } catch (_) { return; }
    const byId = new Map(data.results.map((r) => [r.id, r]));
    if (mine.length) {
      live.hidden = false;
      live.querySelector("#run-live").replaceChildren(...mine.map((a) => requestRow(a, byId.get(a.id))));
      if (mine.every((a) => byId.has(a.id))) clearInterval(runTimer);
    }
    const others = data.results.filter((r) => !mine.some((a) => a.id === r.id)).slice(0, 12);
    recent.hidden = !others.length;
    recent.querySelector("#run-recent").replaceChildren(...others.map((r) => requestRow(r, r)));
  };
  const watch = (queued) => {
    mine = queued;
    clearInterval(runTimer);
    refresh();
    runTimer = setInterval(() => { if (!$("drawer").classList.contains("is-open")) clearInterval(runTimer); else refresh(); }, 2500);
  };
  refresh();
}
$("nav-run").addEventListener("click", () => openRun());

// ---- log ----
// Every run and request, newest first; older ones load while scrolling.
let logWatch;
const REQUESTS = { apply: "Apply now", plan: "Dry run", rollback: "Rollback of", release: "Release of", command: "Command" };
// What kind of job an entry is, shown as a labelled badge with its own
// icon and colour: the host's own scheduled run, a run pushed from a
// controller, a dry run (nothing changed), or something asked for by a person.
const JOBS = {
  scheduled: { label: "Scheduled", note: "The host's own run, on its schedule or when asked to run now", icon: "M10 3.5a6.5 6.5 0 1 0 0 13 6.5 6.5 0 0 0 0-13zM10 6.5V10l2.5 1.5" },
  pushed: { label: "Pushed", note: "Applied from a controller with fleetreeve apply", icon: "M10 16V4.5M5.500 9 10 4.500 14.500 9" },
  dryrun: { label: "Dry run", note: "Nothing was changed; shows what a run would do", icon: "M2.500 10s2.700-4.500 7.500-4.500S17.500 10 17.500 10s-2.700 4.500-7.500 4.500S2.500 10 2.500 10zM10 8.200a1.800 1.800 0 1 0 0 3.600 1.800 1.800 0 0 0 0-3.600z" },
  request: { label: "Request", note: "Asked for by a person, from the console or the API", icon: "M10 9.500a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM4 16.500c.700-3 3-4.500 6-4.500s5.300 1.500 6 4.500" },
};
const jobOf = (e) => e.kind === "request" ? "request" : e.mode === "plan" ? "dryrun" : e.source === "push" ? "pushed" : "scheduled";
function jobBadge(e) {
  const kind = jobOf(e), j = JOBS[kind];
  return el("span", { class: "job is-" + kind, title: j.note },
    el("svg", { viewBox: "0 0 20 20", "aria-hidden": "true" }, el("path", { d: j.icon })), j.label);
}
function logText(e) {
  if (e.kind === "request") {
    const how = e.request === "command" && e.exit != null ? (e.exit === 0 ? "finished" : `exit status ${e.exit}`) : e.status;
    return [REQUESTS[e.request] || e.request, " ", e.resource && el("code", { text: e.resource }), ` ${how || ""}${e.requested_by ? ", asked by " + e.requested_by : ""}. ${e.detail || ""}`];
  }
  const s = e.summary || {}, parts = [];
  if (s.changed) parts.push(`${s.changed} changed`);
  if (s.pending) parts.push(`${plural(s.pending, "difference")} found`);
  if (s.failed) parts.push(`${s.failed} failed`);
  if (s.skipped) parts.push(`${s.skipped} skipped`);
  if (s.refreshed) parts.push(`${s.refreshed} refreshed`);
  if (s.rolled_back) parts.push(`${s.rolled_back} rolled back`);
  if (s.held) parts.push(`${s.held} on hold`);
  if (s.forgotten) parts.push(`${s.forgotten} no longer tracked`);
  const from = e.mode === "plan" && e.source === "push" ? "from a controller, " : "";
  return [`${e.manifest ? e.manifest + ": " : ""}${from}${parts.length ? parts.join(", ") : `all ${s.total ?? ""} resources as declared`}`,
    s.ms != null ? ` (${(s.ms / 1000).toFixed(1)} s)` : "", e.cached ? ", on the cached catalog" : ""];
}
function logRow(e, withHost) {
  const state = e.kind === "request" ? (e.status === "failed" || (e.exit != null && e.exit !== 0) ? "failed" : "changed") : e.state === "error" ? "failed" : e.state || "ok";
  const changes = e.changes || [];
  return el("div", { class: "log-row" + (state === "ok" ? " is-quiet" : "") },
    el("time", { datetime: e.time, text: fmtTime(e.time, true) }),
    stateIcon(state),
    jobBadge(e),
    el("div", { class: "log-what" },
      el("div", {}, withHost && el("button", { class: "log-host", type: "button", text: e.host, onclick: () => openHost(e.host) }), withHost && " ", logText(e)),
      e.error && el("div", { class: "err", text: e.error }),
      changes.slice(0, 8).map((c) => el("div", { class: "log-change" }, el("code", { text: c.id }), " ",
        c.error ? el("span", { class: "err", text: c.error })
          : [(c.props || []).join("; ").replaceAll(" -> ", " → ") || (c.orphan === "remove" ? "removed: it left the manifest" : c.orphan ? "no longer tracked" : c.held ? "on hold" : c.refresh ? "restarted or re-run" : c.status),
            c.drifted && " (was changed outside fleetreeve)"])),
      changes.length > 8 && el("div", { class: "log-change", text: `and ${changes.length - 8} more` })));
}
async function openLog(host) {
  clearInterval(runTimer);
  logWatch?.disconnect();
  openPanel("Log", "Every run hosts reported and every request they carried out. Newest first; scroll down to go back in time.");
  let cursor = "", busy = false, ended = false, day = "", shown = 0, generation = 0;
  const pick = el("select", { "aria-label": "Which host" }, el("option", { value: "", text: "All hosts" }),
    hosts.map((h) => h.name).sort().map((n) => el("option", { value: n, selected: n === host, text: n })));
  const only = el("button", { class: "chip", type: "button", "aria-pressed": "false", text: "Only changes and failures" });
  const again = el("button", { class: "link", type: "button", text: "Newest" });
  const list = el("div", { class: "log" });
  const more = el("button", { class: "more", type: "button", text: "Show older" });
  const note = el("p", { class: "muted", hidden: true });
  const load = async () => {
    if (busy || ended) return;
    busy = true;
    more.textContent = "Loading…";
    const mine = generation, q = new URLSearchParams({ limit: "100" });
    if (pick.value) q.set("host", pick.value);
    if (only.getAttribute("aria-pressed") === "true") q.set("only", "changes");
    if (cursor) q.set("before", cursor);
    try {
      const page = await call("GET", "api/history?" + q);
      if (mine !== generation) return;
      for (const e of page.events) {
        const d = fmtDay(e.time);
        if (d !== day) { day = d; list.append(el("h3", { class: "log-day", text: d })); }
        list.append(logRow(e, !pick.value));
      }
      shown += page.events.length;
      cursor = page.next;
      ended = !cursor;
      more.hidden = ended;
      note.hidden = !ended;
      note.textContent = shown ? `That is everything: ${plural(shown, "entry", "entries")}. The log starts here.` : "Nothing in the log matches this yet.";
    } catch (err) {
      if (mine !== generation) return;
      ended = true; more.hidden = true; note.hidden = false;
      note.textContent = `The log could not be loaded: ${err.message}`;
    } finally {
      if (mine === generation) { busy = false; more.textContent = "Show older"; }
    }
  };
  const restart = () => {
    generation++; cursor = ""; busy = false; ended = false; day = ""; shown = 0;
    list.replaceChildren(); more.hidden = false; note.hidden = true;
    $("drawer-body").scrollTop = 0;
    load();
  };
  pick.addEventListener("change", restart);
  only.addEventListener("click", () => { only.setAttribute("aria-pressed", String(only.getAttribute("aria-pressed") !== "true")); restart(); });
  again.addEventListener("click", restart);
  more.addEventListener("click", load);
  $("drawer-body").replaceChildren(el("div", { class: "log-tools" }, pick, only, again), list, more, note);
  // Reaching the end of what is shown fetches the next, older, page.
  logWatch = new IntersectionObserver((entries) => { if (entries.some((x) => x.isIntersecting)) load(); }, { root: $("drawer-body"), rootMargin: "400px" });
  logWatch.observe(more);
  load();
}
$("nav-log").addEventListener("click", () => openLog());

// ---- accounts ----
let minPassword = 12;
const ROLES = [["viewer", "Viewer: may look"], ["operator", "Operator: may also roll back"], ["admin", "Admin: may also manage accounts"]];

function openPanel(title, sub) {
  hideTip();
  $("drawer-title").textContent = title;
  $("drawer-sub").textContent = sub;
  $("drawer-body").replaceChildren();
  $("drawer").classList.add("is-open"); $("drawer").setAttribute("aria-hidden", "false"); $("scrim").hidden = false; $("drawer").focus();
}

async function call(method, path, body) {
  const r = await fetch(path, { method, headers: { "Content-Type": "application/json", "X-Fleetreeve-Console": "1" }, body: body ? JSON.stringify(body) : undefined });
  if (r.status === 401) { location.replace("login"); throw new Error("signed out"); }
  const data = r.status === 204 ? {} : await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data.error || r.statusText);
  return data;
}

// A generated password is shown once, to pass on to its owner.
function passwordNote(name, password) {
  const copy = el("button", { class: "link", type: "button", text: "Copy", onclick: async () => {
    try { await navigator.clipboard.writeText(password); copy.textContent = "Copied"; } catch (_) { copy.textContent = "Select and copy it"; }
  } });
  return el("div", { class: "secret", role: "status" },
    el("p", {}, `Password for ${name}: `, el("code", { text: password }), " ", copy),
    el("p", { class: "muted", text: "It is shown this once. Pass it on safely; they can replace it under Change password." }));
}

async function openAccounts(note) {
  openPanel("Accounts", "Who may sign in to this console, and what they may do.");
  let data;
  try { data = await call("GET", "api/users"); } catch (e) {
    $("drawer-body").replaceChildren(el("p", { class: "err", text: `The accounts could not be loaded: ${e.message}` }));
    return;
  }
  const error = el("p", { class: "signin-error", role: "alert", hidden: true });
  const attempt = async (fn) => { error.hidden = true; try { openAccounts(await fn()); } catch (e) { error.textContent = e.message; error.hidden = false; } };
  const rows = data.users.map((u) => {
    const you = u.name === data.you;
    const role = el("select", { "aria-label": `Role of ${u.name}`, disabled: u.fixed,
      onchange: (e) => attempt(async () => { await call("PATCH", "api/users/" + encodeURIComponent(u.name), { role: e.target.value }); }) },
      ROLES.map(([value, label]) => el("option", { value, selected: value === u.role, text: label })));
    const row = el("div", { class: "res account" },
      el("div", { class: "res-id" }, u.name, you && el("span", { class: "tag", text: "you" }), u.fixed && el("span", { class: "tag", text: "set in the environment" })),
      role);
    if (!u.fixed) row.append(el("div", { class: "actions" },
      el("button", { class: "link", type: "button", text: "Reset password…", onclick: () => confirmIn(row,
        `Give ${u.name} a new password? Their current one stops working and they are signed out.`, "Reset password",
        () => attempt(async () => { const r = await call("PATCH", "api/users/" + encodeURIComponent(u.name), { reset: true }); return passwordNote(u.name, r.password); })) }),
      !you && el("button", { class: "link is-danger", type: "button", text: "Remove…", onclick: () => confirmIn(row,
        `Remove ${u.name}? They are signed out and can no longer sign in.`, "Remove account",
        () => attempt(async () => { await call("DELETE", "api/users/" + encodeURIComponent(u.name)); })) })));
    else row.append(el("p", { class: "muted", text: "A shared account from the console's settings (FLEETREEVE_DASHBOARD_AUTH). Remove it there once everyone has their own." }));
    return row;
  });
  const name = el("input", { type: "text", placeholder: "name", autocomplete: "off", autocapitalize: "none", spellcheck: "false", "aria-label": "Name of the new account" });
  const newRole = el("select", { "aria-label": "Role of the new account" }, ROLES.map(([value, label]) => el("option", { value, text: label })));
  const add = el("form", { class: "add-account", onsubmit: (e) => { e.preventDefault(); attempt(async () => {
    const r = await call("POST", "api/users", { name: name.value.trim(), role: newRole.value });
    return passwordNote(r.name, r.password);
  }); } }, name, newRole, el("button", { class: "button", type: "submit", text: "Add account" }));
  $("drawer-body").replaceChildren(...[note, error,
    el("section", { class: "run" }, el("div", { class: "run-head" }, el("strong", { text: plural(data.users.length, "account") })), rows),
    el("section", { class: "run" }, el("div", { class: "run-head" }, el("strong", { text: "Add an account" })),
      el("div", { class: "res" }, add, el("p", { class: "muted", text: "The password is generated and shown to you once." })))].filter(Boolean));
}

function confirmIn(row, question, label, go) {
  row.querySelector(".confirm")?.remove();
  const box = el("div", { class: "confirm" }, el("p", { text: question }),
    el("button", { class: "button is-danger", type: "button", text: label, onclick: go }),
    el("button", { class: "link", type: "button", text: "Keep as it is", onclick: () => box.remove() }));
  row.append(box);
}

function openChangePassword() {
  openPanel("Change password", "Replace the password you sign in with.");
  const field = (label, auto) => { const i = el("input", { type: "password", autocomplete: auto, required: true }); return [el("label", {}, label, i), i]; };
  const [l1, current] = field("Current password", "current-password");
  const [l2, next] = field(`New password (at least ${minPassword} characters)`, "new-password");
  const [l3, again] = field("New password, again", "new-password");
  const error = el("p", { class: "signin-error", role: "alert", hidden: true });
  const form = el("form", { class: "password-form", onsubmit: async (e) => {
    e.preventDefault();
    error.hidden = true;
    const problem = next.value.length < minPassword ? `The new password needs at least ${minPassword} characters.` : next.value !== again.value ? "The two new passwords are not the same." : "";
    if (problem) { error.textContent = problem; error.hidden = false; return; }
    try {
      await call("POST", "api/password", { current: current.value, new: next.value });
      $("drawer-body").replaceChildren(el("p", { class: "all-clear" }, stateIcon("ok"), "Your password is changed. Other devices you were signed in on are signed out."));
    } catch (err) { error.textContent = err.message; error.hidden = false; }
  } }, l1, l2, l3, error, el("button", { class: "button", type: "submit", text: "Change password" }));
  $("drawer-body").replaceChildren(form);
  current.focus();
}
$("nav-accounts").addEventListener("click", () => openAccounts());
$("change-password").addEventListener("click", openChangePassword);
$("signout").addEventListener("click", async () => {
  await fetch("api/logout", { method: "POST", headers: { "X-Fleetreeve-Console": "1" } }).catch(() => {});
  location.replace("login");
});

// ---- data ----
let timer;
async function load() {
  clearTimeout(timer);
  try {
    const r = await fetch("api/overview");
    if (r.status === 401) { location.replace("login"); return; } // signed out, or the session ended
    if (!r.ok) throw new Error(r.statusText);
    data = await r.json();
    if (!data.status) data.status = { reports: [], missing: [] };
    digest(data);
    render();
  } catch (e) {
    $("live").classList.add("is-down");
    $("live-text").textContent = "Dashboard unreachable";
  }
  timer = setTimeout(load, Math.max(5, (data && data.poll_seconds) || 15) * 1000);
}

// ---- wiring ----
$("refresh").addEventListener("click", load);
$("q").addEventListener("input", (e) => { view.q = e.target.value; renderTable(); });
for (const th of document.querySelectorAll("th[data-sort]")) th.querySelector("button").addEventListener("click", () => {
  if (view.sort === th.dataset.sort) view.asc = !view.asc; else { view.sort = th.dataset.sort; view.asc = true; }
  renderTable();
});
$("more").addEventListener("click", () => { view.all = true; renderTable(); });
$("drawer-close").addEventListener("click", closeDrawer);
$("scrim").addEventListener("click", closeDrawer);
document.addEventListener("keydown", (e) => { if (e.key === "Escape") closeDrawer(); });
window.addEventListener("resize", () => data && renderTrend(data.history || []));

// Highlight the rail item of the section in view.
const spy = new IntersectionObserver((entries) => {
  for (const e of entries) {
    if (!e.isIntersecting) continue;
    for (const a of document.querySelectorAll(".nav-item")) a.classList.toggle("is-current", a.dataset.nav === e.target.id);
  }
}, { rootMargin: "-30% 0px -60% 0px" });
for (const id of ["overview", "hosts", "attention"]) spy.observe($(id));

function setTheme(t) {
  document.documentElement.dataset.theme = t;
  $("theme").querySelector("span").textContent = t === "dark" ? "Light theme" : "Dark theme";
  try { localStorage.setItem("fleetreeve-theme", t); } catch (_) { /* private mode */ }
}
let saved = null;
try { saved = localStorage.getItem("fleetreeve-theme"); } catch (_) { /* private mode */ }
setTheme(saved || (matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light"));
$("theme").addEventListener("click", () => setTheme(document.documentElement.dataset.theme === "dark" ? "light" : "dark"));

whoami();
load();
