// The manager's panel: its accounts, logging one in (two-step code included)
// and removing one, over the manager's own API on the same origin. Nothing
// is kept in the browser but the key; the password stays in this page's
// memory only until the login it went with has its answer.
"use strict";

const zh = {
  Accounts: "账户",
  "At most 10 accounts.": "最多 10 个账户。",
  "Add account": "添加账户",
  "Apple ID": "Apple ID",
  Password: "密码",
  "Verification code": "验证码",
  "Log in": "登录",
  "Logging in…": "正在登录…",
  Verify: "验证",
  "Verifying…": "正在验证…",
  Cancel: "取消",
  "Start over": "重新开始",
  Remove: "移除",
  "No API_KEY: anyone who can reach this address can use these accounts.":
    "未设置 API_KEY：能访问这个地址的人都能使用这些账户。",
  Test: "测试",
  "Test {name}": "测试 {name}",
  "Testing…": "测试中…",
  "Key fetched in {ms} ms": "取到 key，{ms} ms",
  "Key fetch failed: {why}": "取 key 失败：{why}",
  "Remove {name}?": "移除 {name}？",
  "Remove {name}": "移除 {name}",
  Ready: "就绪",
  Starting: "正在启动",
  Unreachable: "无法连接",
  Serving: "可用",
  "Not serving yet": "尚未就绪",
  "{n} accounts": "{n} 个账户",
  "1 account": "1 个账户",
  "No accounts": "没有账户",
  "serving {regions}": "可用地区：{regions}",
  "This account is already logged in.": "这个账户已经登录。",
  "The login took too long. Try again.": "登录超时，请重试。",
  "The login was canceled.": "登录已取消。",
  "The code came too late or the login ended. Start over.": "验证码过期或登录已结束，请重新开始。",
  "This manager holds 10 accounts already: remove one first.": "已经有 10 个账户了，请先移除一个。",
  "The manager didn't answer.": "manager 没有响应。",
  "Wrapper key": "Wrapper 密钥",
  Continue: "继续",
  "That isn't the key.": "密钥不对。",
  "Forget the key": "忘记密钥",
  Show: "显示",
  Hide: "隐藏",
  Copy: "复制",
  "Copied": "已复制",
};

const lang = navigator.language?.toLowerCase().startsWith("zh") ? "zh" : "en";
const t = (s, vars = {}) => {
  let out = (lang === "zh" && zh[s]) || s;
  for (const [k, v] of Object.entries(vars)) out = out.replace(`{${k}}`, v);
  return out;
};
document.documentElement.lang = lang === "zh" ? "zh-CN" : "en";
for (const el of document.querySelectorAll("[data-t]")) el.textContent = t(el.dataset.t);

const regionNames = (() => {
  try {
    return new Intl.DisplayNames([document.documentElement.lang], { type: "region" });
  } catch {
    return null;
  }
})();
const regionName = (code) => {
  if (!code) return "";
  try {
    return regionNames?.of(code.toUpperCase()) ?? code.toUpperCase();
  } catch {
    return code.toUpperCase();
  }
};

const $ = (id) => document.getElementById(id);

// The key, when the wrapper asks for one (a manager with API_KEY, or the
// Cloudflare gateway), is kept in this browser: the panel is its owner's.
// No cookie, so no other site can use it. Opened as /<key>/ (the
// Cloudflare gateway takes the key from the path's first segment), the page
// keeps that key as if it were typed and takes it out of the address. A
// segment shorter than a key is a path the page is served under instead.
const store = {
  get: () => localStorage.getItem("key") ?? "",
  set: (key) => localStorage.setItem("key", key),
  forget: () => localStorage.removeItem("key"),
};
let key = "";
try {
  key = store.get();
} catch {}
{
  const first = location.pathname.split("/")[1] ?? "";
  let given = "";
  try {
    given = decodeURIComponent(first);
  } catch {}
  if (given.length >= 32) {
    key = given;
    try {
      store.set(key);
    } catch {}
    history.replaceState(null, "", "/" + location.search + location.hash);
  }
}

class Refused extends Error {}

// The manager's answers are {code, msg, data}; code 0 is success, 2 asks
// for the two-step code. A 401 is the key, refused or missing.
async function call(path, body, signal) {
  const headers = body ? { "Content-Type": "application/json" } : {};
  if (key) headers.Authorization = `Bearer ${key}`;
  const res = await fetch(path, {
    method: body ? "POST" : "GET",
    headers,
    body: body ? JSON.stringify(body) : undefined,
    signal,
    cache: "no-store",
  });
  if (res.status === 401) throw new Refused();
  const text = await res.text();
  try {
    return JSON.parse(text);
  } catch {
    return { code: -1, msg: text.trim() || `HTTP ${res.status}` };
  }
}

// What the manager says, in words for a person.
function why(msg) {
  const m = (msg || "").toLowerCase();
  if (m === "already login") return t("This account is already logged in.");
  if (m === "login timeout") return t("The login took too long. Try again.");
  if (m === "login canceled") return t("The login was canceled.");
  if (m.startsWith("no pending login")) return t("The code came too late or the login ended. Start over.");
  if (m.startsWith("account limit")) return t("This manager holds 10 accounts already: remove one first.");
  return msg || t("The manager didn't answer.");
}

