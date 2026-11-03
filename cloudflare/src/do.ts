import { Container } from "@cloudflare/containers";
import { b64url, hashOf, normalizeUserCode, randomToken, seal, unseal, unb64url, userCode } from "./util";

export interface Env {
	WRAPPER: DurableObjectNamespace<WrapperContainer>;
	BACKUP: R2Bucket;
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

export const PAIR_EXPIRES_MS = 600_000;
export const PAIR_INTERVAL_S = 5;
export const ACCESS_TTL_S = 86_400;
export const SESSION_TTL_MS = 3_600_000;
const CHALLENGE_TTL_MS = 120_000;
const REFRESH_GRACE_MS = 60_000;
const REFRESH_IDLE_MS = 30 * 24 * 3_600_000;
// Pair starts per source IP within one window.
const PAIR_RATE_LIMIT = 10;
const PAIR_RATE_WINDOW_MS = 600_000;
// Devices page refreshes last-used at most this often.
const LAST_USED_WRITE_MS = 300_000;

export type Passkey = {
	id: string; // credential id, base64url
	publicKey: string; // raw public key, base64
	counter: number;
	transports?: string[];
	createdAt: number;
};

export type Device = {
	id: string;
	name: string;
	createdAt: number;
	lastUsedAt: number;
	lastIp: string;
	revoked: boolean;
	rtCur: string; // hash of the live refresh token
	rtIssuedAt: number;
	rtPrev: { hash: string; until: number; blob: string } | null;
	rts: string[]; // every refresh-token hash ever issued, for reuse checks
};

type PairState = "pending" | "approved" | "denied";

type Pair = {
	name: string;
	userCodeHash: string;
	state: PairState;
	createdAt: number;
	expiresAt: number;
	lastPollAt: number;
	// Key the approval seals the first tokens with. Only present while the
	// grant is undecided; deleted once the delivery blob exists.
	deliverKey?: string;
	deviceId?: string;
	delivery?: string; // sealed {access_token, refresh_token, device_id}
};

type Challenge = { kind: "reg" | "auth"; expiresAt: number; session?: string };

export type PollResult =
	| { status: "pending" | "slow" | "denied" | "expired" | "unknown" }
	| { status: "approved"; tokens: string };

export type RefreshResult = { ok: true; tokens: string } | { ok: false };

type Cached = { body: string; at: number };
type Renewal = { at: number; ok: string[]; failed: Record<string, string> };

// The one wrapper-manager, its account backup, the answers cached for it,
// and the deployment's pairing state: the owner's passkeys, the paired
// devices' token digests and pending pair grants. Codes and tokens are
// stored only as SHA-256 digests; the first poll's answer rides in a blob
// sealed with a key derived from the device code.
//
// The container sleeps after five idle minutes and loses its disk, so the
// accounts live encrypted in R2 between runs and are restored before the
// first request reaches a freshly started container.
export class WrapperContainer extends Container<Env> {
	constructor(ctx: DurableObjectState<{}>, env: Env) {
		// Runtimes without containers (the Vitest pool) leave ctx.container
		// undefined and Container's constructor refuses to build, which would
		// take every non-container feature of this object down with it. Give
		// it an inert placeholder: running() stays false and any real
		// container operation throws, which fetch() reports as unavailable.
		if (ctx.container === undefined) {
			Object.defineProperty(ctx, "container", { value: { running: false }, configurable: true });
		}
		super(ctx, env);
	}

	defaultPort = MANAGER_PORT;
	requiredPorts = [MANAGER_PORT, ADMIN_PORT];
	sleepAfter = "5m";
	enableInternet = true;

	private restoring: Promise<void> | null = null;
	private backingUp: Promise<void> = Promise.resolve();
	private renewing = false;

	// ---- passkeys ---------------------------------------------------

	async passkeys(): Promise<Passkey[]> {
		return (await this.ctx.storage.get<Passkey[]>("passkeys")) ?? [];
	}

	async savePasskey(key: Passkey): Promise<void> {
		const keys = await this.passkeys();
		if (!keys.some((k) => k.id === key.id)) {
			keys.push(key);
			await this.ctx.storage.put("passkeys", keys);
		}
	}

	async updatePasskeyCounter(id: string, counter: number): Promise<void> {
		const keys = await this.passkeys();
		const key = keys.find((k) => k.id === id);
		if (key && counter > key.counter) {
			key.counter = counter;
			await this.ctx.storage.put("passkeys", keys);
		}
	}

	// ---- challenges ---------------------------------------------------

