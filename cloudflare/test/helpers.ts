// A fake WebAuthn authenticator for tests: a generated ES256 key, a "none"
// attestation statement and real ECDSA signatures over the same values a
// platform authenticator would produce.

export const ORIGIN = "https://wrapper.test";

export function b64url(bytes: Uint8Array | ArrayBuffer): string {
	const b = bytes instanceof Uint8Array ? bytes : new Uint8Array(bytes);
	let s = "";
	for (let i = 0; i < b.length; i++) s += String.fromCharCode(b[i]);
	return btoa(s).replaceAll("+", "-").replaceAll("/", "_").replaceAll("=", "");
}

export function unb64url(text: string): Uint8Array {
	const s = atob(text.replaceAll("-", "+").replaceAll("_", "/"));
	const b = new Uint8Array(s.length);
	for (let i = 0; i < s.length; i++) b[i] = s.charCodeAt(i);
	return b;
}

export async function sha256(text: string | Uint8Array): Promise<Uint8Array> {
	const bytes = typeof text === "string" ? new TextEncoder().encode(text) : text;
	return new Uint8Array(await crypto.subtle.digest("SHA-256", bytes as Uint8Array<ArrayBuffer>));
}

// ---- minimal CBOR -----------------------------------------------------

type Cbor = number | string | Uint8Array | Cbor[] | Map<number | string, Cbor>;

function head(major: number, len: number): number[] {
	if (len < 24) return [(major << 5) | len];
	if (len < 256) return [(major << 5) | 24, len];
	return [(major << 5) | 25, len >> 8, len & 0xff];
}

function cborInto(value: Cbor, out: number[]): void {
	if (typeof value === "number") {
		if (value >= 0) out.push(...head(0, value));
		else out.push(...head(1, -1 - value));
	} else if (typeof value === "string") {
		const bytes = new TextEncoder().encode(value);
		out.push(...head(3, bytes.length), ...bytes);
	} else if (value instanceof Uint8Array) {
		out.push(...head(2, value.length), ...value);
	} else if (Array.isArray(value)) {
		out.push(...head(4, value.length));
		for (const item of value) cborInto(item, out);
	} else {
		out.push(...head(5, value.size));
		for (const [key, item] of value) {
			cborInto(key, out);
			cborInto(item, out);
		}
	}
}

function cbor(value: Cbor): Uint8Array {
	const out: number[] = [];
	cborInto(value, out);
	return new Uint8Array(out);
}

// ---- the fake authenticator -------------------------------------------

const concat = (...parts: Uint8Array[]) => {
	const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
	let at = 0;
	for (const part of parts) {
		out.set(part, at);
		at += part.length;
	}
	return out;
};

function clientData(type: string, challenge: string, origin: string): Uint8Array {
	return new TextEncoder().encode(JSON.stringify({ type, challenge, origin, crossOrigin: false }));
}

// workerd's WebCrypto returns raw r||s signatures; authenticators send DER.
function derInt(bytes: Uint8Array): number[] {
	let i = 0;
	while (i < bytes.length - 1 && bytes[i] === 0) i++;
	const v = [...bytes.slice(i)];
	if (v[0] & 0x80) v.unshift(0);
	return [0x02, v.length, ...v];
}

function toDer(signature: Uint8Array): Uint8Array {
	const r = derInt(signature.subarray(0, signature.length / 2));
	const s = derInt(signature.subarray(signature.length / 2));
	return new Uint8Array([0x30, r.length + s.length, ...r, ...s]);
}

export class FakeAuthenticator {
	privateKey!: CryptoKey;
	publicKey!: CryptoKey;
	credentialId = crypto.getRandomValues(new Uint8Array(16));
	signCount = 0;

	static async create(): Promise<FakeAuthenticator> {
		const auth = new FakeAuthenticator();
		const pair = (await crypto.subtle.generateKey({ name: "ECDSA", namedCurve: "P-256" }, true, ["sign", "verify"])) as CryptoKeyPair;
		auth.privateKey = pair.privateKey;
		auth.publicKey = pair.publicKey;
		return auth;
	}

	get credentialIdB64(): string {
		return b64url(this.credentialId);
	}

	private async coseKey(): Promise<Uint8Array> {
		const raw = new Uint8Array((await crypto.subtle.exportKey("raw", this.publicKey)) as ArrayBuffer);
		return cbor(
			new Map<number | string, Cbor>([
				[1, 2], // kty: EC2
				[3, -7], // alg: ES256
				[-1, 1], // crv: P-256
				[-2, raw.subarray(1, 33)],
				[-3, raw.subarray(33, 65)],
			]),
		);
	}

	private authData(rpId: string, flags: number, extra: Uint8Array = new Uint8Array()): Promise<Uint8Array> {
		return sha256(rpId).then((hash) => {
			const count = new Uint8Array(4);
			new DataView(count.buffer).setUint32(0, this.signCount);
			return concat(hash, new Uint8Array([flags]), count, extra);
		});
	}

	// registrationResponse builds a "none" attestation: fmt none, an empty
	// statement and authData carrying the credential's COSE key.
	async registrationResponse(challenge: string, origin = ORIGIN, rpId = "wrapper.test") {
		const aaguid = new Uint8Array(16);
		const idLen = new Uint8Array(2);
		new DataView(idLen.buffer).setUint16(0, this.credentialId.length);
		const attested = concat(aaguid, idLen, this.credentialId, await this.coseKey());
		const authData = await this.authData(rpId, 0x01 | 0x04 | 0x40, attested); // UP | UV | AT
		const attestationObject = cbor(
			new Map<string, Cbor>([
				["fmt", "none"],
				["attStmt", new Map()],
				["authData", authData],
			]),
		);
		return {
			id: this.credentialIdB64,
			rawId: this.credentialIdB64,
			type: "public-key",
			response: {
				clientDataJSON: b64url(clientData("webauthn.create", challenge, origin)),
				attestationObject: b64url(attestationObject),
				transports: ["internal"],
			},
			clientExtensionResults: {},
		};
	}

	// authenticationResponse signs authData || sha256(clientDataJSON) like a
	// real authenticator.
	async authenticationResponse(challenge: string, origin = ORIGIN, rpId = "wrapper.test") {
		this.signCount += 1;
		const authData = await this.authData(rpId, 0x01 | 0x04); // UP | UV
		const client = clientData("webauthn.get", challenge, origin);
		const signed = concat(authData, await sha256(client));
		const signature = toDer(
			new Uint8Array(await crypto.subtle.sign({ name: "ECDSA", hash: "SHA-256" }, this.privateKey, signed)),
		);
		return {
			id: this.credentialIdB64,
			rawId: this.credentialIdB64,
			type: "public-key",
			response: {
				clientDataJSON: b64url(client),
				authenticatorData: b64url(authData),
				signature: b64url(signature),
				userHandle: null,
			},
			clientExtensionResults: {},
		};
	}
}
