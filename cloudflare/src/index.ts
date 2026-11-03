import {
	generateAuthenticationOptions,
	generateRegistrationOptions,
	verifyAuthenticationResponse,
	verifyRegistrationResponse,
	type AuthenticationResponseJSON,
	type RegistrationResponseJSON,
} from "@simplewebauthn/server";
import { Env, PAIR_EXPIRES_MS, PAIR_INTERVAL_S, SESSION_TTL_MS } from "./do";
import { claimPage, devicesPage, homePage, pairApprovePage, pairCodePage, pairResultPage, pairSignInPage, signInPage } from "./pages";
import { b64, displayUserCode, hashOf, normalizeUserCode, unb64 } from "./util";

export { WrapperContainer } from "./do";

const PROXIED = new Set(["/status", "/token", "/m3u8", "/key", "/lyrics", "/webplayback", "/license", "/login", "/logout"]);
const SESSION_COOKIE = "amdl_sess";
const RP_NAME = "amdl wrapper";

const json = (status: number, body: unknown, headers: Record<string, string> = {}) =>
	new Response(JSON.stringify(body), {
		status,
		headers: { "content-type": "application/json", "cache-control": "no-store", ...headers },
	});
const failure = (status: number, msg: string) => json(status, { code: status, msg });
const oauthError = (error: string) => json(400, { error });
const html = (body: string, status = 200) =>
	new Response(body, { status, headers: { "content-type": "text/html; charset=utf-8", "cache-control": "no-store" } });
const redirect = (to: string) => new Response(null, { status: 303, headers: { location: to } });

function stub(env: Env) {
	return env.WRAPPER.get(env.WRAPPER.idFromName("main"));
}

function sessionCookie(token: string): string {
	return `${SESSION_COOKIE}=${token}; HttpOnly; Secure; SameSite=Strict; Path=/; Max-Age=${SESSION_TTL_MS / 1000}`;
}

const clearSessionCookie = `${SESSION_COOKIE}=; HttpOnly; Secure; SameSite=Strict; Path=/; Max-Age=0`;

function cookieToken(request: Request): string | null {
	const cookie = request.headers.get("cookie") ?? "";
	const match = cookie.match(new RegExp(`(?:^|;\\s*)${SESSION_COOKIE}=([^;\\s]+)`));
	return match ? match[1] : null;
}

// sessionHash validates the cookie against the DO and returns the stored
// digest, which also binds registration challenges to this session.
async function sessionHash(request: Request, env: Env): Promise<string | null> {
	const token = cookieToken(request);
	if (!token) return null;
	const hash = await hashOf(token);
	return (await stub(env).sessionValid(hash)) ? hash : null;
}

function clientIp(request: Request): string {
	return request.headers.get("cf-connecting-ip") ?? "local";
}

// authorized checks the bearer digest against the devices' access digests.
// One DO storage read answers a wrong token 401 without waking the container.
async function authorized(request: Request, env: Env): Promise<boolean> {
	const header = request.headers.get("authorization");
	if (!header?.startsWith("Bearer ")) return false;
	const deviceId = await stub(env).accessLookup(await hashOf(header.slice("Bearer ".length)), clientIp(request));
	return deviceId !== null;
}

async function readJson(request: Request): Promise<Record<string, unknown> | null> {
	try {
		const body = await request.json();
		return body && typeof body === "object" ? (body as Record<string, unknown>) : null;
	} catch {
		return null;
	}
}

// ---- WebAuthn ---------------------------------------------------------

async function registerOptions(request: Request, env: Env, url: URL): Promise<Response> {
	const keys = await stub(env).passkeys();
	const claimed = keys.length > 0;
	const session = await sessionHash(request, env);
	if (claimed && !session) return failure(401, "sign in first");
	const options = await generateRegistrationOptions({
		rpName: RP_NAME,
		rpID: url.hostname,
		userName: "owner",
		userDisplayName: "owner",
		attestationType: "none",
		excludeCredentials: keys.map((k) => ({ id: k.id, transports: k.transports })),
		authenticatorSelection: { residentKey: "preferred", userVerification: "preferred" },
	});
	await stub(env).putChallenge(options.challenge, "reg", claimed ? session! : undefined);
	return json(200, options);
}

