# @gojsop/cli

The [gojsop](https://github.com/efuturetoday/gojsop) CLI. It runs hooks and
policies on your machine, in the engine the operator uses, against a cluster
held in memory.

```sh
npm install --save-dev @gojsop/cli
npx gojsop run policy.yaml --request request.yaml --trace
```

npm installs the binary for your platform as an optional dependency
(`@gojsop/cli-<os>-<arch>`): macOS and Linux on x64 and arm64, Windows on
x64. Set `GOJSOP_BIN` to use another binary.

To test hooks and policies with vitest, use
[`@gojsop/testing`](https://www.npmjs.com/package/@gojsop/testing); it
depends on this package.
