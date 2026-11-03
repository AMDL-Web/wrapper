import { env } from "cloudflare:workers";
import { reset, runInDurableObject, SELF } from "cloudflare:test";
import { afterEach, describe, expect, it } from "vitest";
import worker, { type Env } from "../src/index";

afterEach(async () => {
	await reset();
});

const ORIGIN = "https://worker.test";
// Matches vars.API_KEY in vitest.config.ts.
const API_KEY = "test-key-0123456789abcdef0123456789abcd";

const get = (path: string, headers: Record<string, string> = {}) =>
	SELF.fetch(new Request(`${ORIGIN}${path}`, { headers, redirect: "manual" }));
const bearer = (token: string) => ({ authorization: `Bearer ${token}` });

const typedEnv = env as unknown as Env;
const dobj = () => typedEnv.WRAPPER.get(typedEnv.WRAPPER.idFromName("main"));

describe("API_KEY gate", () => {
	it("accepts the configured key", async () => {
		// Auth passes and the proxy runs; with no container in the test pool
		// the manager answer is 503, not 401.
		expect((await get("/status", bearer(API_KEY))).status).toBe(503);
	});

	it("answers 401 for wrong and missing keys without touching the container", async () => {
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
		expect((await get("/status", bearer("wrong-key-but-long-enough-0123456789"))).status).toBe(401);
		expect((await get("/status", { authorization: "Basic dGVzdA==" })).status).toBe(401);

		const touched = await runInDurableObject(dobj(), async (instance) => (instance as unknown as { __touched: number }).__touched);
		expect(touched).toBe(0);
	});

	it("answers 503 for every request while API_KEY is unset or too short", async () => {
		for (const key of [undefined, "", "short-key"]) {
			const res = await worker.fetch(new Request(`${ORIGIN}/status`, { headers: bearer("whatever") }), { API_KEY: key } as Env);
			expect(res.status).toBe(503);
			expect(await res.json()).toEqual({ code: 503, msg: "API_KEY is not set" });
		}
	});

	it("404s unknown paths for a good key", async () => {
		expect((await get("/bogus", bearer(API_KEY))).status).toBe(404);
	});
});

describe("caches", () => {
	it("serves a fresh cached /status while the container is down", async () => {
		await runInDurableObject(dobj(), async (_instance, state) => {
			await state.storage.put("cache:status", {
				body: JSON.stringify({ code: 0, msg: "SUCCESS", data: { ready: true } }),
				at: Date.now(),
			});
		});
		const res = await get("/status", bearer(API_KEY));
		expect(res.status).toBe(200);
		expect(res.headers.get("x-amdl-wrapper")).toBe("cache");
		expect(await res.json()).toEqual({ code: 0, msg: "SUCCESS", data: { ready: true } });
	});

	it("serves the last answer, token refreshed, when the container sleeps", async () => {
		const cached = { code: 0, msg: "SUCCESS", data: { token: "t", next_refresh_at: 1, refreshed_at: 1 } };
		await runInDurableObject(dobj(), async (_instance, state) => {
			await state.storage.put("cache:token:instance=abc", { body: JSON.stringify(cached), at: Date.now() - 120_000 });
		});
		const res = await get("/token?instance=ABC", bearer(API_KEY));
		expect(res.status).toBe(200);
		const body = (await res.json()) as typeof cached;
		expect(body.data.token).toBe("t");
		// The stale rewrite points the next refresh inside the hour.
		expect(body.data.next_refresh_at).toBeGreaterThan(Date.now() / 1000);
	});

	it("reads a stale cached status through the running container's port, not containerFetch", async () => {
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

		const res = await get("/status", bearer(API_KEY));
		expect(res.status).toBe(200);
		expect(res.headers.get("x-amdl-wrapper")).toBe("live");
		expect(await res.json()).toEqual({ code: 0, msg: "SUCCESS", data: { ready: true } });

		const counts = await runInDurableObject(dobj(), async (instance, state) => ({
			touched: (instance as unknown as { __touched: number }).__touched,
			portFetches: (state as unknown as { __portFetches: number }).__portFetches,
		}));
		expect(counts).toEqual({ touched: 0, portFetches: 1 });
	});
});
