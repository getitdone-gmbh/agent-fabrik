// agentd web UI: a small single-page app on top of the /v1 API. No build
// step, no dependencies. Server data is only ever inserted as text
// (textContent), never as HTML.
"use strict";

// ---------- storage ----------

const store = {
  get(k) {
    try { return sessionStorage.getItem(k) ?? localStorage.getItem(k); } catch { return null; }
  },
  set(k, v, persist) {
    try {
      sessionStorage.setItem(k, v);
      if (persist) localStorage.setItem(k, v);
    } catch { /* storage unavailable: keep it in memory only */ }
  },
  del(k) {
    try { sessionStorage.removeItem(k); localStorage.removeItem(k); } catch { /* ignore */ }
  },
};

const S = {
  token: store.get("agentd.token") || "",
  actAs: store.get("agentd.tenant") || "",
  me: null,        // GET /v1/me (multi-tenant only)
  tenancy: false,  // multi-tenant mode
  admin: false,
  sessions: {},    // agent -> session id of the open chat
};

const editable = () => S.tenancy && S.me && S.me.tenant.id !== "system";

// ---------- DOM helpers ----------

function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "class") el.className = v;
    else if (k === "value") el.value = v;
    else if (k === "checked" || k === "disabled" || k === "selected") el[k] = !!v;
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid == null || kid === false) continue;
    el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return el;
}

const $ = (sel, root = document) => root.querySelector(sel);
const view = () => $("#view");

function mount(...nodes) {
  const v = view();
  v.replaceChildren(...nodes.flat(Infinity).filter((n) => n != null && n !== false));
  window.scrollTo(0, 0);
}

let toastTimer;
function toast(msg, bad) {
  const t = $("#toast");
  t.textContent = msg;
  t.className = "toast" + (bad ? " bad" : "");
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.hidden = true; }, bad ? 6000 : 2500);
}

const fmt = (n) => (n == null ? "–" : Number(n).toLocaleString());
const nameRe = /^[a-zA-Z0-9][a-zA-Z0-9_-]{0,47}$/;
const NAME_PATTERN = "[a-zA-Z0-9][a-zA-Z0-9_\\-]{0,47}"; // HTML pattern (v-mode regex)
const enc = encodeURIComponent;

// ---------- API ----------

class APIError extends Error {
  constructor(status, msg) { super(msg); this.status = status; }
}

function headers(extra) {
  const hd = Object.assign({}, extra);
  if (S.token) hd.Authorization = "Bearer " + S.token;
  if (S.actAs) hd["X-Tenant"] = S.actAs;
  return hd;
}

async function api(method, path, body, opts = {}) {
  const init = { method, headers: headers(opts.headers), cache: "no-store" };
  if (body !== undefined) {
    init.body = typeof body === "string" ? body : JSON.stringify(body);
    if (!init.headers["Content-Type"]) init.headers["Content-Type"] = "application/json";
  }
  const res = await fetch(path, init);
  if (res.status === 401 && !opts.quiet401) { signOut("Your session is no longer valid. Please sign in again."); }
  if (!res.ok) {
    let msg = res.status + " " + res.statusText;
    try { const j = await res.json(); if (j && j.error && j.error.message) msg = j.error.message; } catch { /* not JSON */ }
    throw new APIError(res.status, msg);
  }
  if (res.status === 204) return null;
  const ct = res.headers.get("Content-Type") || "";
  return ct.includes("json") ? res.json() : res.text();
}

// ---------- session ----------

async function signIn(token, remember) {
  S.token = token;
  if (token) store.set("agentd.token", token, remember);
  await boot();
}

function signOut(msg) {
  S.token = ""; S.actAs = ""; S.me = null; S.admin = false; S.tenancy = false;
  store.del("agentd.token"); store.del("agentd.tenant");
  showLogin(msg);
}

async function detect() {
  // /v1/me exists only with multi-tenancy; without it the route is unknown.
  try {
    S.me = await api("GET", "/v1/me", undefined, { quiet401: true });
    S.tenancy = true;
    S.admin = !!S.me.admin;
    return true;
  } catch (e) {
    if (e.status === 401) return false;
    if (e.status !== 404) throw e;
  }
  S.me = null; S.tenancy = false;
  try {
    await api("GET", "/v1/agents", undefined, { quiet401: true });
    S.admin = true;
    return true;
  } catch (e) {
    if (e.status === 401) return false;
    throw e;
  }
}

async function boot() {
  let ok;
  try { ok = await detect(); } catch (e) {
    if (S.actAs) { // the tenant we acted as may be gone
      S.actAs = ""; store.del("agentd.tenant");
      return boot();
    }
    showLogin("Cannot reach the runtime: " + e.message);
    return;
  }
  if (!ok) {
    if (S.token) signOut("That token was not accepted.");
    else showLogin();
    return;
  }
  await renderChrome();
  route();
}

function showLogin(msg) {
  $("#nav").hidden = true;
  $("#who").hidden = true;
  const frag = $("#tpl-login").content.cloneNode(true);
  mount(frag);
  const err = $("#login-error");
  if (msg) { err.textContent = msg; err.hidden = false; }
  $("#login-form").addEventListener("submit", async (ev) => {
    ev.preventDefault();
    const btn = ev.submitter;
    if (btn) btn.disabled = true;
    await signIn($("#login-token").value.trim(), $("#login-remember").checked);
    if (btn) btn.disabled = false;
  });
  $("#login-token").focus();
}

