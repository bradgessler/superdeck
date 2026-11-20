"use strict";
const $ = (id) => document.getElementById(id);

let state = { accounts: [], projects: [], runs: [], root: "" };
let lastJSON = "";
let view = "";
let open = null; // { id, term, source }
const pick = load();

function load() {
  try { return Object.assign({ account: "", project: "" }, JSON.parse(localStorage.getItem("superdeck") || "{}")); }
  catch (_) { return { account: "", project: "" }; }
}
function save() { try { localStorage.setItem("superdeck", JSON.stringify(pick)); } catch (_) {} }

async function api(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: body ? { "Content-Type": "application/json" } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
  return res.json();
}

let errorTimer;
function fail(err) {
  const msg = String(err.message || err);
  $("error").textContent = msg.charAt(0).toUpperCase() + msg.slice(1);
  $("error").hidden = false;
  clearTimeout(errorTimer);
  errorTimer = setTimeout(() => { $("error").hidden = true; }, 6000);
}

function el(tag, props, ...kids) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (k === "class") n.className = v;
    else if (k.startsWith("aria-") || k === "type" || k === "href") n.setAttribute(k, v);
    else n[k] = v;
  }
  n.append(...kids.filter((k) => k !== null && k !== undefined));
  return n;
}

function ago(iso) {
  const s = Math.max(0, (Date.now() - new Date(iso)) / 1000);
  if (s < 60) return "just now";
  if (s < 3600) return Math.floor(s / 60) + "m ago";
  if (s < 86400) return Math.floor(s / 3600) + "h ago";
  return Math.floor(s / 86400) + "d ago";
}

const account = (name) => state.accounts.find((a) => a.name === name);
const signedIn = (a) => !!(a && a.status && a.status.loggedIn);
const baseName = (p) => p.split("/").pop();
const go = (hash) => { location.hash = hash; };

const hintText = {
  "needs-trust": "Claude is asking whether to trust this folder. Answer below.",
  "needs-consent": "Claude is asking to turn on Remote Control for this account. Answer below.",
  "needs-login": "This account is signed out. Sign it in under Accounts, then launch again.",
};

function runState(r) {
  if (r.hint) return { cls: "wait", text: r.hint.replace("needs-", "needs ") };
  if (!r.done) return { cls: "on", text: r.kind === "login" ? "waiting for you" : "running" };
  return r.stopped || r.exitCode === 0 ? { cls: "", text: r.kind === "server" ? "stopped" : "finished" } : { cls: "bad", text: "failed" };
}

// ---- Instances (home) ----

const outdated = (r) => r.kind === "server" && !r.done && r.version && state.claude && r.version !== state.claude;

function instanceRow(r) {
  const s = runState(r);
  const sub = [r.account, `started ${ago(r.started)}`];
  if (r.restarts) sub.push(`restarted ${r.restarts}×`);
  if (outdated(r)) sub.push("update ready");
  return el("a", { href: `#/i/${r.id}`, class: r.done ? "dim" : "" },
    el("span", { class: `led ${s.cls} ${!r.done && s.cls === "on" ? "live" : ""}` }),
    el("span", { class: "row-body" },
      el("span", { class: "row-title" }, baseName(r.project || r.name)),
      el("span", { class: "row-sub" }, sub.join(" · "))),
    el("span", { class: "row-state " + s.cls }, s.text),
    el("span", { class: "chev", "aria-hidden": "true" }, "›"));
}

function renderHome() {
  const servers = state.runs.filter((r) => r.kind === "server");
  const live = servers.filter((r) => !r.done);
  const past = servers.filter((r) => r.done).slice(0, 5);
  const rows = live.map(instanceRow);
  if (!live.length) {
    rows.push(el("div", { class: "empty" }, el("b", {}, "Nothing is running."),
      "Launch an instance and it will show up here and in the Claude app."));
  }
  if (past.length) rows.push(el("div", { class: "divider" }, "Recently stopped"), ...past.map(instanceRow));
  $("instances").replaceChildren(...rows);

  // One quiet line about the Claude CLI. Updating is always your call.
  const updating = state.runs.find((r) => r.kind === "update" && !r.done);
  $("foot").replaceChildren(
    state.claude ? `Claude Code ${state.claude} · ` : "",
    updating ? el("a", { class: "link", href: `#/i/${updating.id}` }, "Update in progress")
      : el("button", { type: "button", onclick: () => api("POST", "/api/update").then((r) => refresh(true).then(() => go(`#/i/${r.id}`)), fail) }, "Check for updates"));
}

// ---- New instance ----

