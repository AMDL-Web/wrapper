import { env } from "cloudflare:workers";
import { reset, runInDurableObject, SELF } from "cloudflare:test";
import { afterEach, describe, expect, it } from "vitest";
import type { Env } from "../src/do";
import { b64url, FakeAuthenticator, ORIGIN, sha256 } from "./helpers";

afterEach(async () => {
	await reset();
});

const typedEnv = env as unknown as Env;
const dobj = () => typedEnv.WRAPPER.get(typedEnv.WRAPPER.idFromName("main"));

const post = (path: string, body: unknown, headers: Record<string, string> = {}) =>
	SELF.fetch(
		new Request(`${ORIGIN}${path}`, {
			method: "POST",
			headers: { "content-type": "application/json", ...headers },
			body: JSON.stringify(body),
		}),
	);

const get = (path: string, headers: Record<string, string> = {}) =>
	SELF.fetch(new Request(`${ORIGIN}${path}`, { headers, redirect: "manual" }));

const bearer = (token: string) => ({ authorization: `Bearer ${token}` });

// ownerSession seeds a passkey (so the deployment counts as claimed) and a
// session straight in storage; the WebAuthn ceremony itself is covered by
// its own tests.
async function ownerSession(): Promise<string> {
	await runInDurableObject(dobj(), async (_instance, state) => {
		await state.storage.put("passkeys", [{ id: "test-key", publicKey: "", counter: 0, createdAt: Date.now() }]);
	});
	const token = await dobj().makeSession();
	return `amdl_sess=${token}`;
}

const postForm = (path: string, fields: Record<string, string>, cookie?: string) =>
	SELF.fetch(
		new Request(`${ORIGIN}${path}`, {
			method: "POST",
			redirect: "manual",
			headers: { "content-type": "application/x-www-form-urlencoded", ...(cookie ? { cookie } : {}) },
			body: new URLSearchParams(fields).toString(),
		}),
	);

const decide = (userCode: string, action: "approve" | "deny", cookie?: string) => postForm("/pair/decide", { code: userCode, action }, cookie);

// setPairField rewrites a grant's storage record, the test's way to jump
// through time without waiting out real expiries.
async function patchPair(deviceCode: string, patch: Record<string, unknown>): Promise<void> {
	const hash = b64url(await sha256(deviceCode));
	await runInDurableObject(dobj(), async (_instance, state) => {
		const key = `pair:dc:${hash}`;
		const pair = (await state.storage.get(key)) as Record<string, unknown>;
		Object.assign(pair, patch);
		await state.storage.put(key, pair);
	});
}

async function patchDevice(deviceId: string, patch: (device: Record<string, unknown>) => void): Promise<void> {
	await runInDurableObject(dobj(), async (_instance, state) => {
		const key = `dev:${deviceId}`;
		const device = (await state.storage.get(key)) as Record<string, unknown>;
		patch(device);
		await state.storage.put(key, device);
	});
}

type Tokens = { access_token: string; refresh_token: string; token_type: string; expires_in: number; device_id: string };

async function pairDevice(name = "amdl"): Promise<{ userCode: string; deviceCode: string; tokens: Tokens }> {
	const start = (await (await post("/pair/start", { name })).json()) as { user_code: string; device_code: string };
	const cookie = await ownerSession();
	const res = await decide(start.user_code, "approve", cookie);
	if (res.status !== 303) throw new Error(`decide answered ${res.status}`);
	const poll = await post("/pair/poll", { device_code: start.device_code });
	if (poll.status !== 200) throw new Error(`poll answered ${poll.status}: ${await poll.text()}`);
	return { userCode: start.user_code, deviceCode: start.device_code, tokens: (await poll.json()) as Tokens };
}

