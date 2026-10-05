#!/bin/sh
# Installs the npm packages the way a user does and runs the tests of a new
# workspace: pack every package, `npm create @gojsop` from the tarball,
# install, `npm test`. No Go and no GOJSOP_BIN: the binary must come from
# the platform package. Needs `make cli-dist VERSION=0.0.0-smoke` and
# `npm run build` first.
set -eu
VERSION=0.0.0-smoke
sdk=$(cd "$(dirname "$0")/.." && pwd)
tgz="$sdk/build/tarballs"

node "$sdk/scripts/publish.ts" --version "$VERSION" --bin-dir "$sdk/../dist/cli" --pack "$tgz"

host=$(node -p 'process.platform + "-" + process.arch')
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

cd "$work"
tar -xzf "$tgz/gojsop-create-$VERSION.tgz"
node package/dist/index.js ws
cd ws
unset GOJSOP_BIN
npm install --no-audit --no-fund \
  "$tgz/gojsop-cli-$host-$VERSION.tgz" \
  "$tgz/gojsop-cli-$VERSION.tgz" \
  "$tgz/gojsop-types-$VERSION.tgz" \
  "$tgz/gojsop-testing-$VERSION.tgz"
npm test
npx gojsop new policy require-owner
npm test
npm run build
test -s dist/policy-no-latest.yaml
test -s dist/policy-require-owner.yaml
test -s dist/hook-count-pods.yaml
npx gojsop run dist/policy-no-latest.yaml --request policies/no-latest/pod.yaml
npx gojsop --help >/dev/null
echo "smoke test passed on $host"
