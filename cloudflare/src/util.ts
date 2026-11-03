// Small primitives shared by the Worker's routes and the Durable Object.
// Tokens and codes are never stored, only their SHA-256 digests.

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

export function b64(bytes: Uint8Array): string {
	let s = "";
	for (let i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i]);
	return btoa(s);
}

export function unb64(text: string): Uint8Array {
	const s = atob(text);
	const b = new Uint8Array(s.length);
	for (let i = 0; i < s.length; i++) b[i] = s.charCodeAt(i);
	return b;
}

export async function sha256(data: string | Uint8Array): Promise<ArrayBuffer> {
	const bytes = typeof data === "string" ? new TextEncoder().encode(data) : data;
	return crypto.subtle.digest("SHA-256", bytes as Uint8Array<ArrayBuffer>);
}

// Hashes are stored and indexed as base64url strings.
export async function hashOf(secret: string): Promise<string> {
	return b64url(await sha256(secret));
}

export function randomToken(): string {
	return b64url(crypto.getRandomValues(new Uint8Array(32)));
}

// Unambiguous alphabet: no 0/O, 1/I/L.
const CODE_ALPHABET = "ABCDEFGHJKMNPQRSTUVWXYZ23456789";

export function userCode(): string {
	const raw = crypto.getRandomValues(new Uint8Array(8));
	let code = "";
	for (const b of raw) code += CODE_ALPHABET[b % CODE_ALPHABET.length];
	return code;
}

// normalizeUserCode accepts what the owner types: any case, with or without
// the dash.
export function normalizeUserCode(input: string): string {
	return input.toUpperCase().replaceAll("-", "").trim();
}

export function displayUserCode(normalized: string): string {
	return normalized.length === 8 ? `${normalized.slice(0, 4)}-${normalized.slice(4)}` : normalized;
}

// The pair delivery blob lets the approved grant sit in storage as ciphertext:
// the poll response is sealed with a key derived from the device code, which
// only the pairing device holds.
export async function seal(keyBytes: Uint8Array | ArrayBuffer, plain: string): Promise<string> {
	const key = await crypto.subtle.importKey("raw", keyBytes, "AES-GCM", false, ["encrypt"]);
	const iv = crypto.getRandomValues(new Uint8Array(12));
	const sealed = await crypto.subtle.encrypt({ name: "AES-GCM", iv }, key, new TextEncoder().encode(plain));
	const out = new Uint8Array(iv.length + sealed.byteLength);
	out.set(iv, 0);
	out.set(new Uint8Array(sealed), iv.length);
	return b64(out);
}

export async function unseal(keyBytes: Uint8Array | ArrayBuffer, blob: string): Promise<string | null> {
	try {
		const bytes = unb64(blob);
		const key = await crypto.subtle.importKey("raw", keyBytes, "AES-GCM", false, ["decrypt"]);
		const plain = await crypto.subtle.decrypt({ name: "AES-GCM", iv: bytes.subarray(0, 12) }, key, bytes.subarray(12));
		return new TextDecoder().decode(plain);
	} catch {
		return null;
	}
}

export function escapeHtml(text: string): string {
	return text.replaceAll("&", "&amp;").replaceAll("<", "&lt;").replaceAll(">", "&gt;").replaceAll('"', "&quot;");
}
