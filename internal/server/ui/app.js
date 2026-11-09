"use strict";
const $ = (s) => document.querySelector(s);
let records = [], source = "", info = {}, noZones = false;

// The token lives in sessionStorage (gone when the tab closes) unless the
// user ticks "Remember on this browser".
const KEY = "lds-token";
function token() {
  try { return sessionStorage.getItem(KEY) || localStorage.getItem(KEY) || ""; } catch { return ""; }
}
function setToken(t, remember) {
  try {
    sessionStorage.setItem(KEY, t);
    if (remember) localStorage.setItem(KEY, t); else localStorage.removeItem(KEY);
  } catch {}
}
function clearToken() { try { sessionStorage.removeItem(KEY); localStorage.removeItem(KEY); } catch {} }

class AuthError extends Error {}

async function api(method, path, body) {
  const headers = {};
  if (body) headers["Content-Type"] = "application/json";
  if (token()) headers["Authorization"] = "Bearer " + token();
  const res = await fetch(path, { method, headers, body: body ? JSON.stringify(body) : undefined, credentials: "omit", cache: "no-store" });
  const data = await res.json().catch(() => ({}));
  if (res.status === 401) throw new AuthError(data.error || "Sign in needed");
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}

function showSignin(msg) {
  $("#app").hidden = true;
  $("#signout").hidden = true;
  $("#signin").hidden = false;
  $("#signin-error").textContent = msg || "";
  $("#status").replaceChildren();
  $("#signin-form").elements.token.focus();
}

function el(tag, attrs = {}, ...kids) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") n.className = v; else if (k.startsWith("on")) n.addEventListener(k.slice(2), v); else n.setAttribute(k, v);
  }
  for (const k of kids.flat()) if (k != null && k !== false) n.append(k);
  return n;
}

function ago(iso) {
  if (!iso || iso.startsWith("0001")) return "never";
  const s = Math.round((Date.now() - new Date(iso)) / 1000);
  if (s < 60) return "just now";
  if (s < 3600) return Math.round(s / 60) + " min ago";
  if (s < 86400) return Math.round(s / 3600) + " h ago";
  return new Date(iso).toLocaleDateString();
}

// Group records under the zone they belong to: the longest wildcard domain
// that covers them, otherwise their parent domain.
function zoneOf(name, zones) {
  const bare = name.replace(/^\*\./, "");
  let best = "";
  for (const z of zones) if ((bare === z || bare.endsWith("." + z)) && z.length > best.length) best = z;
  if (best) return best;
  const parts = bare.split(".");
  return parts.length > 1 ? parts.slice(1).join(".") : bare;
}

function render() {
  const q = $("#q").value.trim().toLowerCase();
  const shown = records.filter((r) =>
    (!source || r.source === source) &&
    (!q || [r.name, r.value, r.container, r.comment].some((f) => f && f.toLowerCase().includes(q))));

  const zones = records.filter((r) => r.type === "WILDCARD").map((r) => r.name.slice(2));
  const groups = new Map();
  for (const r of shown) {
    const z = zoneOf(r.name, zones);
    if (!groups.has(z)) groups.set(z, []);
    groups.get(z).push(r);
  }

  $("#count").textContent = shown.length === records.length
    ? `${records.length} record${records.length === 1 ? "" : "s"}`
    : `${shown.length} of ${records.length} records`;

  const root = $("#records");
  root.replaceChildren();
  if (!records.length) {
    root.append(el("div", { class: "empty" },
      el("p", {}, noZones ? "No records yet. Once a domain is set, add records above or from the command line:" : "No records yet. Add one above, or from the command line:"),
      el("p", {}, el("code", {}, "lds add '*.home.lan' 192.168.1.5"))));
    return;
  }
  if (!shown.length) {
    root.append(el("div", { class: "empty" }, "No records match this filter."));
    return;
  }

  for (const [zone, recs] of [...groups].sort((a, b) => a[0].localeCompare(b[0]))) {
    const wild = recs.find((r) => r.type === "WILDCARD" && r.name === "*." + zone);
    const rows = recs.filter((r) => r !== wild);
    const head = el("h2", {}, zone);
    const tbody = el("tbody");
    if (wild) tbody.append(row(wild, zone));
    for (const r of rows) tbody.append(row(r, zone));
    root.append(el("section", { class: "zone" }, head, el("table", {}, tbody)));
  }
}

