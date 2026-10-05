# @gojsop/create

Creates a workspace for [gojsop](https://github.com/efuturetoday/gojsop)
hooks and policies, with vitest tests that run the scripts in gojsop's
engine.

```sh
npm create @gojsop my-policies
cd my-policies
npm install
npm test
```

The workspace holds one example policy (`policies/no-latest`) and one example
hook (`hooks/count-pods`); copy a directory to start a new one.
