import { configDefaults, defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    // The tests load scripts and manifests by path, so vitest cannot see
    // that they depend on them: rerun everything when one changes.
    forceRerunTriggers: [...configDefaults.forceRerunTriggers, "**/*.{ts,js,yaml}"],
  },
});
