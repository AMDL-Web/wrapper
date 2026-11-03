// The owner's pages: claiming the deployment with a passkey, approving pair
// requests and managing devices. Nothing here reveals anything about the
// accounts. Inline JS only where WebAuthn needs it; the rest is plain HTML.

import { escapeHtml } from "./util";

const page = (title: string, body: string) => `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>${title}</title>
<style>
:root { color-scheme: light dark; --fg: #1d1d1f; --muted: #6e6e73; --bg: #fff; --panel: #f5f5f7; --accent: #0071e3; --danger: #d70015; }
@media (prefers-color-scheme: dark) { :root { --fg: #f5f5f7; --muted: #a1a1a6; --bg: #000; --panel: #1c1c1e; --accent: #0a84ff; --danger: #ff453a; } }
* { box-sizing: border-box; }
body { margin: 0; background: var(--bg); color: var(--fg); font: 16px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
main { max-width: 560px; margin: 0 auto; padding: 48px 16px; }
h1 { font-size: 26px; margin: 0 0 16px; }
h2 { font-size: 18px; margin: 32px 0 8px; }
p { margin: 0 0 16px; color: var(--muted); }
a { color: var(--accent); }
button, .btn { display: inline-block; font: inherit; border: 0; border-radius: 10px; padding: 10px 18px; background: var(--accent); color: #fff; cursor: pointer; text-decoration: none; }
button.secondary, .btn.secondary { background: var(--panel); color: var(--fg); }
button.danger { background: var(--danger); }
form { margin: 0 0 8px; }
form.inline { display: inline; }
input[type=text] { font: inherit; width: 100%; padding: 10px 12px; border: 1px solid var(--muted); border-radius: 10px; background: var(--panel); color: var(--fg); margin: 0 0 12px; }
code { display: block; padding: 14px; border-radius: 10px; background: var(--panel); font: 20px/1.4 ui-monospace, SFMono-Regular, Menlo, monospace; word-break: break-all; margin: 0 0 16px; text-align: center; }
dl { display: grid; grid-template-columns: max-content 1fr; gap: 8px 16px; margin: 0 0 16px; }
dt { color: var(--muted); }
dd { margin: 0; word-break: break-word; }
ul.devices { list-style: none; padding: 0; margin: 0 0 24px; }
ul.devices li { background: var(--panel); border-radius: 10px; padding: 12px 14px; margin: 0 0 10px; }
ul.devices .meta { font-size: 13px; color: var(--muted); margin: 4px 0 10px; }
nav { display: flex; gap: 16px; align-items: center; margin: 0 0 32px; font-size: 15px; }
nav form { margin: 0 0 0 auto; }
nav form button { background: none; color: var(--accent); padding: 0; font-size: 15px; }
.err { color: var(--danger); font-size: 14px; min-height: 20px; }
</style>
</head>
<body><main>${body}</main></body>
</html>`;

// The WebAuthn round trips, shared by every page that can sign in.
const webauthn = `<script>
const B=s=>Uint8Array.from(atob(s.replace(/-/g,"+").replace(/_/g,"/")),c=>c.charCodeAt(0));
const S=b=>btoa(String.fromCharCode.apply(null,new Uint8Array(b))).replace(/\\+/g,"-").replace(/\\//g,"_").replace(/=+$/,"");
const say=m=>{const e=document.getElementById("err");if(e)e.textContent=m};
async function j(u,o){const r=await fetch(u,{method:"POST",headers:{"content-type":"application/json"},body:JSON.stringify(o||{})});if(!r.ok)throw new Error((await r.json().catch(()=>({}))).msg||r.status);return r.json()}
async function register(){try{
const o=await j("/webauthn/register/options");
o.challenge=B(o.challenge);o.user.id=B(o.user.id);(o.excludeCredentials||[]).forEach(c=>c.id=B(c.id));
const c=await navigator.credentials.create({publicKey:o});
await j("/webauthn/register/verify",{id:c.id,rawId:S(c.rawId),type:c.type,response:{attestationObject:S(c.response.attestationObject),clientDataJSON:S(c.response.clientDataJSON),transports:c.response.getTransports?c.response.getTransports():[]},clientExtensionResults:c.getClientExtensionResults()});
location.reload();
}catch(e){say(e.message||"registration failed")}}
async function login(){try{
const o=await j("/webauthn/login/options");
o.challenge=B(o.challenge);(o.allowCredentials||[]).forEach(c=>c.id=B(c.id));
const a=await navigator.credentials.get({publicKey:o});
await j("/webauthn/login/verify",{id:a.id,rawId:S(a.rawId),type:a.type,response:{authenticatorData:S(a.response.authenticatorData),clientDataJSON:S(a.response.clientDataJSON),signature:S(a.response.signature),userHandle:a.response.userHandle?S(a.response.userHandle):null},clientExtensionResults:a.getClientExtensionResults()});
location.reload();
}catch(e){say(e.message||"sign-in failed")}}
</script>`;

export const claimPage = () =>
	page(
		"amdl wrapper",
		`<h1>amdl wrapper</h1>
<p>Claim this wrapper with a passkey.</p>
<button type="button" onclick="register()">Register passkey</button>
<div class="err" id="err"></div>
${webauthn}`,
	);