function row(r, zone) {
  const isWild = r.type === "WILDCARD";
  const sub = r.name === zone ? "" : r.name.slice(0, r.name.length - zone.length - 1);
  const suffix = sub ? "." + zone : zone;
  const nameNode = isWild || r.name.includes("*")
    ? el("span", {}, el("span", { class: "sub" }, sub), el("span", { class: "suffix" }, suffix))
    : el("a", { href: "https://" + r.name, target: "_blank", rel: "noopener", title: "Open https://" + r.name },
        el("span", { class: "sub" }, sub), el("span", { class: "suffix" }, suffix));

  const meta = [];
  if (isWild) meta.push(`every other name in ${zone}`);
  meta.push(r.source === "docker" ? `from container ${r.container}` : r.source === "replica" ? "copied from the primary" : `added ${ago(r.created_at)}`);
  if (r.comment) meta.push(r.comment);

  const act = r.source === "manual" && !info.read_only
    ? el("button", { type: "button", "aria-label": "Remove " + r.name, onclick: () => remove(r) }, "Remove")
    : r.source === "docker"
      ? el("span", { class: "lock", title: "Managed by Docker labels" }, "auto")
      : el("span", { class: "lock", title: "Change it on the primary server" }, "copy");

  return el("tr", { class: r.active ? "" : "inactive" },
    el("td", { class: "name" }, nameNode),
    el("td", { class: "arrow", "aria-hidden": "true" }, "→"),
    el("td", { class: "value" },
      el("span", { class: "type t-" + r.type }, isWild ? "*" : r.type),
      el("span", {}, r.value),
      el("span", { class: "meta" }, meta.join(" — ")),
      r.warning && el("span", { class: "warn" }, r.warning)),
    el("td", { class: "act" }, act));
}

async function remove(r) {
  if (!confirm(`Remove ${r.name} → ${r.value}?`)) return;
  try { await api("DELETE", "/api/records/" + encodeURIComponent(r.id)); hint(`Removed ${r.name}.`); }
  catch (e) {
    if (e instanceof AuthError) { clearToken(); showSignin("Your token was not accepted. Sign in again."); return; }
    hint(e.message, true);
  }
  load();
}

function hint(msg, error) { const h = $("#hint"); h.textContent = msg; h.classList.toggle("error", !!error); }

async function load() {
  try {
    if (info.read_only === undefined) info = await api("GET", "/api/info");
    const [list, status] = await Promise.all([api("GET", "/api/records"), api("GET", "/api/status")]);
    records = list.records || [];
    $("#signin").hidden = true;
    $("#app").hidden = false;
    $("#signout").hidden = !info.auth_required || !token();
    noZones = !status.zones || status.zones.length === 0;
    $("#hint").hidden = noZones;
    $("#readonly").hidden = !info.read_only;
    $("#nozones").hidden = !noZones || !!info.read_only;
    $("#add").hidden = !!info.read_only || noZones;
    renderStatus(status);
    render();
  } catch (e) {
    if (e instanceof AuthError) {
      if (token()) { clearToken(); showSignin("That token was not accepted."); } else showSignin();
      return;
    }
    $("#status").replaceChildren(el("li", { class: "bad" }, "Can't reach the LDS server"));
  }
}

function renderStatus(s) {
  const items = [];
  const d = s.dnsmasq;
  if (d && d.enabled) items.push(el("li", { class: d.running ? "ok" : "bad", title: d.last_error || "" },
    d.running ? `dnsmasq running, reloaded ${ago(d.last_reload)}` : `dnsmasq stopped${d.last_error ? ": " + d.last_error : ""}`));
  else items.push(el("li", {}, "dnsmasq not managed"));
  const k = s.docker;
  if (k && k.enabled) items.push(el("li", { class: k.connected ? "ok" : "bad", title: k.last_error || "" },
    k.connected ? `Docker synced ${ago(k.last_sync)}` : "Docker unreachable"));
  if (s.upstreams && s.upstreams.length) items.push(el("li", { class: "ok" }, "Forwarding to " + s.upstreams.join(", ")));
  $("#status").replaceChildren(...items);
}

$("#add").addEventListener("submit", async (e) => {
  e.preventDefault();
  const f = e.target, v = (k) => f.elements[k].value;
  try {
    const r = await api("POST", "/api/records", { name: v("name"), value: v("value"), comment: v("comment") });
    hint(r.warning ? `Added ${r.name}, but: ${r.warning}` : `Added ${r.name} → ${r.value}.`, !!r.warning);
    f.reset(); f.elements.name.focus();
  } catch (err) {
    if (err instanceof AuthError) { clearToken(); showSignin("Your token was not accepted. Sign in again."); return; }
    hint(err.message, true);
  }
  load();
});
$("#signin-form").addEventListener("submit", (e) => {
  e.preventDefault();
  const f = e.target;
  setToken(f.elements.token.value.trim(), f.elements.remember.checked);
  f.reset();
  load();
});
$("#signout").addEventListener("click", () => { clearToken(); showSignin(); });
$("#q").addEventListener("input", render);
document.querySelectorAll(".filters button").forEach((b) => b.addEventListener("click", () => {
  source = b.dataset.src;
  document.querySelectorAll(".filters button").forEach((x) => x.setAttribute("aria-pressed", String(x === b)));
  render();
}));
load();
setInterval(() => { if ($("#signin").hidden) load(); }, 10000);

// ---- Settings ----

const SOURCE = {
  "default": () => "Default",
  "docker-compose": (f) => el("span", {}, "From docker-compose.yml (", el("code", {}, f.env), ")"),
  "dashboard": () => el("span", { class: "changed" }, "Changed here"),
  "generated": () => "Generated by LDS",
};