describe("passkey claim and sign-in", () => {
	it("claims the deployment, signs in with the passkey and rejects forgeries", async () => {
		const landing = await get("/");
		expect(landing.status).toBe(200);
		expect(await landing.text()).toContain("Register passkey");

		const authenticator = await FakeAuthenticator.create();
		const options = (await (await post("/webauthn/register/options", {})).json()) as { challenge: string };
		const registration = await post("/webauthn/register/verify", await authenticator.registrationResponse(options.challenge));
		expect(registration.status).toBe(200);
		const cookie = registration.headers.get("set-cookie")!;
		expect(cookie).toContain("amdl_sess=");
		expect(cookie).toContain("HttpOnly");
		expect(cookie).toContain("SameSite=Strict");

		const home = await get("/", { cookie: cookie.split(";")[0] });
		expect(await home.text()).toContain("Devices");

		// A fresh sign-in ceremony with the same key.
		const authOptions = (await (await post("/webauthn/login/options", {})).json()) as { challenge: string };
		const login = await post("/webauthn/login/verify", await authenticator.authenticationResponse(authOptions.challenge));
		if (login.status !== 200) console.log("login failed:", await login.text());
		expect(login.status).toBe(200);
		expect(login.headers.get("set-cookie")).toContain("amdl_sess=");

		// A garbage signature must not pass.
		const badOptions = (await (await post("/webauthn/login/options", {})).json()) as { challenge: string };
		const forged = await authenticator.authenticationResponse(badOptions.challenge);
		forged.response.signature = b64url(new Uint8Array(64));
		const rejected = await post("/webauthn/login/verify", forged);
		expect(rejected.status).toBe(400);

		// A challenge is single-use.
		const onceOptions = (await (await post("/webauthn/login/options", {})).json()) as { challenge: string };
		const once = await authenticator.authenticationResponse(onceOptions.challenge);
		expect((await post("/webauthn/login/verify", once)).status).toBe(200);
		expect((await post("/webauthn/login/verify", once)).status).toBe(400);

		// Registering another passkey needs the session.
		expect((await post("/webauthn/register/options", {})).status).toBe(401);
	});

	it("keeps the challenge bound to its kind", async () => {
		const authenticator = await FakeAuthenticator.create();
		const options = (await (await post("/webauthn/register/options", {})).json()) as { challenge: string };
		expect((await post("/webauthn/register/verify", await authenticator.registrationResponse(options.challenge))).status).toBe(200);
		// A registration challenge spent on an authentication fails.
		const regOptions = await post(
			"/webauthn/register/options",
			{},
			{ cookie: `amdl_sess=${(await dobj().makeSession())}` },
		);
		const wrong = await post("/webauthn/login/verify", await authenticator.authenticationResponse(((await regOptions.json()) as { challenge: string }).challenge));
		expect(wrong.status).toBe(400);
	});
});

describe("pairing", () => {
	it("answers pair start with the documented grant", async () => {
		const res = await post("/pair/start", { name: "amdl on macbook" });
		expect(res.status).toBe(200);
		const body = (await res.json()) as Record<string, unknown>;
		expect(body.user_code).toMatch(/^[A-Z2-9]{4}-[A-Z2-9]{4}$/);
		expect(body.verification_url).toBe(`${ORIGIN}/pair`);
		expect(body.verification_url_complete).toBe(`${ORIGIN}/pair?code=${body.user_code}`);
		expect(body.device_code).toMatch(/^[A-Za-z0-9_-]{43}$/);
		expect(body.interval).toBe(5);
		expect(body.expires_in).toBe(600);

		expect((await post("/pair/start", { name: "  " })).status).toBe(400);
		expect((await post("/pair/start", {})).status).toBe(400);
	});

	it("rate limits pair starts per IP", async () => {
		for (let i = 0; i < 10; i++) {
			expect((await post("/pair/start", { name: `device ${i}` })).status).toBe(200);
		}
		const limited = await post("/pair/start", { name: "device 11" });
		expect(limited.status).toBe(429);
	});

	it("walks poll through pending, slow_down, approved and denies cleanly", async () => {
		const start = (await (await post("/pair/start", { name: "amdl mini" })).json()) as {
			user_code: string;
			device_code: string;
		};

		const pending = await post("/pair/poll", { device_code: start.device_code });
		expect(pending.status).toBe(400);
		expect(await pending.json()).toEqual({ error: "authorization_pending" });

		const tooFast = await post("/pair/poll", { device_code: start.device_code });
		expect(tooFast.status).toBe(400);
		expect(await tooFast.json()).toEqual({ error: "slow_down" });

		// The approve page shows the device to the signed-in owner.
		const cookie = await ownerSession();
		const page = await get(`/pair?code=${start.user_code}`, { cookie });
		const html = await page.text();
		expect(html).toContain("amdl mini");
		expect(html).toContain(start.user_code);

		const approved = await decide(start.user_code, "approve", cookie);
		expect(approved.status).toBe(303);

		await patchPair(start.device_code, { lastPollAt: 0 });
		const granted = await post("/pair/poll", { device_code: start.device_code });
		expect(granted.status).toBe(200);
		const tokens = (await granted.json()) as Tokens;
		expect(tokens.token_type).toBe("Bearer");
		expect(tokens.expires_in).toBe(86400);
		expect(tokens.device_id).toBeTruthy();
		expect(tokens.access_token).toMatch(/^[A-Za-z0-9_-]{43}$/);
		expect(tokens.refresh_token).toMatch(/^[A-Za-z0-9_-]{43}$/);

		// A repeated poll before expiry returns the same grant.
		await patchPair(start.device_code, { lastPollAt: 0 });
		const again = await post("/pair/poll", { device_code: start.device_code });
		expect(await again.json()).toEqual(tokens);
	});

	it("reports access_denied after the owner denies", async () => {
		const start = (await (await post("/pair/start", { name: "unwanted" })).json()) as {
			user_code: string;
			device_code: string;
		};
		const cookie = await ownerSession();
		await decide(start.user_code, "deny", cookie);
		const res = await post("/pair/poll", { device_code: start.device_code });
		expect(res.status).toBe(400);
		expect(await res.json()).toEqual({ error: "access_denied" });
	});

	it("reports expired_token for expired and unknown grants", async () => {
		const start = (await (await post("/pair/start", { name: "stale" })).json()) as {
			user_code: string;
			device_code: string;
		};
		await patchPair(start.device_code, { expiresAt: Date.now() - 1 });
		expect(await (await post("/pair/poll", { device_code: start.device_code })).json()).toEqual({ error: "expired_token" });
		expect(await (await post("/pair/poll", { device_code: "Aa0lI1oO0vVnNnBbCc-DdEeFfGgHhIiJjKkLlMmN" })).json()).toEqual({
			error: "expired_token",
		});
	});

	it("needs the owner session to decide", async () => {
		const start = (await (await post("/pair/start", { name: "no session" })).json()) as { user_code: string };
		expect((await decide(start.user_code, "approve")).status).toBe(401);
	});
});

