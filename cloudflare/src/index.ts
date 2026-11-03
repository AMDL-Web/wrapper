import { Container } from "@cloudflare/containers";
import { setupPage, tokenPage, homePage } from "./pages";

interface Env {
	WRAPPER: DurableObjectNamespace<WrapperContainer>;
	BACKUP: R2Bucket;
	// Optional: a token set as a Worker secret replaces the one made by the
	// setup page, for example to rotate it.
	WRAPPER_TOKEN?: string;
}

const MANAGER_PORT = 8080;
const ADMIN_PORT = 8081;
// While the container runs, /status and /token answers are reused this long.
// While it sleeps, the last answers are served without waking it.
const STATUS_TTL_MS = 30_000;
const TOKEN_TTL_MS = 60_000;
// wrapper-lite's --token-refresh-interval: a renewed token is younger.
const REFRESH_INTERVAL_S = 1800;
const RENEW_TIMEOUT_MS = 4 * 60_000;
const BACKUP_KEY = "accounts.tar.gz.enc";
const BACKUP_MAGIC = new TextEncoder().encode("AMW1");
const BACKUP_AAD = new TextEncoder().encode("amdl-wrapper accounts v1");
const PROXIED = new Set(["/status", "/token", "/m3u8", "/key", "/lyrics", "/webplayback", "/license", "/login", "/logout"]);

const json = (status: number, body: unknown, headers: Record<string, string> = {}) =>
	new Response(JSON.stringify(body), {
		status,
		headers: { "content-type": "application/json", "cache-control": "no-store", ...headers },
	});
const failure = (status: number, msg: string) => json(status, { code: status, msg });

async function sha256(text: string): Promise<ArrayBuffer> {
	return crypto.subtle.digest("SHA-256", new TextEncoder().encode(text));
}

function hex(bytes: Uint8Array): string {
	return [...bytes].map((b) => b.toString(16).padStart(2, "0")).join("");
}

type Cached = { body: string; at: number };
type Renewal = { at: number; ok: string[]; failed: Record<string, string> };

// The one wrapper-manager, its account backup and the answers cached for it.
// The container sleeps after five idle minutes and loses its disk, so the
// accounts live encrypted in R2 between runs and are restored before the
// first request reaches a freshly started container.
export class WrapperContainer extends Container<Env> {
	defaultPort = MANAGER_PORT;
	requiredPorts = [MANAGER_PORT, ADMIN_PORT];
	sleepAfter = "5m";
	enableInternet = true;

	private restoring: Promise<void> | null = null;
	private backingUp: Promise<void> = Promise.resolve();
	private renewing = false;

	// ---- token -------------------------------------------------------

	async tokenHash(): Promise<ArrayBuffer | null> {
		if (this.env.WRAPPER_TOKEN) return sha256(this.env.WRAPPER_TOKEN);
		return (await this.ctx.storage.get<ArrayBuffer>("tokenHash")) ?? null;
	}

	// claimToken creates the bearer token on the first call and returns it,
	// keeping only its digest; later calls return null.
	async claimToken(): Promise<string | null> {
		if (this.env.WRAPPER_TOKEN || (await this.ctx.storage.get("tokenHash"))) return null;
		const token = hex(crypto.getRandomValues(new Uint8Array(32)));
		await this.ctx.storage.put("tokenHash", await sha256(token));
		return token;
	}

	// ---- container lifecycle ----------------------------------------

	private running(): boolean {
		return this.ctx.container?.running ?? false;
	}

	// ensureRestored starts the container when it is down and pushes the R2
	// backup into it before anything else reaches the manager.
	private async ensureRestored(): Promise<void> {
		if (this.running() && (await this.ctx.storage.get<boolean>("restored"))) return;
		if (!this.restoring) {
			this.restoring = this.restore().finally(() => {
				this.restoring = null;
			});
		}
		await this.restoring;
	}