	async putChallenge(challenge: string, kind: Challenge["kind"], session?: string): Promise<void> {
		const rec: Challenge = { kind, expiresAt: Date.now() + CHALLENGE_TTL_MS };
		if (session) rec.session = session;
		await this.ctx.storage.put(`chal:${challenge}`, rec);
	}

	// consumeChallenge atomically answers the challenge predicate: a stored
	// challenge is spent on first read, so it can never be replayed.
	async consumeChallenge(challenge: string, kind: Challenge["kind"], session?: string): Promise<boolean> {
		const key = `chal:${challenge}`;
		const rec = await this.ctx.storage.get<Challenge>(key);
		if (!rec) return false;
		await this.ctx.storage.delete(key);
		if (rec.kind !== kind || rec.expiresAt < Date.now()) return false;
		if (rec.session && rec.session !== session) return false;
		return true;
	}

	// ---- sessions -----------------------------------------------------

	async makeSession(): Promise<string> {
		const token = randomToken();
		await this.ctx.storage.put(`sess:${await hashOf(token)}`, { expiresAt: Date.now() + SESSION_TTL_MS });
		return token;
	}

	async sessionValid(sessionHash: string): Promise<boolean> {
		const rec = await this.ctx.storage.get<{ expiresAt: number }>(`sess:${sessionHash}`);
		if (!rec) return false;
		if (rec.expiresAt < Date.now()) {
			await this.ctx.storage.delete(`sess:${sessionHash}`);
			return false;
		}
		return true;
	}

	async dropSession(sessionHash: string): Promise<void> {
		await this.ctx.storage.delete(`sess:${sessionHash}`);
	}

	// ---- pairing ------------------------------------------------------

	// pairStart opens a grant. It is rate limited per caller IP so the
	// unauthenticated endpoint cannot fill storage.
	async pairStart(name: string, ipHash: string): Promise<{ userCode: string; deviceCode: string } | { error: "rate_limited" }> {
		const rateKey = `rl:pair:${ipHash}`;
		const now = Date.now();
		const rate = (await this.ctx.storage.get<{ n: number; resetAt: number }>(rateKey)) ?? { n: 0, resetAt: now + PAIR_RATE_WINDOW_MS };
		if (now > rate.resetAt) {
			rate.n = 0;
			rate.resetAt = now + PAIR_RATE_WINDOW_MS;
		}
		if (rate.n >= PAIR_RATE_LIMIT) return { error: "rate_limited" };
		rate.n += 1;
		await this.ctx.storage.put(rateKey, rate);

		const dc = randomToken();
		const uc = userCode();
		const dcHash = await hashOf(dc);
		const deliverKey = await hashOf(`deliver:${dc}`);
		const pair: Pair = {
			name,
			userCodeHash: await hashOf(uc),
			state: "pending",
			createdAt: now,
			expiresAt: now + PAIR_EXPIRES_MS,
			lastPollAt: 0,
			deliverKey,
		};
		await this.ctx.storage.put(`pair:dc:${dcHash}`, pair);
		await this.ctx.storage.put(`pair:uc:${pair.userCodeHash}`, dcHash);
		return { userCode: uc, deviceCode: dc };
	}

	// pairInfo lets the approval page show which device a code belongs to.
	async pairInfo(userCodeInput: string): Promise<{ name: string; state: PairState } | null> {
		const pair = await this.pairByUserCode(userCodeInput);
		if (!pair) return null;
		return { name: pair.name, state: pair.state };
	}

	private async pairByUserCode(userCodeInput: string): Promise<Pair | null> {
		const ucHash = await hashOf(normalizeUserCode(userCodeInput));
		const dcHash = await this.ctx.storage.get<string>(`pair:uc:${ucHash}`);
		if (!dcHash) return null;
		const pair = await this.ctx.storage.get<Pair>(`pair:dc:${dcHash}`);
		if (!pair) return null;
		if (pair.state === "pending" && pair.expiresAt < Date.now()) {
			await this.dropPair(dcHash, pair);
			return null;
		}
		return pair;
	}

	private async dropPair(dcHash: string, pair: Pair): Promise<void> {
		await this.ctx.storage.delete([`pair:dc:${dcHash}`, `pair:uc:${pair.userCodeHash}`]);
	}

