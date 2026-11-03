import { cloudflareTest } from "@cloudflare/vitest-plugin";
import { defineConfig } from "vitest/config";

export default defineConfig({
	plugins: [
		cloudflareTest({
			wrangler: { configPath: "./wrangler.jsonc" },
			// The plugin's bundled workerd is a few days behind the Wrangler
			// config's compatibility date; pin the test runtime to its newest.
			miniflare: {
				compatibilityDate: "2026-10-30",
				bindings: { API_KEY: "test-key-0123456789abcdef0123456789abcd" },
			},
		}),
	],
	test: {
		include: ["test/**/*.spec.ts"],
	},
});