async function renderChrome() {
  $("#nav").hidden = false;
  $("#who").hidden = false;
  for (const a of document.querySelectorAll("[data-tenant]")) a.hidden = !editable();
  const label = $("#who-label");
  if (S.tenancy) {
    const t = S.me.tenant;
    label.textContent = (t.name && t.name !== t.id ? t.name + " · " : "") + t.id + (t.plan ? " · " + t.plan : "");
  } else {
    label.textContent = "single-tenant";
  }
  $("#logout").hidden = !S.token;
  const sw = $("#tenant-switch");
  sw.hidden = true;
  if (S.tenancy && S.admin) {
    try {
      const { tenants } = await api("GET", "/v1/admin/tenants");
      sw.replaceChildren(
        h("option", { value: "" }, "system (config file)"),
        ...tenants.map((t) => h("option", { value: t.id, selected: t.id === S.actAs }, t.id + (t.name && t.name !== t.id ? " – " + t.name : ""))),
      );
      sw.hidden = false;
    } catch { /* not an admin token after all */ }
  }
}

// ---------- routing ----------

function route() {
  const parts = (location.hash.replace(/^#\/?/, "") || "agents").split("/").map(decodeURIComponent);
  const section = parts[0];
  for (const a of document.querySelectorAll("[data-nav]")) a.classList.toggle("active", a.dataset.nav === section);
  const run = {
    agents: () => {
      if (!parts[1]) return agentList();
      if (parts[1] === "new") return agentEditor(null);
      if (parts[2] === "edit") return agentEditor(parts[1]);
      return agentDetail(parts[1]);
    },
    providers: providersPage,
    connectors: () => (parts[1] ? connectorEditor(parts[1] === "new" ? null : parts[1]) : connectorsPage()),
    secrets: secretsPage,
  }[section] || agentList;
  Promise.resolve(run()).catch((e) => {
    if (e instanceof APIError && e.status === 401) return;
    mount(h("p", { class: "error" }, e.message));
  });
}

const go = (hash) => { if (location.hash === hash) route(); else location.hash = hash; };

// ---------- data ----------

async function loadConfig() {
  if (!editable()) return null;
  return api("GET", "/v1/config");
}

function providerChoices(cfg) {
  const own = Object.keys(cfg ? cfg.providers : {}).sort();
  const shared = (S.me && S.me.shared_providers || []).filter((p) => !own.includes(p));
  return { own, shared };
}

// ---------- agents: list ----------

async function agentList() {
  mount(h("p", { class: "muted" }, "Loading…"));
  const [runtime, cfg] = await Promise.all([api("GET", "/v1/agents"), loadConfig()]);
  const byName = new Map(runtime.agents.map((a) => [a.name, a]));
  const cards = [];
  const names = new Set(runtime.agents.map((a) => a.name));
  if (cfg) Object.keys(cfg.agents).forEach((n) => names.add(n));
  for (const name of [...names].sort()) {
    const rt = byName.get(name);
    const spec = cfg ? cfg.agents[name] : null;
    const triggers = rt ? rt.triggers || [] : (spec && spec.triggers || []).map((t) => t.type);
    cards.push(h("a", { class: "agent", href: "#/agents/" + enc(name) },
      h("div", { class: "name" }, name, !rt && h("span", { class: "badge", title: "expose: false – reachable only through its triggers" }, "hidden")),
      h("div", { class: "desc" }, (rt && rt.description) || (spec && spec.description) || ""),
      h("div", { class: "meta" }, ((rt && rt.provider) || (spec && spec.provider) || "default") + ((rt && rt.model) || (spec && spec.model) ? " · " + ((rt && rt.model) || spec.model) : "")),
      h("div", { class: "chips" },
        triggers.map((t) => h("span", { class: "chip trig" }, t)),
        rt && rt.tools.slice(0, 6).map((t) => h("span", { class: "chip" }, t)),
        rt && rt.tools.length > 6 && h("span", { class: "chip" }, "+" + (rt.tools.length - 6))),
    ));
  }
  const canEdit = editable();
  mount(
    h("div", { class: "head" },
      h("h1", {}, "Agents"),
      h("span", { class: "spacer" }),
      canEdit && h("a", { class: "button primary", href: "#/agents/new" }, "+ New agent")),
    usageStrip(),
    !canEdit && readOnlyBanner(),
    cards.length
      ? h("div", { class: "grid" }, cards)
      : h("div", { class: "empty" },
        h("p", {}, "No agents yet."),
        canEdit && h("a", { class: "button primary", href: "#/agents/new" }, "Create your first agent")),
  );
}

function readOnlyBanner() {
  if (!S.tenancy) return h("div", { class: "banner" }, "Single-tenant mode: agents come from the config file. You can chat with them here; to add agents, edit the config file or enable tenancy.");
  if (S.admin) return h("div", { class: "banner" }, "These are the system agents from the config file (read-only). Pick a tenant at the top right to manage its agents.");
  return null;
}

function usageStrip() {
  if (!S.me || !S.me.usage) return null;
  const u = S.me.usage, l = S.me.limits || {};
  const tokens = (u.input_tokens || 0) + (u.output_tokens || 0);
  const stat = (k, v, of) => h("div", { class: "stat" },
    h("div", { class: "k" }, k),
    h("div", { class: "v" }, fmt(v), of > 0 && h("span", { class: "muted" }, " / " + fmt(of))),
    of > 0 && h("div", { class: "bar" }, barFill(v / of)));
  const f = S.me.files;
  return h("div", { class: "stats" },
    stat("Runs this month", u.runs),
    stat("Tokens this month", tokens, l.monthly_tokens),
    l.max_agents > 0 && stat("Agent limit", l.max_agents),
    f && stat("Stored files", f.files, f.max_files));
}

function barFill(ratio) {
  const i = h("i");
  i.style.width = Math.min(100, Math.max(0, ratio * 100)).toFixed(1) + "%";
  return i;
}

// ---------- agents: detail + chat ----------

async function agentDetail(name) {
  mount(h("p", { class: "muted" }, "Loading…"));
  const cfg = await loadConfig();
  const spec = cfg ? cfg.agents[name] : null;
  let rt = null;
  try { rt = await api("GET", "/v1/agents/" + enc(name)); } catch (e) { if (e.status !== 404) throw e; }
  if (!rt && !spec) { mount(backLink(), h("p", {}, "Agent ", h("code", {}, name), " not found.")); return; }

  const facts = h("dl", { class: "facts" });
  const fact = (k, v) => { if (v != null && v !== "") facts.append(h("dt", {}, k), h("dd", {}, v)); };
  fact("Description", (rt && rt.description) || (spec && spec.description));
  fact("Provider", (rt && rt.provider) || (spec && spec.provider));
  fact("Model", (rt && rt.model) || (spec && spec.model) || "provider default");
  if (rt) fact("Tools", rt.tools.length ? h("div", { class: "chips" }, rt.tools.map((t) => h("span", { class: "chip" }, t))) : "none");
  if (spec && spec.system) fact("Instructions", h("pre", {}, spec.system));
  const trig = (spec && spec.triggers) || [];
  if (trig.length) fact("Triggers", h("div", {}, trig.map((t) => h("div", { class: "meta" }, describeTrigger(name, t)))));
  if (rt) fact("API", h("code", {}, "POST /v1/agents/" + name + "/run"));

  const del = async () => {
    if (!confirm("Delete agent \"" + name + "\"? Its triggers stop immediately.")) return;
    try { await api("DELETE", "/v1/config/agents/" + enc(name)); toast("Agent deleted"); go("#/agents"); } catch (e) { toast(e.message, true); }
  };

  mount(
    backLink(),
    h("div", { class: "head" },
      h("h1", {}, name),
      !rt && h("span", { class: "badge" }, "hidden"),
      h("span", { class: "spacer" }),
      spec && h("a", { class: "button", href: "#/agents/" + enc(name) + "/edit" }, "Edit"),
      spec && h("button", { class: "danger", type: "button", onclick: del }, "Delete")),
    h("div", { class: "split" },
      h("div", { class: "card" }, facts),
      rt ? chatPanel(name) : h("div", { class: "card muted" }, "This agent is not exposed (expose: false). It only runs through its triggers.")),
  );
}

function describeTrigger(agent, t) {
  switch (t.type) {
    case "cron": return "cron " + (t.schedule || "") + (t.timezone ? " (" + t.timezone + ")" : "");
    case "interval": return "every " + (t.every || "?");
    case "imap": return "inbox of " + (t.connector || "?") + (t.every ? ", every " + t.every : "");
    case "webhook": {
      const base = S.tenancy && S.me.tenant.id !== "system" ? "/v1/tenants/" + S.me.tenant.id + "/hooks/" : "/v1/hooks/";
      return "webhook POST " + location.origin + base + agent + (t.name ? "/" + t.name : "");
    }
    default: return t.type;
  }
}

const backLink = () => h("a", { class: "crumb", href: "#/agents" }, "← Agents");

function chatPanel(name) {
  const log = h("div", { class: "log" });
  const input = h("textarea", { placeholder: "Message " + name + "…", title: "Enter sends, Shift+Enter adds a new line", rows: 1 });
  const send = h("button", { class: "primary", type: "submit" }, "Send");
  const sessionLabel = h("span", { class: "muted" });
  const setSession = (id) => {
    if (id) S.sessions[name] = id; else delete S.sessions[name];
    sessionLabel.textContent = id ? "session " + id.slice(0, 12) + "…" : "new conversation";
  };
  setSession(S.sessions[name]);
  const reset = async () => {
    const id = S.sessions[name];
    setSession(null);
    log.replaceChildren();
    if (id) api("DELETE", "/v1/agents/" + enc(name) + "/sessions/" + enc(id)).catch(() => {});
    input.focus();
  };
  const scroll = () => { log.scrollTop = log.scrollHeight; };

  let busy = false;
  const submit = async (ev) => {
    ev && ev.preventDefault();
    const text = input.value.trim();
    if (!text || busy) return;
    busy = true; send.disabled = true;
    input.value = "";
    log.append(h("div", { class: "msg user" }, text));
    const pending = h("div", { class: "msg bot typing" });
    log.append(pending); scroll();
    const tools = new Map();
    try {
      await streamRun(name, { input: text, session_id: S.sessions[name] || "", stream: true }, (ev, data) => {
        if (ev === "tool_call") {
          const args = data.args ? JSON.stringify(data.args) : "";
          const d = h("details", {}, h("summary", {}, "⚙ " + data.tool), h("pre", {}, args));
          const box = h("div", { class: "tool" }, d);
          tools.set(data.call_id || data.tool, { box, d });
          log.insertBefore(box, pending);
        } else if (ev === "tool_result") {
          const t = tools.get(data.call_id || data.tool);
          if (t) {
            t.d.append(h("pre", {}, "→ " + (data.text || "")));
            if (data.is_error) t.box.classList.add("bad");
          }
        } else if (ev === "result") {
          pending.classList.remove("typing");
          pending.replaceChildren(document.createTextNode(data.output || "(no answer)"),
            h("div", { class: "foot" }, data.steps + " step" + (data.steps === 1 ? "" : "s") + " · " + data.tool_calls + " tool call" + (data.tool_calls === 1 ? "" : "s") + " · " + ((data.duration_ms || 0) / 1000).toFixed(1) + " s"));
          setSession(data.session_id);
        } else if (ev === "error") {
          throw new Error(data.error || "run failed");
        }
        scroll();
      });
      if (pending.classList.contains("typing")) throw new Error("the run ended without an answer");
    } catch (e) {
      pending.className = "msg err";
      pending.textContent = e.message;
    } finally {
      busy = false; send.disabled = false; scroll(); input.focus();
    }
  };
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) submit(e);
  });
  input.addEventListener("input", () => { input.style.height = "42px"; input.style.height = Math.min(160, input.scrollHeight) + "px"; });

  return h("div", { class: "card chat" },
    h("div", { class: "chat-head" }, h("strong", {}, "Chat"), sessionLabel, h("span", { class: "spacer" }),
      h("button", { class: "ghost small", type: "button", onclick: reset }, "New conversation")),
    log,
    h("form", { class: "compose", onsubmit: submit }, input, send));
}