async function registerVerify(request: Request, env: Env, url: URL): Promise<Response> {
	const keys = await stub(env).passkeys();
	const claimed = keys.length > 0;
	const session = await sessionHash(request, env);
	if (claimed && !session) return failure(401, "sign in first");
	const body = (await readJson(request)) as RegistrationResponseJSON | null;
	if (!body?.response) return failure(400, "bad response");
	let verified;
	try {
		verified = await verifyRegistrationResponse({
			response: body,
			expectedChallenge: (challenge) => stub(env).consumeChallenge(challenge, "reg", claimed ? session! : undefined),
			expectedOrigin: url.origin,
			expectedRPID: url.hostname,
			requireUserVerification: false,
		});
	} catch (err) {
		return failure(400, err instanceof Error ? err.message : "verification failed");
	}
	if (!verified.verified) return failure(400, "verification failed");
	const credential = verified.registrationInfo.credential;
	await stub(env).savePasskey({
		id: credential.id,
		publicKey: b64(credential.publicKey),
		counter: credential.counter,
		transports: credential.transports,
		createdAt: Date.now(),
	});
	const headers: Record<string, string> = {};
	if (!claimed) headers["set-cookie"] = sessionCookie(await stub(env).makeSession());
	return json(200, { ok: true }, headers);
}

async function loginOptions(request: Request, env: Env, url: URL): Promise<Response> {
	const keys = await stub(env).passkeys();
	if (!keys.length) return failure(409, "not claimed");
	const options = await generateAuthenticationOptions({
		rpID: url.hostname,
		allowCredentials: keys.map((k) => ({ id: k.id, transports: k.transports })),
		userVerification: "preferred",
	});
	await stub(env).putChallenge(options.challenge, "auth");
	return json(200, options);
}

async function loginVerify(request: Request, env: Env, url: URL): Promise<Response> {
	const keys = await stub(env).passkeys();
	if (!keys.length) return failure(409, "not claimed");
	const body = (await readJson(request)) as AuthenticationResponseJSON | null;
	if (!body?.response) return failure(400, "bad response");
	const key = keys.find((k) => k.id === body.id);
	if (!key) return failure(400, "unknown credential");
	let verified;
	try {
		verified = await verifyAuthenticationResponse({
			response: body,
			expectedChallenge: (challenge) => stub(env).consumeChallenge(challenge, "auth"),
			expectedOrigin: url.origin,
			expectedRPID: url.hostname,
			credential: {
				id: key.id,
				publicKey: unb64(key.publicKey) as Uint8Array<ArrayBuffer>,
				counter: key.counter,
				transports: key.transports,
			},
			requireUserVerification: false,
		});
	} catch (err) {
		return failure(400, err instanceof Error ? err.message : "verification failed");
	}
	if (!verified.verified) return failure(400, "verification failed");
	await stub(env).updatePasskeyCounter(key.id, verified.authenticationInfo.newCounter);
	return json(200, { ok: true }, { "set-cookie": sessionCookie(await stub(env).makeSession()) });
}

// ---- pages ------------------------------------------------------------

async function rootPage(request: Request, env: Env): Promise<Response> {
	const claimed = (await stub(env).passkeys()).length > 0;
	if (!claimed) return html(claimPage());
	return html((await sessionHash(request, env)) ? homePage() : signInPage());
}

async function pairPage(request: Request, env: Env, url: URL): Promise<Response> {
	const claimed = (await stub(env).passkeys()).length > 0;
	if (!claimed) return redirect("/");
	const code = normalizeUserCode(url.searchParams.get("code") ?? "");
	const result = url.searchParams.get("r");
	if (!(await sessionHash(request, env))) return html(pairSignInPage(code || null));
	if (result === "approved" || result === "denied" || result === "gone") return html(pairResultPage(displayUserCode(code), result));
	if (!code) return html(pairCodePage());
	const info = await stub(env).pairInfo(code);
	if (!info || info.state !== "pending") return html(pairResultPage(displayUserCode(code), "gone"));
	return html(pairApprovePage(displayUserCode(code), info.name));
}

async function pairDecide(request: Request, env: Env): Promise<Response> {
	if (!(await sessionHash(request, env))) return failure(401, "sign in first");
	const form = await request.formData().catch(() => null);
	if (!form) return failure(400, "bad form");
	const code = normalizeUserCode(String(form.get("code") ?? ""));
	const approve = form.get("action") === "approve";
	const decided = await stub(env).pairDecide(code, approve);
	const result = !decided.ok ? "gone" : approve ? "approved" : "denied";
	return redirect(`/pair?code=${encodeURIComponent(displayUserCode(code))}&r=${result}`);
}