	private async restore(): Promise<void> {
		await this.ctx.storage.put("restored", false);
		await this.startAndWaitForPorts({
			ports: [MANAGER_PORT, ADMIN_PORT],
			cancellationOptions: { portReadyTimeoutMS: 60_000 },
		});
		const object = await this.env.BACKUP.get(BACKUP_KEY);
		if (object) {
			const snapshot = await this.decrypt(await object.arrayBuffer());
			const res = await this.containerFetch(
				new Request("http://manager/restore", { method: "POST", body: snapshot }),
				ADMIN_PORT,
			);
			const reply = await res.json<{ code: number; msg: string; data?: { restored: number; ready: number } }>();
			if (reply.code !== 0) throw new Error(`restore failed: ${reply.msg}`);
			console.log(`restored ${reply.data?.restored} accounts, ${reply.data?.ready} up`);
		}
		await this.ctx.storage.put("restored", true);
	}

	override onStop(): void | Promise<void> {
		return this.ctx.storage.put("restored", false);
	}

	override async onActivityExpired(): Promise<void> {
		try {
			await this.backup();
		} catch (err) {
			console.error(`backup before sleep failed: ${err}`);
		}
		await this.stop();
	}

	// ---- R2 backup ---------------------------------------------------

	private async backupKey(): Promise<CryptoKey> {
		let raw = await this.ctx.storage.get<ArrayBuffer>("backupKey");
		if (!raw) {
			raw = crypto.getRandomValues(new Uint8Array(32)).buffer as ArrayBuffer;
			await this.ctx.storage.put("backupKey", raw);
		}
		return crypto.subtle.importKey("raw", raw, "AES-GCM", false, ["encrypt", "decrypt"]);
	}

	private async encrypt(plain: ArrayBuffer): Promise<Uint8Array> {
		const iv = crypto.getRandomValues(new Uint8Array(12));
		const sealed = await crypto.subtle.encrypt(
			{ name: "AES-GCM", iv, additionalData: BACKUP_AAD },
			await this.backupKey(),
			plain,
		);
		const out = new Uint8Array(BACKUP_MAGIC.length + iv.length + sealed.byteLength);
		out.set(BACKUP_MAGIC, 0);
		out.set(iv, BACKUP_MAGIC.length);
		out.set(new Uint8Array(sealed), BACKUP_MAGIC.length + iv.length);
		return out;
	}

	private async decrypt(blob: ArrayBuffer): Promise<ArrayBuffer> {
		const bytes = new Uint8Array(blob);
		const magic = bytes.subarray(0, BACKUP_MAGIC.length);
		if (magic.some((b, i) => b !== BACKUP_MAGIC[i])) throw new Error("backup has an unknown format");
		const iv = bytes.subarray(BACKUP_MAGIC.length, BACKUP_MAGIC.length + 12);
		return crypto.subtle.decrypt(
			{ name: "AES-GCM", iv, additionalData: BACKUP_AAD },
			await this.backupKey(),
			bytes.subarray(BACKUP_MAGIC.length + 12),
		);
	}

	// backup writes the manager's accounts to R2 as one encrypted unit. It
	// runs only after this container's restore succeeded, so a container that
	// came up without its accounts never overwrites the good copy.
	async backup(): Promise<void> {
		const run = this.backingUp.then(async () => {
			if (!this.running() || !(await this.ctx.storage.get<boolean>("restored"))) return;
			const res = await this.containerFetch(new Request("http://manager/snapshot"), ADMIN_PORT);
			if (!res.ok) throw new Error(`snapshot answered ${res.status}`);
			const snapshot = await res.arrayBuffer();
			await this.env.BACKUP.put(BACKUP_KEY, await this.encrypt(snapshot));
			await this.ctx.storage.put("backupAt", Date.now());
			console.log(`backed up ${snapshot.byteLength} bytes`);
		});
		this.backingUp = run.catch(() => {});
		return run;
	}

	// ---- cache -------------------------------------------------------