// The accounts.

let accounts = [];
let removing = new Set();
// The last test of each account: it has the account fetch a key from Apple.
const tests = new Map();

async function testAccount(id) {
  tests.set(id, { busy: true });
  renderAccounts();
  let r;
  try {
    r = await call("check", { id });
  } catch (e) {
    tests.delete(id);
    if (e instanceof Refused) askKey(true);
    else renderAccounts();
    return;
  }
  tests.set(
    id,
    r.code === 0
      ? { ok: true, text: t("Key fetched in {ms} ms", { ms: r.data.ms }) }
      : { ok: false, text: t("Key fetch failed: {why}", { why: why(r.msg) }) },
  );
  renderAccounts();
}

// Whether the wrapper answers without a key, asked once: then the red note
// says so. Behind a key (API_KEY, or the Cloudflare gateway) there's nothing
// to say.
let exposed = null;
async function checkExposed() {
  if (exposed !== null) return;
  if (!key) {
    exposed = true;
  } else {
    try {
      const res = await fetch("status", { cache: "no-store" });
      exposed = res.status !== 401;
    } catch {
      return;
    }
  }
  $("exposed").hidden = !exposed;
}

function renderAccounts() {
  const list = $("accounts");
  for (const row of list.querySelectorAll(".account")) row.remove();
  $("empty").hidden = accounts.length > 0;
  $("limit").hidden = accounts.length < 10;
  for (const a of accounts) {
    const row = document.createElement("div");
    row.className = "account";

    const dot = document.createElement("span");
    dot.className = a.ready ? "dot" : "dot waiting";
    dot.title = a.ready ? t("Serving") : t("Not serving yet");
    row.append(dot);

    const who = document.createElement("div");
    who.className = "who";
    const name = document.createElement("div");
    name.className = a.name ? "name" : "name mono";
    name.textContent = a.name || a.id;
    who.append(name);
    const facts = document.createElement("div");
    facts.className = "facts";
    if (a.region) {
      const r = document.createElement("span");
      r.className = "region";
      r.textContent = `${a.region.toUpperCase()} · ${regionName(a.region)}`;
      facts.append(r);
    }
    if (a.name) {
      const id = document.createElement("span");
      id.textContent = a.id.slice(0, 8);
      id.title = a.id;
      facts.append(id);
    }
    if (!a.ready) {
      const s = document.createElement("span");
      s.textContent = t("Not serving yet");
      facts.append(s);
    }
    const tested = tests.get(a.id);
    if (tested && !tested.busy) {
      const s = document.createElement("span");
      s.className = tested.ok ? "tested ok" : "tested bad";
      s.textContent = tested.text;
      s.title = tested.text;
      facts.append(s);
    }
    who.append(facts);
    row.append(who);

    const test = document.createElement("button");
    test.type = "button";
    test.className = "ghost small";
    test.disabled = !a.ready || !!tested?.busy || removing.has(a.id);
    test.textContent = tested?.busy ? t("Testing…") : t("Test");
    test.ariaLabel = t("Test {name}", { name: a.name || a.id });
    test.addEventListener("click", () => void testAccount(a.id));
    row.append(test);

    const remove = document.createElement("button");
    remove.type = "button";
    remove.className = "icon";
    remove.disabled = removing.has(a.id);
    remove.title = remove.ariaLabel = t("Remove {name}", { name: a.name || a.id });
    remove.innerHTML =
      '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M4.5 6h11M8 6V4.5h4V6M6 6l.7 9.5h6.6L14 6" /></svg>';
    remove.addEventListener("click", () => confirmRemove(a));
    row.append(remove);

    list.append(row);
  }
}

function renderStatus(data, reached) {
  const state = $("state");
  state.hidden = false;
  if (!reached) {
    state.className = "chip bad";
    state.textContent = t("Unreachable");
    $("summary").textContent = location.host;
    return;
  }
  state.className = data.ready ? "chip ok" : "chip warn";
  state.textContent = data.ready ? t("Ready") : t("Starting");
  const n = accounts.length;
  const count = n === 0 ? t("No accounts") : n === 1 ? t("1 account") : t("{n} accounts", { n });
  const regions = (data.regions ?? []).map((r) => r.toUpperCase()).join(", ");
  $("summary").textContent = [location.host, count, regions && t("serving {regions}", { regions })]
    .filter(Boolean)
    .join(" · ");
}

// Asking for the key hides everything else: without it there's nothing to show.
function askKey(wrong) {
  const shown = !$("key-section").hidden;
  $("key-section").hidden = false;
  $("managed").hidden = true;
  $("state").hidden = true;
  $("summary").textContent = location.host;
  $("key-error").textContent = wrong ? t("That isn't the key.") : "";
  $("key-error").hidden = !wrong;
  if (!shown) $("key").focus();
}

function keyAccepted() {
  $("key-section").hidden = true;
  $("managed").hidden = false;
  $("forget-row").hidden = !key;
  $("shown-section").hidden = !key;
  if ($("shown").value !== key) {
    $("shown").value = key;
    reveal(false);
  }
}