// streamRun posts a run with stream=true and calls onEvent for every SSE
// event. EventSource cannot POST or send headers, so this reads the body.
async function streamRun(name, body, onEvent) {
  const res = await fetch("/v1/agents/" + enc(name) + "/run", {
    method: "POST",
    headers: headers({ "Content-Type": "application/json", Accept: "text/event-stream" }),
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    let msg = res.status + " " + res.statusText;
    try { const j = await res.json(); if (j.error && j.error.message) msg = j.error.message; } catch { /* ignore */ }
    if (res.status === 401) signOut("Your session is no longer valid. Please sign in again.");
    throw new Error(msg);
  }
  const reader = res.body.getReader();
  const dec = new TextDecoder();
  let buf = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (value) buf += dec.decode(value, { stream: true });
    let i;
    while ((i = buf.indexOf("\n\n")) >= 0) {
      const chunk = buf.slice(0, i);
      buf = buf.slice(i + 2);
      let ev = "message", data = "";
      for (const line of chunk.split("\n")) {
        if (line.startsWith("event: ")) ev = line.slice(7);
        else if (line.startsWith("data: ")) data += line.slice(6);
      }
      if (data) onEvent(ev, JSON.parse(data));
    }
    if (done) return;
  }
}

// ---------- agents: editor ----------