describe("refresh", () => {
	it("rotates the pair and replays the answer inside the grace window", async () => {
		const { tokens } = await pairDevice();

		const rotated = await post("/pair/refresh", { refresh_token: tokens.refresh_token });
		expect(rotated.status).toBe(200);
		const second = (await rotated.json()) as Tokens;
		expect(second.refresh_token).not.toBe(tokens.refresh_token);
		expect(second.access_token).not.toBe(tokens.access_token);

		// A retried request within the window gets the same pair back.
		const retried = await post("/pair/refresh", { refresh_token: tokens.refresh_token });
		expect(retried.status).toBe(200);
		expect(await retried.json()).toEqual(second);

		// The new token rotates normally.
		const third = await post("/pair/refresh", { refresh_token: second.refresh_token });
		expect(third.status).toBe(200);
	});

	it("revokes the device when a rotated-out token is reused after grace", async () => {
		const { tokens } = await pairDevice();
		const second = (await (await post("/pair/refresh", { refresh_token: tokens.refresh_token })).json()) as Tokens;

		await patchDevice(tokens.device_id, (device) => {
			(device.rtPrev as { until: number }).until = Date.now() - 1;
		});

		const reused = await post("/pair/refresh", { refresh_token: tokens.refresh_token });
		expect(reused.status).toBe(400);
		expect(await reused.json()).toEqual({ error: "invalid_grant" });

		// Theft detection kills the device: the live refresh token and the
		// access token are dead too.
		expect((await post("/pair/refresh", { refresh_token: second.refresh_token })).status).toBe(400);
		expect((await get("/status", bearer(second.access_token))).status).toBe(401);
	});

	it("lets a refresh token unused for 30 days expire", async () => {
		const { tokens } = await pairDevice();
		await patchDevice(tokens.device_id, (device) => {
			device.rtIssuedAt = Date.now() - 31 * 24 * 3_600_000;
		});
		const res = await post("/pair/refresh", { refresh_token: tokens.refresh_token });
		expect(res.status).toBe(400);
		expect(await res.json()).toEqual({ error: "invalid_grant" });

		// The device is expired, not revoked: it still appears on /devices.
		const list = await dobj().devices();
		expect(list.some((d) => d.id === tokens.device_id)).toBe(true);
	});

	it("rejects unknown refresh tokens", async () => {
		expect((await post("/pair/refresh", { refresh_token: "nope" })).status).toBe(400);
		expect((await post("/pair/refresh", {})).status).toBe(400);
	});
});