function renderChips() {
  if (!state.accounts.length) {
    $("account-chips").replaceChildren(el("span", { class: "none" }, "No accounts yet. ", el("a", { href: "#/accounts" }, "Add one first.")));
    return;
  }
  if (!account(pick.account)) pick.account = (state.accounts.find(signedIn) || state.accounts[0]).name;
  $("account-chips").replaceChildren(...state.accounts.map((a) =>
    el("button", {
      type: "button", class: "chip", "aria-pressed": String(a.name === pick.account),
      onclick: () => { pick.account = a.name; save(); renderChips(); renderGo(); },
    }, el("span", { class: "led " + (signedIn(a) ? "on" : "") }), a.name)));
}

function renderProjects() {
  const q = $("filter").value.trim().toLowerCase();
  const all = state.projects;
  $("filter").placeholder = all.length ? `Filter ${all.length} folders` : "No folders yet";
  if (!all.includes(pick.project)) pick.project = "";
  const hits = all.filter((p) => p.toLowerCase().includes(q));
  if (pick.project && !q) { hits.splice(hits.indexOf(pick.project), 1); hits.unshift(pick.project); }
  const shown = hits.slice(0, q ? 40 : 8);
  const rows = shown.map((p) => {
    const parent = p.includes("/") ? p.slice(0, p.lastIndexOf("/")) : "";
    return el("button", {
      type: "button", "aria-pressed": String(p === pick.project),
      onclick: () => { pick.project = p; save(); renderProjects(); renderGo(); },
    }, el("span", { class: "row-body" }, el("span", { class: "row-title" }, baseName(p)), el("span", { class: "row-sub" }, parent)),
      el("span", { class: "tick" }, "✓"));
  });
  if (!rows.length) rows.push(el("div", { class: "empty" }, all.length ? "Nothing matches." : `No Git repositories found in ${state.root}.`));
  else if (hits.length > shown.length) rows.push(el("div", { class: "empty" }, `${hits.length - shown.length} more. Type to find one.`));
  $("project-list").replaceChildren(...rows);
}

function renderGo() {
  const b = $("go");
  const a = account(pick.account);
  b.disabled = false;
  if (!a) { b.textContent = "Add an account first"; b.disabled = true; }
  else if (!signedIn(a)) b.textContent = `Sign in to ${a.name} first`;
  else if (!pick.project) { b.textContent = "Pick a folder"; b.disabled = true; }
  else b.textContent = `Launch ${baseName(pick.project)} as ${a.name}`;
}

// ---- Accounts ----

function renderAccounts() {
  const rows = state.accounts.map((a) => {
    const s = a.status || {};
    const pending = state.runs.find((r) => r.kind === "login" && r.account === a.name && !r.done);
    const who = pending ? "Sign-in in progress"
      : s.loggedIn ? [s.email, s.orgName].filter(Boolean).join(" · ") || "Signed in" : "Signed out";
    return el("div", {},
      el("span", { class: "led " + (pending ? "wait" : s.loggedIn ? "on" : "") }),
      el("span", { class: "row-body" }, el("span", { class: "row-title" }, a.name), el("span", { class: "row-sub" }, who)),
      pending ? el("a", { class: "btn", href: `#/i/${pending.id}` }, "Continue")
        : el("button", { type: "button", class: "btn", onclick: () => signIn(a.name) }, s.loggedIn ? "Sign in again" : "Sign in"));
  });
  if (!rows.length) rows.push(el("div", { class: "empty" }, el("b", {}, "No accounts yet."), "Name one below, then sign it in."));
  $("accounts").replaceChildren(...rows);
}

// ---- Instance ----

function renderInstance() {
  if (!open) return;
  const r = state.runs.find((x) => x.id === open.id);
  if (!r) return;
  const s = runState(r);
  const login = r.kind === "login";
  const server = r.kind === "server";
  $("back").textContent = login ? "‹ Accounts" : "‹ Instances";
  $("back").setAttribute("href", login ? "#/accounts" : "#/");
  $("i-title").textContent = login ? `Sign in ${r.account}` : server ? baseName(r.project || r.name) : r.name;
  const meta = [s.text];
  if (server) meta.push(r.account);
  meta.push(`started ${ago(r.started)}`);
  if (server) meta.push(`Claude Code ${r.version || "?"}`);
  $("i-meta").textContent = meta.join(" · ");
  $("i-path").textContent = server ? `${state.root}/${r.project}` : "";
  $("i-update").hidden = !outdated(r);
  $("i-update-text").textContent = outdated(r) ? `Claude Code ${state.claude} is installed. This instance is still running ${r.version}.` : "";
  $("paste").hidden = !login;
  $("i-hint").hidden = !r.hint;
  $("i-hint").textContent = hintText[r.hint] || "";
  $("i-url").hidden = !r.url || r.done;
  if (r.url) $("i-url").href = r.url;
  $("stop").hidden = r.done;
  $("i-input").hidden = r.done;
}

