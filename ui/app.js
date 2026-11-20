"use strict";
const $ = (id) => document.getElementById(id);

let state = { accounts: [], projects: [], runs: [], root: "" };
let lastJSON = "";
let open = null; // { id, term, source }
const pick = load();

function load() {
  try { return Object.assign({ account: "", project: "", spawn: "worktree" }, JSON.parse(localStorage.getItem("superdeck") || "{}")); }
  catch (_) { return { account: "", project: "", spawn: "worktree" }; }
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
    else if (k.startsWith("aria-") || k === "type") n.setAttribute(k, v);
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

const hintText = {
  "needs-trust": "Claude is asking whether to trust this folder. Answer below.",
  "needs-consent": "Claude is asking to turn on Remote Control for this account. Answer below.",
  "needs-login": "This account is signed out. Go back and sign it in under Accounts.",
};

function runState(r) {
  if (r.hint) return { cls: "wait", text: r.hint.replace("needs-", "needs ") };
  if (!r.done) return { cls: "on", text: r.kind === "login" ? "waiting" : "running" };
  return r.exitCode === 0 ? { cls: "", text: "finished" } : { cls: "bad", text: "exited " + r.exitCode };
}

// ---- home ----

function renderChips() {
  if (!state.accounts.length) {
    $("account-chips").replaceChildren(el("span", { class: "empty-inline" }, "No accounts yet. Add one under Accounts."));
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
  $("filter").placeholder = all.length ? `Filter ${all.length} projects` : "No projects yet";
  if (!all.includes(pick.project)) pick.project = "";
  const hits = all.filter((p) => p.toLowerCase().includes(q));
  // Keep the chosen project visible at the top even when the list is long.
  if (pick.project && !q) { hits.splice(hits.indexOf(pick.project), 1); hits.unshift(pick.project); }
  const shown = hits.slice(0, 60);
  const rows = shown.map((p) => {
    const parent = p.includes("/") ? p.slice(0, p.lastIndexOf("/")) : "";
    return el("li", {}, el("button", {
      type: "button", "aria-pressed": String(p === pick.project),
      onclick: () => { pick.project = p; save(); renderProjects(); renderGo(); },
    }, el("span", { class: "name" }, baseName(p)), el("span", { class: "path" }, parent), el("span", { class: "tick" }, "✓")));
  });
  if (!rows.length) rows.push(el("li", { class: "none" }, all.length ? "Nothing matches that filter." : "No Git repositories found in the projects folder."));
  else if (hits.length > shown.length) rows.push(el("li", { class: "none" }, `${hits.length - shown.length} more. Type to narrow.`));
  $("project-list").replaceChildren(...rows);
}

function renderSpawn() {
  for (const b of $("spawn").children) b.setAttribute("aria-pressed", String(b.dataset.v === pick.spawn));
  $("spawn-help").textContent = pick.spawn === "worktree"
    ? "Each session you start from the Claude app gets its own copy of the code, so they can't overwrite each other."
    : "Every session edits the same files. Simple, but two sessions at once can collide.";
}

function renderGo() {
  const go = $("go");
  const a = account(pick.account);
  go.disabled = false;
  if (!a) { go.textContent = "Add an account first"; go.disabled = true; }
  else if (!signedIn(a)) go.textContent = `Sign in to ${a.name}`;
  else if (!pick.project) { go.textContent = "Pick a project"; go.disabled = true; }
  else go.textContent = `Launch ${baseName(pick.project)} as ${a.name}`;
}

function renderRuns() {
  const live = state.runs.filter((r) => !r.done);
  const past = state.runs.filter((r) => r.done).slice(0, 4);
  $("run-count").textContent = live.length ? String(live.length) : "";
  const cards = [...live, ...past].map((r) => {
    const s = runState(r);
    return el("li", {}, el("button", { type: "button", class: "card" + (r.done ? " dim" : ""), onclick: () => openConsole(r.id) },
      el("span", { class: `led ${s.cls} ${s.cls === "on" ? "live" : ""}` }),
      el("span", { class: "body" },
        el("span", { class: "title" }, r.kind === "login" ? `Signing in ${r.account}` : baseName(r.project || r.name)),
        el("span", { class: "sub" }, r.kind === "login" ? ago(r.started) : `${r.account} · ${ago(r.started)}`)),
      el("span", { class: "state " + s.cls }, s.text),
      el("span", { class: "go", "aria-hidden": "true" }, "›")));
  });
  $("runs").replaceChildren(...(cards.length ? cards : [el("li", { class: "blank" }, "Nothing running. Launch a project above and it will show up here and in the Claude app.")]));
}

function renderAccounts() {
  const rows = state.accounts.map((a) => {
    const s = a.status || {};
    const who = s.loggedIn ? [s.email, s.orgName].filter(Boolean).join(" · ") || "Signed in" : "Signed out";
    return el("li", { class: "card" },
      el("span", { class: "led " + (s.loggedIn ? "on" : "") }),
      el("span", { class: "body" }, el("span", { class: "title" }, a.name), el("span", { class: "sub" }, who)),
      el("span", { class: "actions" },
        el("button", { type: "button", class: "ghost", onclick: () => api("POST", `/api/accounts/${a.name}/refresh`).then(refresh, fail) }, "Check"),
        el("button", { type: "button", class: "ghost", onclick: () => signIn(a.name) }, s.loggedIn ? "Switch" : "Sign in")));
  });
  $("accounts").replaceChildren(...(rows.length ? rows : [el("li", { class: "blank" }, "Add a label for each Claude account you want to run, then sign each one in.")]));
}

function renderConsoleMeta() {
  if (!open) return;
  const r = state.runs.find((x) => x.id === open.id);
  if (!r) return;
  const s = runState(r);
  $("console-title").textContent = r.kind === "login" ? `Sign in ${r.account}` : baseName(r.project || r.name);
  $("console-state").textContent = r.kind === "login" ? s.text : `${r.account} · ${s.text}`;
  $("console-hint").hidden = !r.hint;
  $("console-hint").textContent = hintText[r.hint] || "";
  $("console-url").hidden = !r.url || r.done;
  if (r.url) $("console-url").href = r.url;
  $("stop").hidden = r.done;
}

function render() {
  const live = state.runs.filter((r) => !r.done && r.kind === "server").length;
  $("summary").textContent = live ? `${live} running` : "idle";
  $("root-help").textContent = state.root ? `Creates an empty Git repository in ${state.root}.` : "";
  renderChips(); renderProjects(); renderSpawn(); renderGo(); renderRuns(); renderAccounts(); renderConsoleMeta();
}

async function refresh() {
  try {
    const next = await api("GET", "/api/state");
    const json = JSON.stringify(next);
    state = next;
    if (json !== lastJSON) { lastJSON = json; render(); } else { renderRuns(); renderConsoleMeta(); }
  } catch (e) { fail(e); }
}

// ---- actions ----

function signIn(name) {
  api("POST", `/api/accounts/${name}/login`).then((r) => openConsole(r.id), fail);
}

$("go").onclick = () => {
  const a = account(pick.account);
  if (!a) return;
  if (!signedIn(a)) return signIn(a.name);
  api("POST", "/api/servers", { account: a.name, project: pick.project, spawn: pick.spawn }).then((r) => openConsole(r.id), fail);
};
$("filter").oninput = renderProjects;
$("spawn").onclick = (e) => { if (e.target.dataset.v) { pick.spawn = e.target.dataset.v; save(); renderSpawn(); } };
$("add-account").onsubmit = (e) => {
  e.preventDefault();
  const name = $("account-name").value;
  api("POST", "/api/accounts", { name }).then(() => { $("account-name").value = ""; pick.account = name; save(); return refresh(); }, fail);
};
$("add-project").onsubmit = (e) => {
  e.preventDefault();
  api("POST", "/api/projects", { name: $("project-name").value }).then((r) => {
    $("project-name").value = ""; $("filter").value = ""; pick.project = r.project; save();
    return refresh().then(() => $("launch").scrollIntoView({ behavior: "smooth" }));
  }, fail);
};

// ---- console ----

function send(data) {
  if (open) api("POST", `/api/runs/${open.id}/input`, { data }).catch(fail);
}

function openConsole(id) {
  closeConsole();
  $("home").hidden = true;
  $("console").hidden = false;
  window.scrollTo(0, 0);
  const fontSize = 12;
  const width = $("term").clientWidth - 18;
  const cols = Math.max(40, Math.min(120, Math.floor(width / (fontSize * 0.602))));
  const rows = 22;
  const term = new Terminal({
    cols, rows, fontSize, fontFamily: "ui-monospace, 'SF Mono', Menlo, monospace", cursorBlink: true, scrollback: 2000,
    theme: { background: "#0b0908", foreground: "#f4ede4", cursor: "#ffb224", selectionBackground: "#5a4a2a" },
  });
  term.open($("term"));
  term.onData(send);
  const source = new EventSource(`/api/runs/${id}/stream`);
  source.onmessage = (e) => term.write(Uint8Array.from(atob(e.data), (c) => c.charCodeAt(0)));
  source.addEventListener("end", () => { source.close(); refresh(); });
  open = { id, term, source };
  api("POST", `/api/runs/${id}/resize`, { cols, rows }).catch(() => {});
  renderConsoleMeta();
  refresh();
}

function closeConsole() {
  if (!open) return;
  open.source.close();
  open.term.dispose();
  open = null;
  $("term").replaceChildren();
  $("console").hidden = true;
  $("home").hidden = false;
}

$("back").onclick = () => { closeConsole(); refresh(); };
$("stop").onclick = () => {
  if (open && confirm("Stop this process? Sessions it is hosting will end.")) api("POST", `/api/runs/${open.id}/stop`).then(refresh, fail);
};
$("keys").onclick = (e) => { const b = e.target.closest("button"); if (b) send(b.dataset.k); };
$("paste").onsubmit = (e) => {
  e.preventDefault();
  const v = $("paste-text").value;
  if (v) send(v + "\r");
  $("paste-text").value = "";
};

refresh();
setInterval(refresh, 4000);
