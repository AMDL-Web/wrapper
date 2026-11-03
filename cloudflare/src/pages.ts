// The setup pages: the first visitor creates the bearer token, which is shown
// once. Nothing here reveals anything about the accounts.

const page = (title: string, body: string) => `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>${title}</title>
<style>
:root { color-scheme: light dark; --fg: #1d1d1f; --muted: #6e6e73; --bg: #fff; --panel: #f5f5f7; --accent: #0071e3; }
@media (prefers-color-scheme: dark) { :root { --fg: #f5f5f7; --muted: #a1a1a6; --bg: #000; --panel: #1c1c1e; --accent: #0a84ff; } }
* { box-sizing: border-box; }
body { margin: 0; background: var(--bg); color: var(--fg); font: 16px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
main { max-width: 560px; margin: 0 auto; padding: 64px 16px; }
h1 { font-size: 28px; margin: 0 0 16px; }
p { margin: 0 0 16px; color: var(--muted); }
button { font: inherit; border: 0; border-radius: 10px; padding: 10px 18px; background: var(--accent); color: #fff; cursor: pointer; }
code { display: block; padding: 14px; border-radius: 10px; background: var(--panel); font: 14px/1.4 ui-monospace, SFMono-Regular, Menlo, monospace; word-break: break-all; margin: 0 0 16px; }
dl { display: grid; grid-template-columns: max-content 1fr; gap: 8px 16px; margin: 0 0 16px; }
dt { color: var(--muted); }
dd { margin: 0; word-break: break-all; }
</style>
</head>
<body><main>${body}</main></body>
</html>`;

export const setupPage = () =>
	page(
		"amdl wrapper setup",
		`<h1>amdl wrapper</h1>
<p>Create the token amdl uses to reach this wrapper. It is shown once.</p>
<form method="post" action="/setup"><button type="submit">Create token</button></form>`,
	);

export const tokenPage = (token: string, origin: string) =>
	page(
		"amdl wrapper token",
		`<h1>Token created</h1>
<p>Copy it now; it is not shown again.</p>
<code id="token">${token}</code>
<p><button type="button" onclick="navigator.clipboard.writeText(document.getElementById('token').textContent).then(()=>{this.textContent='Copied'})">Copy</button></p>
<dl><dt>Wrapper URL</dt><dd>${origin}</dd><dt>Token</dt><dd>the value above</dd></dl>`,
	);

export const homePage = () => page("amdl wrapper", `<h1>amdl wrapper</h1><p>Set up.</p>`);