	private async forget(): Promise<void> {
		const keys = await this.ctx.storage.list({ prefix: "cache:" });
		await this.ctx.storage.delete([...keys.keys()]);
	}

	private async manager(path: string, init?: RequestInit): Promise<Response> {
		await this.ensureRestored();
		return this.containerFetch(new Request(`http://manager${path}`, init), MANAGER_PORT);
	}

	// peek asks the running manager without renewing the container's activity
	// timeout, so polling /status or /token never keeps it awake.
	private async peek(path: string): Promise<Response> {
		if (!this.ctx.container || !(await this.ctx.storage.get<boolean>("restored"))) return this.manager(path);
		return this.ctx.container.getTcpPort(MANAGER_PORT).fetch(new Request(`http://manager${path}`));
	}

	// cachedAnswer answers from the manager while the container runs and the
	// cached copy is older than ttl, and from the cache otherwise, waking the
	// container only when nothing is cached. keep decides what may be cached.
	private async cachedAnswer(
		key: string,
		path: string,
		ttl: number,
		keep: (status: number, body: string) => boolean,
		stale: (body: string) => string = (b) => b,
	): Promise<Response> {
		const cached = await this.ctx.storage.get<Cached>(key);
		const fresh = cached && Date.now() - cached.at < ttl;
		if (cached && (fresh || !this.running())) {
			return new Response(fresh ? cached.body : stale(cached.body), {
				headers: { "content-type": "application/json", "cache-control": "no-store", "x-amdl-wrapper": "cache" },
			});
		}
		const res = this.running() ? await this.peek(path) : await this.manager(path);
		const body = await res.text();
		if (keep(res.status, body)) await this.ctx.storage.put(key, { body, at: Date.now() } satisfies Cached);
		return new Response(body, {
			status: res.status,
			headers: { "content-type": "application/json", "cache-control": "no-store", "x-amdl-wrapper": "live" },
		});
	}

	// ---- requests ----------------------------------------------------

	override async fetch(request: Request): Promise<Response> {
		const url = new URL(request.url);
		try {
			switch (url.pathname) {
				case "/status":
					return await this.cachedAnswer("cache:status", "/status", STATUS_TTL_MS, (status, body) => {
						const reply = JSON.parse(body);
						return status === 200 && reply.code === 0 && reply.data?.ready === true;
					});
				case "/token": {
					const q = new URLSearchParams();
					for (const name of ["instance", "region"]) {
						const v = url.searchParams.get(name);
						if (v) q.set(name, v.toLowerCase());
					}
					const path = q.size ? `/token?${q}` : "/token";
					return await this.cachedAnswer(
						`cache:token:${q}`,
						path,
						TOKEN_TTL_MS,
						(status, body) => status === 200 && JSON.parse(body).code === 0,
						// Served while asleep: tell amdl to ask again within the
						// hour (it clamps to that), not at a refresh long past.
						(body) => {
							const reply = JSON.parse(body);
							if (reply.data) reply.data.next_refresh_at = Math.floor(Date.now() / 1000) + 3600;
							return JSON.stringify(reply);
						},
					);
				}
				case "/login":
				case "/logout": {
					const res = await this.manager(url.pathname + url.search, {
						method: request.method,
						headers: { "content-type": "application/json" },
						body: request.method === "POST" ? await request.arrayBuffer() : undefined,
					});
					const body = await res.text();
					let code: number | undefined;
					try {
						code = JSON.parse(body).code;
					} catch {}
					if (code === 0) {
						await this.forget();
						this.ctx.waitUntil(this.backup().catch((err) => console.error(`backup after ${url.pathname} failed: ${err}`)));
					}
					return new Response(body, { status: res.status, headers: { "content-type": "application/json", "cache-control": "no-store" } });
				}
				default: {
					await this.ensureRestored();
					return await this.containerFetch(request, MANAGER_PORT);
				}
			}
		} catch (err) {
			console.error(`${url.pathname}: ${err}`);
			return failure(503, `wrapper unavailable: ${err instanceof Error ? err.message : err}`);
		}
	}

