import { cloudflareTest } from "@cloudflare/vitest-plugin";
import { defineConfig } from "vitest/config";

export default defineConfig({
	plugins: [
		cloudflareTest({
			wrangler: { configPath: "./wrangler.jsonc" },
			// The plugin's bundled workerd is a few days behind the Wrangler
			// config's compatibility date; pin the test runtime to its newest.
			miniflare: { compatibilityDate: "2026-10-30" },
		}),
	],
	test: {
		include: ["test/**/*.spec.ts"],
	},
});