const TRIGGER_FIELDS = {
  cron: [["schedule", "Schedule", "0 8 * * 1-5", "minute hour day month weekday"], ["timezone", "Time zone", "Europe/Berlin"], ["prompt", "Prompt", "Scheduled run: …"]],
  interval: [["every", "Every", "15m", "a duration like 30m or 6h"], ["prompt", "Prompt", "Scheduled run: …"]],
  webhook: [["name", "Name", "github", "path segment of the URL (optional)"], ["secret", "Secret", "", "header X-Webhook-Secret, ?secret= or GitHub HMAC"], ["prompt", "Prompt", "Handle this event."]],
  imap: [["connector", "Email connector", "mail"], ["every", "Poll every", "5m"], ["allow_from", "Allowed senders", "*@example.com", "comma-separated globs; empty = anyone"]],
};
const TRIGGER_KEYS = new Set(Object.values(TRIGGER_FIELDS).flat().map((f) => f[0]).concat(["type"]));

async function agentEditor(name) {
  if (!editable()) { go("#/agents"); return; }
  mount(h("p", { class: "muted" }, "Loading…"));
  const cfg = await loadConfig();
  const isNew = name == null;
  if (!isNew && !cfg.agents[name]) { mount(backLink(), h("p", {}, "Agent not found.")); return; }
  let spec = isNew ? { tools: ["system.*"] } : structuredClone(cfg.agents[name]);
  const { own, shared } = providerChoices(cfg);
  const connectors = Object.entries(cfg.connectors).sort(([a], [b]) => a.localeCompare(b));
  const limits = (S.me && S.me.limits) || {};

  // ----- form controls -----
  const f = {
    name: h("input", { required: true, pattern: NAME_PATTERN, placeholder: "support", value: isNew ? "" : name, disabled: !isNew }),
    description: h("input", { placeholder: "Answers customer questions" }),
    provider: h("select", {},
      own.length + shared.length === 0 && h("option", { value: "" }, "— no provider —"),
      own.map((p) => h("option", { value: p }, p)),
      shared.map((p) => h("option", { value: p }, p + " (shared)"))),
    model: h("input", { placeholder: "provider default" }),
    system: h("textarea", { rows: 7, placeholder: "You are the support assistant of …" }),
    maxSteps: h("input", { type: "number", min: 1, max: limits.max_steps > 0 ? limits.max_steps : null, placeholder: limits.max_steps > 0 ? "max " + limits.max_steps : "12" }),
    temperature: h("input", { type: "number", min: 0, max: 2, step: 0.1, placeholder: "provider default" }),
    expose: h("input", { type: "checkbox" }),
    extraTools: h("input", { placeholder: "e.g. crm.get_*, files.read" }),
  };
  const toolBoxes = [
    ["system.*", "Clock (system.now)"],
    ["files.*", "Files: read and write"],
    ["files.list,files.read", "Files: read only"],
    ...connectors.map(([n, c]) => [n + ".*", n + " (" + (c.type || "connector") + ")"]),
  ].map(([pattern, label]) => ({ pattern, box: h("input", { type: "checkbox", value: pattern }), label }));
  const modelHint = h("span", { class: "hint" });
  const syncModel = () => {
    const isShared = shared.includes(f.provider.value);
    f.model.disabled = isShared;
    if (isShared) f.model.value = "";
    modelHint.textContent = isShared ? "Shared providers always use the operator's model." : "";
  };
  f.provider.addEventListener("change", syncModel);
  const triggersBox = h("div");

  function triggerRow(t) {
    const row = h("div", { class: "trigger" });
    const type = h("select", {}, Object.keys(TRIGGER_FIELDS).map((k) => h("option", { value: k, selected: k === t.type }, k)));
    const fields = h("div", { class: "form-grid" });
    row._get = () => {
      const out = {};
      for (const [k, v] of Object.entries(t)) if (!TRIGGER_KEYS.has(k)) out[k] = v; // keep fields the form doesn't know
      out.type = type.value;
      for (const inp of fields.querySelectorAll("[data-k]")) {
        const v = inp.type === "checkbox" ? inp.checked : inp.value.trim();
        if (v === "" || v === false) continue;
        out[inp.dataset.k] = inp.dataset.k === "allow_from" ? v.split(",").map((s) => s.trim()).filter(Boolean) : v;
      }
      if (type.value === "webhook" && t.async != null) out.async = t.async;
      return out;
    };
    const draw = () => {
      fields.replaceChildren(...TRIGGER_FIELDS[type.value].map(([k, label, ph, hint]) => {
        let inp;
        if (k === "connector") {
          const mail = connectors.filter(([, c]) => c.type === "email" || c.type === "imap");
          inp = h("select", { "data-k": k }, mail.length ? mail.map(([n]) => h("option", { value: n, selected: n === t[k] }, n)) : h("option", { value: "" }, "— add an email connector first —"));
        } else {
          const v = Array.isArray(t[k]) ? t[k].join(", ") : t[k] || "";
          inp = h("input", { "data-k": k, placeholder: ph, value: v, type: k === "secret" ? "password" : "text", autocomplete: "off" });
        }
        return h("label", { class: k === "prompt" ? "full" : null }, label, inp, hint && h("span", { class: "hint" }, hint));
      }));
    };
    type.addEventListener("change", () => { t = row._get(); draw(); });
    draw();
    row.replaceChildren(h("label", {}, "Type", type), fields, h("button", { class: "ghost small danger", type: "button", onclick: () => row.remove() }, "Remove"));
    return row;
  }

  function fillForm(sp) {
    f.description.value = sp.description || "";
    const prov = sp.provider || (own.length + shared.length === 1 ? (own[0] || shared[0]) : "");
    if (prov && ![...f.provider.options].some((o) => o.value === prov)) f.provider.append(h("option", { value: prov }, prov + " (missing)"));
    f.provider.value = prov || (own[0] || shared[0] || "");
    f.model.value = sp.model || "";
    f.system.value = sp.system || "";
    f.maxSteps.value = sp.max_steps || "";
    f.temperature.value = sp.temperature ?? "";
    f.expose.checked = sp.expose !== false;
    let tools = [...(sp.tools || [])];
    for (const tb of toolBoxes) {
      const parts = tb.pattern.split(",");
      tb.box.checked = parts.every((p) => tools.includes(p));
      if (tb.box.checked) tools = tools.filter((t) => !parts.includes(t));
    }
    // "files.*" and "files (read only)" exclude each other.
    if (toolBoxes[1].box.checked) toolBoxes[2].box.checked = false;
    f.extraTools.value = tools.join(", ");
    triggersBox.replaceChildren(...(sp.triggers || []).map((t) => triggerRow({ ...t })));
    syncModel();
  }

  function readForm() {
    const sp = structuredClone(spec);
    const set = (k, v) => { if (v === "" || v == null || (Array.isArray(v) && !v.length)) delete sp[k]; else sp[k] = v; };
    set("description", f.description.value.trim());
    set("provider", f.provider.value);
    set("model", f.model.disabled ? "" : f.model.value.trim());
    set("system", f.system.value.trim());
    set("max_steps", f.maxSteps.value ? Number(f.maxSteps.value) : null);
    set("temperature", f.temperature.value !== "" ? Number(f.temperature.value) : null);
    if (f.expose.checked) delete sp.expose; else sp.expose = false;
    const tools = [];
    for (const tb of toolBoxes) if (tb.box.checked) tools.push(...tb.pattern.split(","));
    for (const t of f.extraTools.value.split(",")) if (t.trim() && !tools.includes(t.trim())) tools.push(t.trim());
    set("tools", [...new Set(tools)]);
    set("triggers", [...triggersBox.children].map((r) => r._get()));
    return sp;
  }

  // ----- JSON mode -----
  const json = h("textarea", { class: "code", spellcheck: "false" });
  const formPane = h("div");
  const jsonPane = h("div", { hidden: true }, json, h("span", { class: "hint" }, "The agent definition as JSON, the same schema as an entry under agents: in the config file."));
  const tabForm = h("button", { type: "button", class: "on" }, "Form");
  const tabJSON = h("button", { type: "button" }, "JSON");
  const err = h("p", { class: "error", hidden: true });
  const showErr = (m) => { err.textContent = m; err.hidden = !m; };
  let mode = "form";
  const switchTo = (m) => {
    if (m === mode) return;
    if (m === "json") { spec = readForm(); json.value = JSON.stringify(spec, null, 2); }
    else {
      try { spec = JSON.parse(json.value); } catch (e) { showErr("Invalid JSON: " + e.message); return; }
      fillForm(spec);
    }
    showErr("");
    mode = m;
    formPane.hidden = m !== "form"; jsonPane.hidden = m !== "json";
    tabForm.classList.toggle("on", m === "form"); tabJSON.classList.toggle("on", m === "json");
  };
  tabForm.addEventListener("click", () => switchTo("form"));
  tabJSON.addEventListener("click", () => switchTo("json"));

  toolBoxes[1].box.addEventListener("change", () => { if (toolBoxes[1].box.checked) toolBoxes[2].box.checked = false; });
  toolBoxes[2].box.addEventListener("change", () => { if (toolBoxes[2].box.checked) toolBoxes[1].box.checked = false; });

  formPane.append(
    h("div", { class: "form-grid" },
      h("label", {}, "Name", f.name, h("span", { class: "hint" }, "letters, digits, - and _; used in URLs and as the model name")),
      h("label", {}, "Description", f.description, h("span", { class: "hint" }, "shown to MCP clients that call this agent")),
      h("label", {}, "Provider", f.provider, own.length + shared.length === 0 && h("span", { class: "hint" }, h("a", { href: "#/providers" }, "Add a provider"), " first.")),
      h("label", {}, "Model", f.model, modelHint),
      h("label", { class: "full" }, "Instructions (system prompt)", f.system)),
    h("div", { class: "section" },
      h("h3", {}, "Tools"),
      h("div", { class: "tools" }, toolBoxes.map((tb) => h("label", { class: "check" }, tb.box, tb.label))),
      connectors.length === 0 && h("p", { class: "hint" }, "Connect MCP servers, HTTP APIs or email under ", h("a", { href: "#/connectors" }, "Connectors"), " to give agents more tools."),
      h("label", {}, "More tool patterns", f.extraTools, h("span", { class: "hint" }, "comma-separated globs over <connector>.<tool>"))),
    h("div", { class: "section" },
      h("h3", {}, "Triggers"),
      h("p", { class: "hint" }, "Besides the API, MCP and chat, an agent can run on a schedule, on webhooks or for incoming email."),
      triggersBox,
      h("button", { class: "small", type: "button", onclick: () => triggersBox.append(triggerRow({ type: "cron", timezone: Intl.DateTimeFormat().resolvedOptions().timeZone })) }, "+ Add trigger")),
    h("div", { class: "section" },
      h("h3", {}, "Limits"),
      h("div", { class: "form-grid" },
        h("label", {}, "Max steps per run", f.maxSteps),
        h("label", {}, "Temperature", f.temperature)),
      h("label", { class: "check" }, f.expose, "Reachable through the API, MCP and chat (uncheck for trigger-only agents)")),
  );
  fillForm(spec);

  const save = h("button", { class: "primary", type: "submit" }, isNew ? "Create agent" : "Save");
  const onSave = async (ev) => {
    ev.preventDefault();
    showErr("");
    const n = isNew ? f.name.value.trim() : name;
    if (!nameRe.test(n)) { showErr("The name must start with a letter or digit and contain only letters, digits, - and _ (max 48)."); return; }
    let body;
    if (mode === "json") {
      try { body = JSON.parse(json.value); } catch (e) { showErr("Invalid JSON: " + e.message); return; }
    } else body = readForm();
    if (isNew && cfg.agents[n] && !confirm("An agent named \"" + n + "\" exists. Replace it?")) return;
    save.disabled = true;
    try {
      await api("PUT", "/v1/config/agents/" + enc(n), body);
      toast(isNew ? "Agent created" : "Saved");
      go("#/agents/" + enc(n));
    } catch (e) { showErr(e.message); } finally { save.disabled = false; }
  };

  mount(
    h("a", { class: "crumb", href: isNew ? "#/agents" : "#/agents/" + enc(name) }, "← " + (isNew ? "Agents" : name)),
    h("div", { class: "head" }, h("h1", {}, isNew ? "New agent" : "Edit " + name), h("span", { class: "spacer" }), h("div", { class: "tabs" }, tabForm, tabJSON)),
    h("form", { class: "card", onsubmit: onSave, novalidate: true },
      formPane, jsonPane,
      h("div", { class: "bar-actions" }, err,
        h("a", { class: "button ghost", href: isNew ? "#/agents" : "#/agents/" + enc(name) }, "Cancel"), save)),
  );
  (isNew ? f.name : f.description).focus();
}

