// The npm package that carries the gojsop binary for each platform, keyed by
// `${process.platform}-${process.arch}`, with the Go target it is built for.
// scripts/publish.ts builds one package per entry and lists them as
// optionalDependencies of @gojsop/cli, so npm installs only the matching one.
export interface Platform {
  /** npm package that holds the binary */
  pkg: string;
  goos: string;
  goarch: string;
}

export const platforms: Record<string, Platform> = {
  "darwin-arm64": { pkg: "@gojsop/cli-darwin-arm64", goos: "darwin", goarch: "arm64" },
  "darwin-x64": { pkg: "@gojsop/cli-darwin-x64", goos: "darwin", goarch: "amd64" },
  "linux-arm64": { pkg: "@gojsop/cli-linux-arm64", goos: "linux", goarch: "arm64" },
  "linux-x64": { pkg: "@gojsop/cli-linux-x64", goos: "linux", goarch: "amd64" },
  "win32-x64": { pkg: "@gojsop/cli-win32-x64", goos: "windows", goarch: "amd64" },
};
