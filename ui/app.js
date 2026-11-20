"use strict";
const $ = (id) => document.getElementById(id);
let state = { accounts: [], projects: [], runs: [] };
let open = null; // { id, term, source }

async function api(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: body ? { "Content-Type": "application/json" } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  if (res.status === 401) { location.href = "/login.html"; throw new Error("signed out"); }
  if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
  return res.json();
}

function fail(err) {
  $("error").textContent = err.message;
  $("error").hidden = false;
  setTimeout(() => { $("error").hidden = true; }, 6000);
}

function el(tag, props, ...kids) {
  const n = Object.assign(document.createElement(tag), props || {});
  n.append(...kids);
  return n;
}

function setOptions(select, values, label) {
  const keep = select.value;
  select.replaceChildren(...values.map((v) => el("option", { value: v }, label ? label(v) : v)));
  if (values.includes(keep)) select.value = keep;
}

const hints = {
  "needs-trust": "Claude is asking whether to trust this folder. Answer in the console.",
  "needs-consent": "Claude is asking to enable Remote Control for this account. Answer in the console.",
  "needs-login": "This account is signed out. Sign it in from the Accounts list.",
};

function runTag(r) {
  if (r.hint) return el("span", { className: "tag warn" }, r.hint.replace("needs-", "needs "));
  if (!r.done) return el("span", { className: "tag ok" }, "running");
  return el("span", { className: "tag " + (r.exitCode === 0 ? "" : "bad") }, "exited " + r.exitCode);
}

function render() {
  $("root").textContent = state.root || "";
  setOptions($("account"), state.accounts.map((a) => a.name));
  setOptions($("project"), state.projects);

  const runs = state.runs.map((r) => {
    const li = el("li", {},
      el("div", { className: "grow" }, r.name, el("small", {}, r.project || r.kind)),
      runTag(r),
      el("button", { className: "quiet", type: "button", onclick: () => openConsole(r.id) }, "Console"));
    return li;
  });
  $("runs").replaceChildren(...(runs.length ? runs : [el("li", { className: "empty" }, "Nothing running yet.")]));

  const accts = state.accounts.map((a) => {
    const s = a.status || {};
    const who = s.loggedIn ? (s.email || "signed in") + (s.orgName ? " · " + s.orgName : "") : "signed out";
    return el("li", {},
      el("div", { className: "grow" }, a.name, el("small", {}, who)),
      el("span", { className: "tag " + (s.loggedIn ? "ok" : "warn") }, s.loggedIn ? "signed in" : "signed out"),
      el("button", { className: "quiet", type: "button", onclick: () => api("POST", `/api/accounts/${a.name}/refresh`).then(refresh, fail) }, "Check"),
      el("button", { className: "quiet", type: "button", onclick: () => api("POST", `/api/accounts/${a.name}/login`).then((r) => openConsole(r.id), fail) }, "Sign in"));
  });
  $("accounts").replaceChildren(...(accts.length ? accts : [el("li", { className: "empty" }, "Add an account label, then sign it in.")]));

  if (open) {
    const r = state.runs.find((x) => x.id === open.id);
    if (r) {
      $("console-title").textContent = r.name;
      $("console-hint").hidden = !r.hint;
      $("console-hint").textContent = hints[r.hint] || "";
      $("console-url").hidden = !r.url || r.done;
      if (r.url) $("console-url").href = r.url;
      $("stop").hidden = r.done;
    }
  }
}

async function refresh() {
  try { state = await api("GET", "/api/state"); render(); } catch (e) { fail(e); }
}

function send(data) {
  if (open) api("POST", `/api/runs/${open.id}/input`, { data }).catch(fail);
}

function openConsole(id) {
  closeConsole();
  $("home").hidden = true;
  $("console").hidden = false;
  const width = Math.min(document.documentElement.clientWidth - 44, 720);
  const fontSize = 12;
  const cols = Math.max(40, Math.min(120, Math.floor(width / (fontSize * 0.602))));
  const rows = 24;
  const term = new Terminal({ cols, rows, fontSize, fontFamily: "Menlo, monospace", cursorBlink: true, scrollback: 2000 });
  term.open($("term"));
  term.onData(send);
  const source = new EventSource(`/api/runs/${id}/stream`);
  source.onmessage = (e) => term.write(Uint8Array.from(atob(e.data), (c) => c.charCodeAt(0)));
  source.addEventListener("end", () => { source.close(); refresh(); });
  open = { id, term, source };
  api("POST", `/api/runs/${id}/resize`, { cols, rows }).catch(() => {});
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
$("stop").onclick = () => open && api("POST", `/api/runs/${open.id}/stop`).then(refresh, fail);
$("keys").onclick = (e) => { if (e.target.dataset.k) send(e.target.dataset.k); };
$("paste").onsubmit = (e) => {
  e.preventDefault();
  const v = $("paste-text").value;
  if (v) send(v + "\r");
  $("paste-text").value = "";
};
$("start").onsubmit = (e) => {
  e.preventDefault();
  api("POST", "/api/servers", { account: $("account").value, project: $("project").value, spawn: $("spawn").value })
    .then((r) => openConsole(r.id), fail);
};
$("add-account").onsubmit = (e) => {
  e.preventDefault();
  api("POST", "/api/accounts", { name: $("account-name").value }).then(() => { $("account-name").value = ""; refresh(); }, fail);
};
$("add-project").onsubmit = (e) => {
  e.preventDefault();
  api("POST", "/api/projects", { name: $("project-name").value }).then((r) => {
    $("project-name").value = "";
    return refresh().then(() => { $("project").value = r.project; });
  }, fail);
};

refresh();
setInterval(refresh, 4000);