// The key, shown masked under the accounts, to copy into amdl elsewhere.
function reveal(on) {
  $("shown").type = on ? "text" : "password";
  $("reveal").textContent = on ? t("Hide") : t("Show");
}
$("reveal").addEventListener("click", () => reveal($("shown").type === "password"));
$("copy").addEventListener("click", async () => {
  try {
    await navigator.clipboard.writeText(key);
  } catch {
    return;
  }
  $("copy").textContent = t("Copied");
  setTimeout(() => ($("copy").textContent = t("Copy")), 1500);
});

async function refresh() {
  try {
    const r = await call("status");
    if (r.code !== 0) throw new Error(r.msg);
    keyAccepted();
    void checkExposed();
    accounts = r.data.instances ?? [];
    for (const id of tests.keys()) if (!accounts.some((a) => a.id === id)) tests.delete(id);
    renderAccounts();
    renderStatus(r.data, true);
  } catch (e) {
    if (e instanceof Refused) askKey(!!key);
    else renderStatus({}, false);
  }
}

$("key-form").addEventListener("submit", (e) => {
  e.preventDefault();
  key = $("key").value.trim();
  try {
    store.set(key);
  } catch {}
  exposed = null;
  $("key").value = "";
  void refresh();
});
$("forget").addEventListener("click", () => {
  try {
    store.forget();
  } catch {}
  key = "";
  exposed = null;
  $("shown").value = "";
  $("shown-section").hidden = true;
  void refresh();
});

function confirmRemove(a) {
  const dialog = $("confirm");
  $("confirm-title").textContent = t("Remove {name}?", { name: a.name || a.id });
  dialog.returnValue = "";
  dialog.onclose = async () => {
    if (dialog.returnValue !== "remove") return;
    removing.add(a.id);
    renderAccounts();
    const r = await call("logout", { id: a.id }).catch((e) => ({ code: -1, msg: "", refused: e instanceof Refused }));
    if (r.refused) {
      removing.delete(a.id);
      askKey(true);
      return;
    }
    removing.delete(a.id);
    if (r.code !== 0) showError(why(r.msg));
    await refresh();
  };
  dialog.showModal();
  $("confirm-remove").focus();
}

// Logging in. The first request starts the login and answers once it's done
// or Apple asks for the code; the second hands the code over with the same
// Apple ID and password. Leaving a request (Cancel) stops the login.

const form = $("login");
let step = "credentials"; // or "code"
let inFlight = null; // the AbortController of the request on its way
let password = "";

function showError(text) {
  $("error").textContent = text;
  $("error").hidden = !text;
}

function renderLogin() {
  const busy = inFlight !== null;
  const code = step === "code";
  $("credentials").hidden = code;
  $("code-row").hidden = !code;
  $("user").disabled = $("pass").disabled = busy;
  $("code").disabled = busy;
  const submit = $("submit");
  submit.textContent = code ? t(busy ? "Verifying…" : "Verify") : t(busy ? "Logging in…" : "Log in");
  submit.disabled = busy || (code ? !/^\d{6}$/.test($("code").value) : !$("user").value || !$("pass").value);
  const back = $("back");
  back.hidden = !busy && !code;
  back.textContent = busy ? t("Cancel") : t("Start over");
}

function finish(r) {
  if (r.code === 2) {
    step = "code";
    renderLogin();
    $("code").value = "";
    $("code").focus();
    return;
  }
  if (r.code === 0) {
    step = "credentials";
    password = "";
    form.reset();
    showError("");
    void refresh();
  } else {
    // A login that fails at the code is over: it starts again from the password.
    if (step === "code") startOver();
    showError(why(r.msg));
  }
}

async function send(body) {
  inFlight = new AbortController();
  showError("");
  renderLogin();
  // The login sends a refresh along: the account shows as it's added.
  const poll = setInterval(refresh, 3000);
  try {
    finish(await call("login", body, inFlight.signal));
  } catch (e) {
    if (e instanceof Refused) {
      inFlight = null;
      askKey(true);
      return;
    }
    finish({ code: -1, msg: e.name === "AbortError" ? "login canceled" : "" });
  } finally {
    clearInterval(poll);
    inFlight = null;
    renderLogin();
  }
}

function startOver() {
  step = "credentials";
  password = "";
  $("code").value = "";
  renderLogin();
  $("pass").value = "";
  $("pass").focus();
}

form.addEventListener("input", renderLogin);
form.addEventListener("submit", (e) => {
  e.preventDefault();
  if (inFlight) return;
  const username = $("user").value.trim();
  if (step === "credentials") {
    password = $("pass").value;
    void send({ username, password });
  } else {
    void send({ username, password, code: $("code").value.trim() });
  }
});
$("back").addEventListener("click", () => {
  if (inFlight) inFlight.abort();
  else startOver();
});

renderLogin();
void refresh();
setInterval(() => {
  if (!document.hidden) void refresh();
}, 5000);
document.addEventListener("visibilitychange", () => {
  if (!document.hidden) void refresh();
});
