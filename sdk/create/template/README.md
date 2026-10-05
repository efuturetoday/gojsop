# gojsop workspace

Hooks and policies for [gojsop](https://github.com/efuturetoday/gojsop), one
directory each: the manifest, its script and its vitest tests.

```sh
npm install
npm test            # or: npx vitest (watch mode)
npm run deploy      # tests, then kubectl apply -k .: every hook and policy, scripts as ConfigMaps
```

The scripts run in gojsop's engine, never in Node, against a cluster held in
memory. Start a new hook or policy by copying a directory and listing it in
`kustomization.yaml`. To remove one, run `kubectl delete -k <directory>`
before you drop it from `kustomization.yaml`; `kubectl apply` does not prune.