function render() {
  if (view === "home") renderHome();
  if (view === "new") { renderChips(); renderProjects(); renderGo(); }
  if (view === "accounts") renderAccounts();
  if (view === "instance") renderInstance();
}

async function refresh(force) {
  try {
    const next = await api("GET", "/api/state");
    const json = JSON.stringify(next);
    state = next;
    // The folder picker is only redrawn when something changed, so it does
    // not jump while you are scrolling or typing.
    if (force || json !== lastJSON || view !== "new") render();
    lastJSON = json;
  } catch (e) { fail(e); }
}

// ---- actions ----

function signIn(name) {
  api("POST", `/api/accounts/${name}/login`).then((r) => refresh(true).then(() => go(`#/i/${r.id}`)), fail);
}

$("go").onclick = () => {
  const a = account(pick.account);
  if (!a) return;
  if (!signedIn(a)) return signIn(a.name);
  api("POST", "/api/servers", { account: a.name, project: pick.project })
    .then((r) => refresh(true).then(() => go(`#/i/${r.id}`)), fail);
};
$("filter").oninput = renderProjects;
$("add-account").onsubmit = (e) => {
  e.preventDefault();
  const name = $("account-name").value;
  api("POST", "/api/accounts", { name }).then(() => { $("account-name").value = ""; pick.account = name; save(); return refresh(true); }, fail);
};
$("add-project").onsubmit = (e) => {
  e.preventDefault();
  api("POST", "/api/projects", { name: $("project-name").value }).then((r) => {
    $("project-name").value = ""; $("filter").value = ""; pick.project = r.project; save();
    return refresh(true);
  }, fail);
};

// ---- console ----

// The page can only press a fixed set of keys, or submit a sign-in code.
function send(body) {
  if (open) api("POST", `/api/runs/${open.id}/input`, body).catch(fail);
}

function openConsole(id) {
  closeConsole();
  const fontSize = window.innerWidth >= 760 ? 13 : 12;
  const cols = Math.max(40, Math.min(140, Math.floor(($("term").clientWidth - 22) / (fontSize * 0.602))));
  const rows = window.innerWidth >= 760 ? 30 : 20;
  const term = new Terminal({
    disableStdin: true, cols, rows, fontSize, fontFamily: "ui-monospace, 'SF Mono', Menlo, monospace", cursorBlink: true, scrollback: 3000,
    theme: { background: "#0b0908", foreground: "#f4ede4", cursor: "#ffb224", selectionBackground: "#5a4a2a" },
  });
  term.open($("term"));
  const source = new EventSource(`/api/runs/${id}/stream`);
  source.onmessage = (e) => term.write(Uint8Array.from(atob(e.data), (c) => c.charCodeAt(0)));
  source.addEventListener("end", () => { source.close(); refresh(true); });
  open = { id, term, source };
  api("POST", `/api/runs/${id}/resize`, { cols, rows }).catch(() => {});
}

function closeConsole() {
  if (!open) return;
  open.source.close();
  open.term.dispose();
  open = null;
  $("term").replaceChildren();
}

$("stop").onclick = () => {
  if (open && confirm("Stop this instance? Any Claude sessions it is hosting will end.")) {
    api("POST", `/api/runs/${open.id}/stop`).then(() => refresh(true), fail);
  }
};
$("keys").onclick = (e) => { const b = e.target.closest("button"); if (b) send({ key: b.dataset.k }); };
$("relaunch").onclick = () => {
  if (open && confirm("Relaunch this instance? Any Claude sessions it is hosting will end.")) {
    api("POST", `/api/runs/${open.id}/relaunch`).then((r) => refresh(true).then(() => go(`#/i/${r.id}`)), fail);
  }
};
$("paste").onsubmit = (e) => {
  e.preventDefault();
  const v = $("paste-text").value;
  if (v) send({ code: v });
  $("paste-text").value = "";
};

// ---- routing ----

function route() {
  const path = location.hash.slice(1) || "/";
  const inst = path.match(/^\/i\/([0-9a-f]+)$/);
  view = inst ? "instance" : path === "/new" ? "new" : path === "/accounts" ? "accounts" : "home";
  document.body.dataset.view = view;
  for (const v of ["home", "new", "accounts", "instance"]) $("view-" + v).hidden = v !== view;
  $("nav-home").toggleAttribute("aria-current", view === "home" || view === "new" || (view === "instance"));
  $("nav-accounts").toggleAttribute("aria-current", view === "accounts");
  for (const a of [$("nav-home"), $("nav-accounts")]) if (a.hasAttribute("aria-current")) a.setAttribute("aria-current", "page");
  closeConsole();
  window.scrollTo(0, 0);
  if (inst) openConsole(inst[1]);
  refresh(true);
}

window.addEventListener("hashchange", route);
route();
setInterval(refresh, 4000);