async function devicesRoute(request: Request, env: Env): Promise<Response> {
	if (!(await sessionHash(request, env))) return redirect("/");
	return html(devicesPage(await stub(env).devices()));
}

async function devicesRevoke(request: Request, env: Env, all: boolean): Promise<Response> {
	if (!(await sessionHash(request, env))) return failure(401, "sign in first");
	if (all) await stub(env).revokeAll();
	else {
		const form = await request.formData().catch(() => null);
		if (!form) return failure(400, "bad form");
		await stub(env).revoke(String(form.get("id") ?? ""));
	}
	return redirect("/devices");
}

async function logout(request: Request, env: Env): Promise<Response> {
	const token = cookieToken(request);
	if (token) await stub(env).dropSession(await hashOf(token));
	return new Response(null, { status: 303, headers: { location: "/", "set-cookie": clearSessionCookie } });
}

// ---- device flow ------------------------------------------------------

async function pairStart(request: Request, env: Env, url: URL): Promise<Response> {
	const body = await readJson(request);
	const name = typeof body?.name === "string" ? body.name.trim() : "";
	if (!name || name.length > 100) return oauthError("invalid_client");
	const started = await stub(env).pairStart(name, await hashOf(clientIp(request)));
	if ("error" in started) return json(429, { error: "rate_limited" });
	const code = displayUserCode(started.userCode);
	return json(200, {
		user_code: code,
		verification_url: `${url.origin}/pair`,
		verification_url_complete: `${url.origin}/pair?code=${code}`,
		device_code: started.deviceCode,
		interval: PAIR_INTERVAL_S,
		expires_in: PAIR_EXPIRES_MS / 1000,
	});
}

async function pairPoll(request: Request, env: Env): Promise<Response> {
	const body = await readJson(request);
	const deviceCode = typeof body?.device_code === "string" ? body.device_code : "";
	if (!deviceCode) return oauthError("expired_token");
	const result = await stub(env).pairPoll(deviceCode, clientIp(request));
	switch (result.status) {
		case "approved":
			return new Response(result.tokens, { headers: { "content-type": "application/json", "cache-control": "no-store" } });
		case "pending":
			return oauthError("authorization_pending");
		case "slow":
			return oauthError("slow_down");
		case "denied":
			return oauthError("access_denied");
		default:
			return oauthError("expired_token");
	}
}

async function pairRefresh(request: Request, env: Env): Promise<Response> {
	const body = await readJson(request);
	const refreshToken = typeof body?.refresh_token === "string" ? body.refresh_token : "";
	if (!refreshToken) return oauthError("invalid_grant");
	const result = await stub(env).refresh(refreshToken, clientIp(request));
	if (!result.ok) return oauthError("invalid_grant");
	return new Response(result.tokens, { headers: { "content-type": "application/json", "cache-control": "no-store" } });
}

export default {
	async fetch(request: Request, env: Env): Promise<Response> {
		const url = new URL(request.url);
		if (url.protocol !== "https:" && url.hostname !== "localhost") return failure(403, "https only");

		switch (`${request.method} ${url.pathname}`) {
			case "GET /":
				return rootPage(request, env);
			case "POST /logout":
				return logout(request, env);
			case "POST /webauthn/register/options":
				return registerOptions(request, env, url);
			case "POST /webauthn/register/verify":
				return registerVerify(request, env, url);
			case "POST /webauthn/login/options":
				return loginOptions(request, env, url);
			case "POST /webauthn/login/verify":
				return loginVerify(request, env, url);
			case "GET /pair":
				return pairPage(request, env, url);
			case "POST /pair/decide":
				return pairDecide(request, env);
			case "GET /devices":
				return devicesRoute(request, env);
			case "POST /devices/revoke":
				return devicesRevoke(request, env, false);
			case "POST /devices/revoke-all":
				return devicesRevoke(request, env, true);
			case "POST /pair/start":
				return pairStart(request, env, url);
			case "POST /pair/poll":
				return pairPoll(request, env);
			case "POST /pair/refresh":
				return pairRefresh(request, env);
		}

		if (!(await authorized(request, env))) return failure(401, "unauthorized");
		if (!PROXIED.has(url.pathname)) return failure(404, "not found");
		const forwarded = new Request(request);
		forwarded.headers.delete("authorization");
		return stub(env).fetch(forwarded);
	},

	async scheduled(_controller: ScheduledController, env: Env, ctx: ExecutionContext): Promise<void> {
		ctx.waitUntil(stub(env).renew().then(() => {}));
	},
} satisfies ExportedHandler<Env>;