// ---------- providers ----------

const PROVIDER_TYPES = ["anthropic", "mistral", "openai", "ollama", "openai-compatible", "gemini", "groq", "openrouter", "deepseek", "together", "scaleway", "ionos", "xai", "azure"];
const NEEDS_URL = new Set(["ollama", "openai-compatible", "azure"]);

async function providersPage() {
  if (!editable()) { go("#/agents"); return; }
  const cfg = await loadConfig();
  const { shared } = providerChoices(cfg);
  const rows = Object.entries(cfg.providers).sort(([a], [b]) => a.localeCompare(b)).map(([n, p]) =>
    h("tr", {},
      h("td", {}, h("strong", {}, n)),
      h("td", {}, h("code", {}, p.type || "")),
      h("td", {}, p.model || h("span", { class: "muted" }, "default")),
      h("td", { class: "muted" }, p.base_url || ""),
      h("td", { class: "actions" },
        h("button", { class: "small", type: "button", onclick: () => rawEditor("providers", n, p) }, "Edit"),
        " ",
        h("button", { class: "small danger", type: "button", onclick: () => removeResource("providers", n, providersPage) }, "Delete"))));
  for (const n of shared) rows.push(h("tr", {}, h("td", {}, h("strong", {}, n)), h("td", { colspan: 3, class: "muted" }, "shared by the operator; no key needed"), h("td")));

  const f = {
    name: h("input", { placeholder: "mistral", pattern: NAME_PATTERN }),
    type: h("select", {}, PROVIDER_TYPES.map((t) => h("option", { value: t }, t))),
    model: h("input", { placeholder: "optional, e.g. mistral-small-latest" }),
    baseURL: h("input", { placeholder: "https://…", type: "url" }),
    key: h("input", { type: "password", autocomplete: "off", placeholder: "stored encrypted as a secret" }),
  };
  const urlLabel = h("label", {}, "Base URL", f.baseURL, h("span", { class: "hint" }, "required for ollama, openai-compatible and azure"));
  const syncType = () => { urlLabel.hidden = !NEEDS_URL.has(f.type.value); };
  f.type.addEventListener("change", () => { syncType(); if (!f.name.value) f.name.placeholder = f.type.value; });
  syncType();
  const err = h("p", { class: "error", hidden: true });
  const add = async (ev) => {
    ev.preventDefault();
    err.hidden = true;
    const name = f.name.value.trim() || f.name.placeholder;
    if (!nameRe.test(name)) { err.textContent = "Invalid name."; err.hidden = false; return; }
    const spec = { type: f.type.value };
    if (f.model.value.trim()) spec.model = f.model.value.trim();
    if (NEEDS_URL.has(f.type.value) && f.baseURL.value.trim()) spec.base_url = f.baseURL.value.trim();
    const doc = { providers: { [name]: spec } };
    if (f.key.value) {
      const secret = (name.toUpperCase().replace(/[^A-Z0-9_]/g, "_") + "_API_KEY").replace(/^([0-9])/, "_$1");
      spec.api_key = "${secret:" + secret + "}";
      doc.secrets = { [secret]: f.key.value };
    }
    try { await api("PUT", "/v1/config", doc); toast("Provider saved"); providersPage(); } catch (e) { err.textContent = e.message; err.hidden = false; }
  };

  mount(h("div", { class: "stack" },
    h("div", { class: "head" }, h("h1", {}, "Providers")),
    h("p", { class: "muted" }, "Language models your agents can use. API keys are stored encrypted as secrets and never shown again."),
    rows.length ? h("table", {}, h("thead", {}, h("tr", {}, h("th", {}, "Name"), h("th", {}, "Type"), h("th", {}, "Model"), h("th", {}, "Endpoint"), h("th"))), h("tbody", {}, rows))
      : h("div", { class: "empty" }, h("p", {}, "No providers yet. Add one below.")),
    h("form", { class: "card", onsubmit: add },
      h("h2", {}, "Add provider"),
      h("div", { class: "form-grid" },
        h("label", {}, "Type", f.type), h("label", {}, "Name", f.name),
        h("label", {}, "Model", f.model), h("label", {}, "API key", f.key),
        urlLabel),
      h("div", { class: "bar-actions" }, err, h("button", { class: "primary", type: "submit" }, "Add provider")))),
  );
}