describe("bearer auth", () => {
	it("answers 401 on bad tokens without waking the container", async () => {
		await runInDurableObject(dobj(), async (instance) => {
			const obj = instance as unknown as { __touched: number; containerFetch: unknown; startAndWaitForPorts: unknown };
			obj.__touched = 0;
			const mark = () => {
				obj.__touched++;
				return Promise.reject(new Error("container was touched"));
			};
			obj.containerFetch = mark;
			obj.startAndWaitForPorts = mark;
		});

		expect((await get("/status")).status).toBe(401);
		expect((await get("/status", bearer("not-a-token"))).status).toBe(401);
		expect((await get("/bogus", bearer("not-a-token"))).status).toBe(401);

		const touched = await runInDurableObject(dobj(), async (instance) => (instance as unknown as { __touched: number }).__touched);
		expect(touched).toBe(0);
	});

	it("reads a stale cached status through the running container's port, not containerFetch", async () => {
		const { tokens } = await pairDevice();
		await runInDurableObject(dobj(), async (instance, state) => {
			const probe = state as unknown as { __portFetches: number };
			probe.__portFetches = 0;
			Object.defineProperty(state, "container", {
				value: {
					running: true,
					getTcpPort: () => ({
						fetch: () => {
							probe.__portFetches++;
							return Promise.resolve(new Response(JSON.stringify({ code: 0, msg: "SUCCESS", data: { ready: true } })));
						},
					}),
				},
				configurable: true,
			});
			const obj = instance as unknown as { __touched: number; containerFetch: unknown };
			obj.__touched = 0;
			obj.containerFetch = () => {
				obj.__touched++;
				return Promise.reject(new Error("containerFetch must not run for a cached read"));
			};
			await state.storage.put("restored", true);
			await state.storage.put("cache:status", { body: JSON.stringify({ code: 0, data: { ready: true }, stale: true }), at: Date.now() - 60_000 });
		});

		const res = await get("/status", bearer(tokens.access_token));
		expect(res.status).toBe(200);
		expect(res.headers.get("x-amdl-wrapper")).toBe("live");
		expect(await res.json()).toEqual({ code: 0, msg: "SUCCESS", data: { ready: true } });

		const counts = await runInDurableObject(dobj(), async (instance, state) => ({
			touched: (instance as unknown as { __touched: number }).__touched,
			portFetches: (state as unknown as { __portFetches: number }).__portFetches,
		}));
		expect(counts).toEqual({ touched: 0, portFetches: 1 });
	});

	it("serves a paired device from the cache and 404s unknown paths", async () => {
		const { tokens } = await pairDevice();
		await runInDurableObject(dobj(), async (_instance, state) => {
			await state.storage.put("cache:status", {
				body: JSON.stringify({ code: 0, msg: "SUCCESS", data: { ready: true } }),
				at: Date.now(),
			});
		});

		const res = await get("/status", bearer(tokens.access_token));
		expect(res.status).toBe(200);
		expect(res.headers.get("x-amdl-wrapper")).toBe("cache");
		expect(await res.json()).toEqual({ code: 0, msg: "SUCCESS", data: { ready: true } });

		expect((await get("/not-proxied", bearer(tokens.access_token))).status).toBe(404);
	});
});

describe("devices", () => {
	it("lists devices and revokes one or all of them", async () => {
		const first = await pairDevice("amdl one");
		const second = await pairDevice("amdl two");
		const cookie = await ownerSession();

		const page = await get("/devices", { cookie });
		const html = await page.text();
		expect(html).toContain("amdl one");
		expect(html).toContain("amdl two");

		const revoked = await postForm("/devices/revoke", { id: first.tokens.device_id }, cookie);
		expect(revoked.status).toBe(303);
		expect((await get("/status", bearer(first.tokens.access_token))).status).toBe(401);
		expect((await post("/pair/refresh", { refresh_token: first.tokens.refresh_token })).status).toBe(400);

		// The survivor is still a live credential; without a container the
		// proxy answers 503 rather than 401.
		const alive = await get("/status", bearer(second.tokens.access_token));
		expect(alive.status).toBe(503);

		const wipe = await postForm("/devices/revoke-all", {}, cookie);
		expect(wipe.status).toBe(303);
		expect((await get("/status", bearer(second.tokens.access_token))).status).toBe(401);
	});

	it("redirects anonymous visitors away from /devices", async () => {
		expect((await get("/devices")).status).toBe(303);
	});
});