export const signInPage = () =>
	page(
		"amdl wrapper",
		`<h1>amdl wrapper</h1>
<button type="button" onclick="login()">Sign in with passkey</button>
<div class="err" id="err"></div>
${webauthn}`,
	);

export const homePage = () =>
	page(
		"amdl wrapper",
		`<nav><a href="/devices">Devices</a><form method="post" action="/logout"><button type="submit">Sign out</button></form></nav>
<h1>amdl wrapper</h1>
<h2>Pair a device</h2>
<p><a class="btn secondary" href="/pair">Enter a pairing code</a></p>
<h2>New API key</h2>
<form method="post" action="/keys"><input type="text" name="name" placeholder="Key name" autocomplete="off" required maxlength="100"><button type="submit">Create</button></form>
<h2>Add a passkey</h2>
<button type="button" onclick="register()">Register passkey</button>
<div class="err" id="err"></div>
${webauthn}`,
	);

export const pairSignInPage = (code: string | null) =>
	page(
		"Pair a device",
		`<h1>Pair a device</h1>
${code ? `<code>${escapeHtml(code)}</code>` : ""}
<button type="button" onclick="login()">Sign in with passkey</button>
<div class="err" id="err"></div>
${webauthn}`,
	);

export const pairCodePage = () =>
	page(
		"Pair a device",
		`<h1>Pair a device</h1>
<form method="get" action="/pair"><input type="text" name="code" placeholder="XXXX-XXXX" autocomplete="off" autocapitalize="characters" required><button type="submit">Continue</button></form>`,
	);

export const pairApprovePage = (code: string, name: string) =>
	page(
		"Pair a device",
		`<nav><a href="/devices">Devices</a><form method="post" action="/logout"><button type="submit">Sign out</button></form></nav>
<h1>Pair a device</h1>
<code>${escapeHtml(code)}</code>
<dl><dt>Device</dt><dd>${escapeHtml(name)}</dd></dl>
<form class="inline" method="post" action="/pair/decide"><input type="hidden" name="code" value="${escapeHtml(code)}"><button type="submit" name="action" value="approve">Approve</button></form>
<form class="inline" method="post" action="/pair/decide"><input type="hidden" name="code" value="${escapeHtml(code)}"><button type="submit" class="danger" name="action" value="deny">Deny</button></form>`,
	);

export const pairResultPage = (code: string, result: "approved" | "denied" | "gone") => {
	const text = { approved: "Device paired.", denied: "Request denied.", gone: "Request expired or already handled." }[result];
	return page("Pair a device", `<h1>Pair a device</h1><code>${escapeHtml(code)}</code><p>${text}</p><p><a href="/devices">Devices</a></p>`);
};

export const keyCreatedPage = (name: string, key: string) =>
	page(
		"API key",
		`<h1>API key</h1>
<dl><dt>Name</dt><dd>${escapeHtml(name)}</dd></dl>
<code>${escapeHtml(key)}</code>
<p>Shown once — copy it now.</p>
<p><a href="/devices">Devices</a></p>`,
	);

export type DeviceRow = { id: string; name: string; createdAt: number; lastUsedAt: number; lastIp: string };

const when = (ms: number) => (ms ? new Date(ms).toISOString().replace("T", " ").slice(0, 16) + "Z" : "—");

export const devicesPage = (devices: DeviceRow[], keys: DeviceRow[]) =>
	page(
		"Devices",
		`<nav><a href="/">Home</a><form method="post" action="/logout"><button type="submit">Sign out</button></form></nav>
<h1>Devices</h1>
${
	devices.length
		? `<ul class="devices">${devices
				.map(
					(d) => `<li><strong>${escapeHtml(d.name)}</strong>
<div class="meta">paired ${when(d.createdAt)} · used ${when(d.lastUsedAt)}${d.lastIp ? ` · ${escapeHtml(d.lastIp)}` : ""}</div>
<form method="post" action="/devices/revoke"><input type="hidden" name="id" value="${escapeHtml(d.id)}"><button type="submit" class="danger">Revoke</button></form></li>`,
				)
				.join("")}</ul>
<form method="post" action="/devices/revoke-all"><button type="submit" class="secondary">Revoke all</button></form>`
		: `<p>No paired devices.</p>`
}
<h2>API keys</h2>
<form method="post" action="/keys"><input type="text" name="name" placeholder="Key name" autocomplete="off" required maxlength="100"><button type="submit">Create</button></form>
${
	keys.length
		? `<ul class="devices">${keys
				.map(
					(k) => `<li><strong>${escapeHtml(k.name)}</strong>
<div class="meta">created ${when(k.createdAt)}${k.lastUsedAt ? ` · used ${when(k.lastUsedAt)}` : ""}${k.lastIp ? ` · ${escapeHtml(k.lastIp)}` : ""}</div>
<form method="post" action="/keys/revoke"><input type="hidden" name="id" value="${escapeHtml(k.id)}"><button type="submit" class="danger">Revoke</button></form></li>`,
				)
				.join("")}</ul>`
		: `<p>No API keys.</p>`
}`,
	);