async function removeResource(kind, name, after) {
  if (!confirm("Delete " + kind.slice(0, -1) + " \"" + name + "\"?")) return;
  try { await api("DELETE", "/v1/config/" + kind + "/" + enc(name)); toast("Deleted"); after(); } catch (e) { toast(e.message, true); }
}

// rawEditor edits any definition as YAML or JSON (the server accepts both).
function rawEditor(kind, name, spec, isNew) {
  const nameInput = h("input", { required: true, pattern: NAME_PATTERN, value: name || "", disabled: !isNew, placeholder: "name" });
  const text = h("textarea", { class: "code", spellcheck: "false", value: typeof spec === "string" ? spec : JSON.stringify(spec, null, 2) });
  const err = h("p", { class: "error", hidden: true });
  const back = kind === "providers" ? "#/providers" : "#/connectors";
  const secretList = h("span", { class: "hint" });
  api("GET", "/v1/secrets").then(({ secrets }) => {
    secretList.textContent = secrets.length ? "Your secrets: " + secrets.join(", ") : "You have no secrets yet.";
  }).catch(() => {});
  const save = async (ev) => {
    ev.preventDefault();
    const n = nameInput.value.trim();
    if (!nameRe.test(n)) { err.textContent = "Invalid name."; err.hidden = false; return; }
    try {
      await api("PUT", "/v1/config/" + kind + "/" + enc(n), text.value, { headers: { "Content-Type": "application/yaml" } });
      toast("Saved");
      if (location.hash === back) route(); else location.hash = back;
    } catch (e) { err.textContent = e.message; err.hidden = false; }
  };
  mount(
    h("a", { class: "crumb", href: back }, "← " + kind[0].toUpperCase() + kind.slice(1)),
    h("div", { class: "head" }, h("h1", {}, (isNew ? "New " : "Edit ") + kind.slice(0, -1) + (name && !isNew ? " " + name : ""))),
    h("form", { class: "card", onsubmit: save },
      h("label", {}, "Name", nameInput),
      h("label", {}, "Definition (YAML or JSON)", text,
        h("span", { class: "hint" }, "Reference secrets as ${secret:NAME} (", h("a", { href: "#/secrets" }, "manage secrets"), "). Server environment variables are not available here."),
        secretList),
      h("div", { class: "bar-actions" }, err, h("a", { class: "button ghost", href: back }, "Cancel"), h("button", { class: "primary", type: "submit" }, "Save"))),
  );
  (isNew ? nameInput : text).focus();
}