function showView(view) {
  document.querySelectorAll(".tabs button").forEach((b) => {
    if (b.dataset.view === view) b.setAttribute("aria-current", "page"); else b.removeAttribute("aria-current");
  });
  $("#view-records").hidden = view !== "records";
  $("#view-settings").hidden = view !== "settings";
  if (view === "settings") loadSettings();
}
document.querySelectorAll(".tabs button, [data-goto]").forEach((b) =>
  b.addEventListener("click", () => showView(b.dataset.view || b.dataset.goto)));

let settingsFields = [];

async function loadSettings() {
  try {
    const res = await api("GET", "/api/settings");
    $("#settings-error").hidden = !res.error;
    $("#settings-error").textContent = res.error || "";
    renderSettings(res.fields);
  } catch (e) {
    if (e instanceof AuthError) { clearToken(); showSignin("Your token was not accepted. Sign in again."); return; }
    settingsHint(e.message, true);
  }
}

function fieldText(f) {
  if (Array.isArray(f.value)) return f.value.length ? f.value.join(", ") : "none";
  if (f.kind === "bool") return f.value ? "on" : "off";
  return f.value === "" || f.value == null ? "not set" : String(f.value);
}

function renderSettings(fields) {
  settingsFields = fields;
  const groups = new Map();
  for (const f of fields) {
    if (!groups.has(f.group)) groups.set(f.group, []);
    groups.get(f.group).push(f);
  }
  const root = $("#settings-fields");
  root.replaceChildren();
  for (const [group, list] of groups) {
    const fs = el("fieldset", {}, el("legend", {}, group));
    for (const f of list) fs.append(settingRow(f));
    root.append(fs);
  }
}

function settingRow(f) {
  const id = "set-" + f.key;
  const help = el("span", { class: "help" }, f.help);
  if (!f.editable) {
    return el("div", { class: "setting readonly" },
      el("div", { class: "label" }, f.label, help),
      el("div", { class: "control" },
        el("span", { class: "value" }, fieldText(f)),
        el("span", { class: "source" }, f.source === "generated" ? "Generated by LDS" : el("span", {}, "Change with ", el("code", {}, f.env), " in docker-compose.yml"))));
  }
  let input;
  if (f.kind === "bool") {
    input = el("input", { type: "checkbox", id });
    input.checked = !!f.value;
  } else if (f.kind === "int") {
    input = el("input", { type: "number", id, inputmode: "numeric" });
    input.value = String(f.value);
  } else {
    input = el("input", { type: "text", id, spellcheck: "false",
      placeholder: f.kind === "list" ? "comma-separated" : f.kind === "ip" ? "IP address, or empty for off" : "" });
    input.value = Array.isArray(f.value) ? f.value.join(", ") : (f.value || "");
  }
  input.dataset.key = f.key;
  input.dataset.kind = f.kind;
  input.dataset.orig = f.kind === "bool" ? String(!!f.value) : input.value;
  const source = el("span", { class: "source" }, (SOURCE[f.source] || SOURCE["default"])(f));
  if (f.source === "dashboard") {
    source.append(el("button", { type: "button", class: "link", onclick: () => saveSettings({ reset: [f.key] }, `${f.label} is back to its docker-compose.yml value.`) },
      "Use docker-compose.yml value"));
  }
  return el("div", { class: "setting" },
    el("label", { for: id }, f.label, help),
    el("div", { class: "control" }, input, source));
}

function settingValue(input) {
  const kind = input.dataset.kind;
  if (kind === "bool") return input.checked;
  if (kind === "int") return Number(input.value);
  if (kind === "list") return input.value.split(/[\s,]+/).filter(Boolean);
  return input.value.trim();
}

function settingsHint(msg, error) {
  const h = $("#settings-hint");
  h.textContent = msg;
  h.classList.toggle("error", !!error);
}

async function saveSettings(change, done) {
  try {
    const preview = await api("PUT", "/api/settings", { ...change, dry_run: true });
    if (preview.warnings && preview.warnings.length &&
        !confirm(preview.warnings.join("\n\n") + "\n\nSave anyway?")) {
      settingsHint("Nothing was changed.");
      return;
    }
    const res = await api("PUT", "/api/settings", change);
    renderSettings(res.fields);
    settingsHint(done || "Saved. The changes are live.");
    load();
  } catch (e) {
    if (e instanceof AuthError) { clearToken(); showSignin("Your token was not accepted. Sign in again."); return; }
    settingsHint(e.message, true);
  }
}

$("#settings-form").addEventListener("submit", (e) => {
  e.preventDefault();
  const set = {};
  for (const input of $("#settings-fields").querySelectorAll("input[data-key]")) {
    const now = input.dataset.kind === "bool" ? String(input.checked) : input.value;
    if (now !== input.dataset.orig) set[input.dataset.key] = settingValue(input);
  }
  if (!Object.keys(set).length) { settingsHint("Nothing to save: no setting was changed."); return; }
  saveSettings({ set });
});
