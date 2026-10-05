# gojsop workspace

Hooks and policies for [gojsop](https://github.com/efuturetoday/gojsop), one
directory each: the manifest, its script and its tests.

```bash
npm install
npm test                          # or: npx vitest (watch mode)
npx gojsop new policy <name>      # add a policy (or: new hook <name>)
npx gojsop rn <name> <new-name>   # rename one
npx gojsop rm <name>              # remove one
npm run deploy                    # tests, builds dist/, kubectl apply -f dist/
```

The scripts run in gojsop's engine, never in Node, against a cluster held in
memory. `npm run build` writes one file per hook and policy to `dist/`, with
the script inside.