// ---------- connectors ----------

const CONNECTOR_TEMPLATES = {
  "MCP server": `type: mcp
url: https://mcp.example.com/mcp
headers:
  Authorization: Bearer \${secret:MCP_TOKEN}
# include: ["list_*", "get_*"]   # optional tool filter
`,
  "HTTP API": `type: http
base_url: https://api.example.com
headers:
  Authorization: Bearer \${secret:API_TOKEN}
endpoints:
  - name: get_item
    description: Get an item by id.
    method: GET
    path: /items/{id}
    params:
      id: {description: Item id}
`,
  "Web requests (allowlist)": `type: http
allow_hosts: ["*.wikipedia.org", "api.open-meteo.com"]
allow_methods: [GET]
`,
  "Email (SMTP + IMAP)": `type: email
smtp:
  host: smtp.example.com
  port: 587
  tls: starttls
  username: bot@example.com
  password: \${secret:MAIL_PASSWORD}
  from: "Support Bot <bot@example.com>"
imap:
  host: imap.example.com
  port: 993
  tls: implicit
  username: bot@example.com
  password: \${secret:MAIL_PASSWORD}
allow_recipients: ["*@example.com"]
`,
};

async function connectorsPage() {
  if (!editable()) { go("#/agents"); return; }
  const cfg = await loadConfig();
  const rows = Object.entries(cfg.connectors).sort(([a], [b]) => a.localeCompare(b)).map(([n, c]) =>
    h("tr", {},
      h("td", {}, h("strong", {}, n)),
      h("td", {}, h("code", {}, c.type || "")),
      h("td", { class: "muted" }, c.url || c.base_url || (c.smtp && c.smtp.host) || (c.imap && c.imap.host) || (c.allow_hosts || []).join(", ")),
      h("td", {}, h("code", {}, n + ".*")),
      h("td", { class: "actions" },
        h("button", { class: "small", type: "button", onclick: () => rawEditor("connectors", n, c) }, "Edit"),
        " ",
        h("button", { class: "small danger", type: "button", onclick: () => removeResource("connectors", n, connectorsPage) }, "Delete"))));
  mount(
    h("div", { class: "head" }, h("h1", {}, "Connectors"), h("span", { class: "spacer" }),
      h("select", { onchange: (e) => { if (e.target.value) rawEditor("connectors", "", CONNECTOR_TEMPLATES[e.target.value], true); } },
        h("option", { value: "" }, "+ New connector…"),
        Object.keys(CONNECTOR_TEMPLATES).map((k) => h("option", { value: k }, k)))),
    h("p", { class: "muted" }, "Connectors give agents tools: MCP servers, HTTP APIs and email. Agents pick them with patterns like ", h("code", {}, "crm.*"), "."),
    rows.length ? h("table", {}, h("thead", {}, h("tr", {}, h("th", {}, "Name"), h("th", {}, "Type"), h("th", {}, "Endpoint"), h("th", {}, "Tools"), h("th"))), h("tbody", {}, rows))
      : h("div", { class: "empty" }, h("p", {}, "No connectors yet. Choose a template at the top right.")),
  );
}

