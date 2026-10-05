import { fileURLToPath } from "node:url";
import { defineConfig } from "vitest/config";

const here = (p: string) => fileURLToPath(new URL(p, import.meta.url));

// The package's own tests, plus the examples that sit next to the example
// hooks and policies; both import the built package like a user would.
export default defineConfig({
  resolve: {
    alias: {
      "@gojsop/testing": here("./dist/index.js"),
      vitest: here("./node_modules/vitest/dist/index.js"),
    },
  },
  server: { fs: { allow: [here("../..")] } },
  test: {
    include: ["test/**/*.test.ts", "../../internal/workspace/testdata/**/*.test.ts"],
    testTimeout: 30_000,
  },
});