	// ---- daily renewal -----------------------------------------------

	// renew wakes the container, waits until every account's music token is
	// younger than the refresh interval (wrapper-lite refreshes a stale token
	// a few seconds after it starts), caches the tokens and backs them up.
	async renew(): Promise<Renewal> {
		if (this.renewing) return { at: Date.now(), ok: [], failed: { "*": "a renewal is already running" } };
		this.renewing = true;
		try {
			const status = await (await this.manager("/status")).json<{ data?: { instances?: { id: string }[] } }>();
			const ids = (status.data?.instances ?? []).map((i) => i.id);
			const result: Renewal = { at: Date.now(), ok: [], failed: {} };
			const deadline = Date.now() + RENEW_TIMEOUT_MS;
			for (const id of ids) {
				let last = "";
				for (;;) {
					const res = await this.manager(`/token?instance=${id}`);
					const body = await res.text();
					last = `HTTP ${res.status}`;
					if (res.status === 200) {
						const reply = JSON.parse(body);
						const age = Date.now() / 1000 - (reply.data?.refreshed_at ?? 0);
						last = `token is ${Math.round(age)} s old`;
						if (reply.code === 0 && age < REFRESH_INTERVAL_S) {
							await this.ctx.storage.put(`cache:token:instance=${id}`, { body, at: Date.now() } satisfies Cached);
							result.ok.push(id);
							break;
						}
					}
					if (Date.now() > deadline) {
						result.failed[id] = last;
						break;
					}
					await new Promise((r) => setTimeout(r, 5000));
				}
			}
			await this.ctx.storage.delete("cache:status");
			await this.backup();
			await this.ctx.storage.put("renewal", result);
			console.log(`renewal: ${result.ok.length} renewed, failed ${JSON.stringify(result.failed)}`);
			return result;
		} finally {
			this.renewing = false;
		}
	}
}

function stub(env: Env) {
	return env.WRAPPER.get(env.WRAPPER.idFromName("main"));
}

let knownHash: ArrayBuffer | null = null;

async function authorized(request: Request, env: Env): Promise<boolean> {
	const header = request.headers.get("authorization");
	if (!header?.startsWith("Bearer ")) return false;
	if (env.WRAPPER_TOKEN) knownHash = await sha256(env.WRAPPER_TOKEN);
	if (!knownHash) knownHash = await stub(env).tokenHash();
	if (!knownHash) return false;
	const given = await sha256(header.slice("Bearer ".length));
	return crypto.subtle.timingSafeEqual(given, knownHash);
}

export default {
	async fetch(request: Request, env: Env): Promise<Response> {
		const url = new URL(request.url);
		if (url.protocol !== "https:" && url.hostname !== "localhost") return failure(403, "https only");

		if (url.pathname === "/" || url.pathname === "/setup") {
			const configured = !!env.WRAPPER_TOKEN || !!(await stub(env).tokenHash());
			if (request.method === "GET") {
				return new Response(configured ? homePage() : setupPage(), {
					headers: { "content-type": "text/html; charset=utf-8", "cache-control": "no-store" },
				});
			}
			if (request.method === "POST" && url.pathname === "/setup") {
				const token = await stub(env).claimToken();
				const wantsJSON = (request.headers.get("accept") ?? "").includes("application/json");
				if (!token) return wantsJSON ? failure(409, "already set up") : new Response(homePage(), { status: 409, headers: { "content-type": "text/html; charset=utf-8" } });
				if (wantsJSON) return json(200, { code: 0, msg: "SUCCESS", data: { token } });
				return new Response(tokenPage(token, url.origin), {
					headers: { "content-type": "text/html; charset=utf-8", "cache-control": "no-store" },
				});
			}
			return failure(405, "method not allowed");
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