function connectorEditor(name) { return name ? loadConfig().then((c) => rawEditor("connectors", name, c.connectors[name] || {})) : connectorsPage(); }

// ---------- secrets + keys ----------

async function secretsPage() {
  if (!editable()) { go("#/agents"); return; }
  const [{ secrets }, { keys }] = await Promise.all([api("GET", "/v1/secrets"), api("GET", "/v1/keys")]);
  const sName = h("input", { required: true, placeholder: "MISTRAL_KEY", pattern: "[A-Za-z_][A-Za-z0-9_]{0,63}" });
  const sValue = h("input", { required: true, type: "password", autocomplete: "off" });
  const sErr = h("p", { class: "error", hidden: true });
  const putSecret = async (ev) => {
    ev.preventDefault();
    try { await api("PUT", "/v1/secrets/" + enc(sName.value.trim()), { value: sValue.value }); toast("Secret saved"); secretsPage(); } catch (e) { sErr.textContent = e.message; sErr.hidden = false; }
  };
  const delSecret = async (n) => {
    if (!confirm("Delete secret " + n + "?")) return;
    try { await api("DELETE", "/v1/secrets/" + enc(n)); toast("Deleted"); secretsPage(); } catch (e) { toast(e.message, true); }
  };
  const kName = h("input", { placeholder: "ci, zapier, …" });
  const newKey = h("div");
  const createKey = async (ev) => {
    ev.preventDefault();
    try {
      const r = await api("POST", "/v1/keys", { name: kName.value.trim() });
      await secretsPage();
      $("#new-key").replaceChildren(h("div", { class: "banner" }, h("strong", {}, "New key – copy it now, it is not shown again:"), h("div", { class: "keybox" }, r.key)));
    } catch (e) { toast(e.message, true); }
  };
  newKey.id = "new-key";
  const delKey = async (k) => {
    if (!confirm("Revoke key " + (k.name || k.prefix) + "? Clients using it stop working.")) return;
    try { await api("DELETE", "/v1/keys/" + enc(k.id)); toast("Key revoked"); secretsPage(); } catch (e) { toast(e.message, true); }
  };
  mount(h("div", { class: "stack" },
    h("div", { class: "head" }, h("h1", {}, "Secrets & keys")),
    h("h2", {}, "Secrets"),
    h("p", { class: "muted" }, "Encrypted values your definitions reference as ", h("code", {}, "${secret:NAME}"), ". Values are write-only."),
    secrets.length ? h("table", {}, h("tbody", {}, secrets.map((n) => h("tr", {}, h("td", {}, h("code", {}, n)), h("td", { class: "muted" }, "${secret:" + n + "}"),
      h("td", { class: "actions" }, h("button", { class: "small danger", type: "button", onclick: () => delSecret(n) }, "Delete"))))))
      : h("div", { class: "empty" }, h("p", {}, "No secrets yet.")),
    h("form", { class: "card", onsubmit: putSecret },
      h("div", { class: "form-grid" }, h("label", {}, "Name", sName), h("label", {}, "Value", sValue)),
      h("div", { class: "bar-actions" }, sErr, h("button", { class: "primary", type: "submit" }, "Save secret"))),
    h("h2", {}, "API keys"),
    h("p", { class: "muted" }, "Keys (", h("code", {}, "ark_…"), ") let scripts, MCP clients and other apps call your agents."),
    newKey,
    keys.length ? h("table", {}, h("thead", {}, h("tr", {}, h("th", {}, "Name"), h("th", {}, "Prefix"), h("th", {}, "Created"), h("th"))),
      h("tbody", {}, keys.map((k) => h("tr", {}, h("td", {}, k.name || "–"), h("td", {}, h("code", {}, k.prefix + "…")), h("td", { class: "muted" }, new Date(k.created_at).toLocaleString()),
        h("td", { class: "actions" }, h("button", { class: "small danger", type: "button", onclick: () => delKey(k) }, "Revoke"))))))
      : h("div", { class: "empty" }, h("p", {}, "No API keys yet.")),
    h("form", { class: "card", onsubmit: createKey },
      h("div", { class: "form-grid" }, h("label", {}, "Key name", kName)),
      h("div", { class: "bar-actions" }, h("button", { class: "primary", type: "submit" }, "Create key")))),
  );
}

// ---------- start ----------

window.addEventListener("hashchange", () => { if (S.token || S.admin || S.me) route(); });
document.addEventListener("DOMContentLoaded", () => {
  $("#logout").addEventListener("click", () => signOut());
  $("#tenant-switch").addEventListener("change", async (e) => {
    S.actAs = e.target.value;
    if (S.actAs) store.set("agentd.tenant", S.actAs); else store.del("agentd.tenant");
    await boot();
    if (location.hash.startsWith("#/agents/")) go("#/agents");
  });
  boot();
});