	// pairDecide approves or denies a grant. Approval creates the device and
	// its first token pair; the tokens go back to the device sealed under a
	// key derived from the device code, so nothing usable sits in storage.
	async pairDecide(userCodeInput: string, approve: boolean): Promise<{ ok: boolean; state?: PairState }> {
		const ucHash = await hashOf(normalizeUserCode(userCodeInput));
		const dcHash = await this.ctx.storage.get<string>(`pair:uc:${ucHash}`);
		if (!dcHash) return { ok: false };
		const pairKey = `pair:dc:${dcHash}`;
		const pair = await this.ctx.storage.get<Pair>(pairKey);
		if (!pair || pair.state !== "pending") return { ok: false, state: pair?.state };
		if (pair.expiresAt < Date.now()) {
			await this.dropPair(dcHash, pair);
			return { ok: false };
		}
		if (!approve) {
			pair.state = "denied";
			delete pair.deliverKey;
			await this.ctx.storage.put(pairKey, pair);
			return { ok: true, state: "denied" };
		}
		const device = await this.newDevice(pair.name);
		const tokens = await this.issueTokens(device, unb64url(pair.deliverKey!));
		await this.ctx.storage.put(`dev:${device.id}`, device);
		pair.state = "approved";
		pair.deviceId = device.id;
		pair.delivery = tokens.sealed;
		delete pair.deliverKey;
		await this.ctx.storage.put(pairKey, pair);
		return { ok: true, state: "approved" };
	}

	// pairPoll answers the device's token endpoint.
	async pairPoll(deviceCode: string, ip: string): Promise<PollResult> {
		const dcHash = await hashOf(deviceCode);
		const pairKey = `pair:dc:${dcHash}`;
		const pair = await this.ctx.storage.get<Pair>(pairKey);
		if (!pair) return { status: "unknown" };
		const now = Date.now();
		if (pair.expiresAt < now) {
			await this.dropPair(dcHash, pair);
			return { status: "expired" };
		}
		if (now - pair.lastPollAt < PAIR_INTERVAL_S * 1000) {
			return { status: "slow" };
		}
		pair.lastPollAt = now;
		await this.ctx.storage.put(pairKey, pair);
		if (pair.state === "pending") return { status: "pending" };
		if (pair.state === "denied") {
			await this.dropPair(dcHash, pair);
			return { status: "denied" };
		}
		const tokens = await unseal(unb64url(await hashOf(`deliver:${deviceCode}`)), pair.delivery!);
		if (!tokens) return { status: "expired" };
		const device = await this.device(pair.deviceId!);
		if (!device || device.revoked) return { status: "denied" };
		await this.touchDevice(device, ip);
		return { status: "approved", tokens };
	}

	// ---- refresh ------------------------------------------------------

	// refresh rotates a device's tokens. The previous refresh token keeps
	// working for a grace window so a retried request still gets an answer;
	// anything rotated out of the window marks the device compromised.
	async refresh(refreshToken: string, ip: string): Promise<RefreshResult> {
		const h = await hashOf(`rt:${refreshToken}`);
		const index = await this.ctx.storage.get<{ d: string; s: "cur" | "prev" | "dead" }>(`rt:${h}`);
		if (!index) return { ok: false };
		const device = await this.device(index.d);
		if (!device || device.revoked) return { ok: false };
		const now = Date.now();

		if (index.s === "dead") {
			await this.revokeDevice(device);
			return { ok: false };
		}
		if (index.s === "prev") {
			if (device.rtPrev && device.rtPrev.hash === h && now < device.rtPrev.until) {
				const tokens = await unseal(unb64url(await hashOf(`deliver:${refreshToken}`)), device.rtPrev.blob);
				if (tokens) {
					await this.touchDevice(device, ip);
					return { ok: true, tokens };
				}
			}
			// Reused after the grace window: theft detection.
			await this.ctx.storage.put(`rt:${h}`, { d: device.id, s: "dead" });
			await this.revokeDevice(device);
			return { ok: false };
		}
		if (now - device.rtIssuedAt > REFRESH_IDLE_MS) return { ok: false };

		// The old "prev" slot, if any, is now fully rotated out.
		if (device.rtPrev) await this.ctx.storage.put(`rt:${device.rtPrev.hash}`, { d: device.id, s: "dead" });
		const issued = await this.issueTokens(device, unb64url(await hashOf(`deliver:${refreshToken}`)), now);
		device.rtPrev = { hash: h, until: now + REFRESH_GRACE_MS, blob: issued.sealed };
		await this.ctx.storage.put(`rt:${device.rtPrev.hash}`, { d: device.id, s: "prev" });
		await this.ctx.storage.put(`dev:${device.id}`, device);
		await this.touchDevice(device, ip);
		return { ok: true, tokens: issued.plain };
	}

	// issueTokens mints a fresh access/refresh pair for the device, records
	// both digests and seals the answer so only the caller holding the
	// delivery key can read it. The plaintext goes to the requester now, the
	// sealed copy stays for a grace-window replay.
	private async issueTokens(device: Device, deliverKey: Uint8Array, now = Date.now()): Promise<{ sealed: string; plain: string }> {
		const accessToken = randomToken();
		const refreshToken = randomToken();
		const rtHash = await hashOf(`rt:${refreshToken}`);
		await this.ctx.storage.put(`rt:${rtHash}`, { d: device.id, s: "cur" });
		if (device.rtCur) await this.ctx.storage.put(`rt:${device.rtCur}`, { d: device.id, s: "prev" });
		device.rtCur = rtHash;
		device.rtIssuedAt = now;
		device.rts.push(rtHash);
		await this.ctx.storage.put(`at:${await hashOf(accessToken)}`, { d: device.id, exp: now + ACCESS_TTL_S * 1000 });
		const plain = JSON.stringify({
			access_token: accessToken,
			refresh_token: refreshToken,
			token_type: "Bearer",
			expires_in: ACCESS_TTL_S,
			device_id: device.id,
		});
		return { sealed: await seal(deliverKey, plain), plain };
	}

	private async newDevice(name: string): Promise<Device> {
		const now = Date.now();
		return {
			id: b64url(crypto.getRandomValues(new Uint8Array(16))),
			name,
			createdAt: now,
			lastUsedAt: now,
			lastIp: "",
			revoked: false,
			rtCur: "",
			rtIssuedAt: now,
			rtPrev: null,
			rts: [],
		};
	}

	private async device(id: string): Promise<Device | null> {
		return (await this.ctx.storage.get<Device>(`dev:${id}`)) ?? null;
	}

	private async touchDevice(device: Device, ip: string): Promise<void> {
		const now = Date.now();
		if (now - device.lastUsedAt < LAST_USED_WRITE_MS && device.lastIp === ip) return;
		device.lastUsedAt = now;
		device.lastIp = ip;
		await this.ctx.storage.put(`dev:${device.id}`, device);
	}

	// ---- bearer check ---------------------------------------------------

	// accessLookup answers whether a bearer digest belongs to a live device.
	// It is the only check a proxied request needs: a storage read never
	// starts the container, so a wrong token still gets a 401 while it sleeps.
	async accessLookup(accessHash: string, ip: string): Promise<string | null> {
		const index = await this.ctx.storage.get<{ d: string; exp: number }>(`at:${accessHash}`);
		if (!index || index.exp < Date.now()) return null;
		const device = await this.device(index.d);
		if (!device || device.revoked) return null;
		await this.touchDevice(device, ip);
		return device.id;
	}

	// ---- devices ------------------------------------------------------

	async devices(): Promise<Omit<Device, "rtCur" | "rtIssuedAt" | "rtPrev" | "rts" | "revoked">[]> {
		const list = await this.ctx.storage.list<Device>({ prefix: "dev:" });
		return [...list.values()]
			.filter((d) => !d.revoked)
			.sort((a, b) => a.createdAt - b.createdAt)
			.map(({ id, name, createdAt, lastUsedAt, lastIp }) => ({ id, name, createdAt, lastUsedAt, lastIp }));
	}

	// revokeDevice invalidates every token the device holds: live and
	// previous refresh digests, every rotated-out digest still indexed and
	// its access digests. Pending deliveries die with the pair record.
	async revokeDevice(device: Device): Promise<void> {
		device.revoked = true;
		const keys = [`dev:${device.id}`];
		for (const h of device.rts) keys.push(`rt:${h}`);
		if (device.rtPrev) keys.push(`rt:${device.rtPrev.hash}`);
		const access = await this.ctx.storage.list<{ d: string }>({ prefix: "at:" });
		for (const [key, index] of access) if (index.d === device.id) keys.push(key);
		await this.ctx.storage.delete(keys);
		await this.ctx.storage.put(`dev:${device.id}`, device);
	}

	async revoke(id: string): Promise<boolean> {
		const device = await this.device(id);
		if (!device) return false;
		await this.revokeDevice(device);
		return true;
	}

	async revokeAll(): Promise<void> {
		const list = await this.ctx.storage.list<Device>({ prefix: "dev:" });
		for (const device of list.values()) {
			if (!device.revoked) await this.revokeDevice(device);
		}
	}

	// ---- container lifecycle -------------------------------------------

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

	// ---- R2 backup ------------------------------------------------------

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

	// ---- cache ----------------------------------------------------------

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

	// ---- requests -------------------------------------------------------

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
			return new Response(JSON.stringify({ code: 503, msg: `wrapper unavailable: ${err instanceof Error ? err.message : err}` }), {
				status: 503,
				headers: { "content-type": "application/json", "cache-control": "no-store" },
			});
		}
	}

	// ---- daily renewal --------------------------------------------------

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
