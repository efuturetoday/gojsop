# Changelog

## [0.1.1](https://github.com/efuturetoday/gojsop/compare/v0.1.0...v0.1.1) (2026-10-05)


### Bug Fixes

* **deps:** patch vulnerable dependencies, build with Go 1.26.8 ([22bafe0](https://github.com/efuturetoday/gojsop/commit/22bafe0d2cf29965f61efa4da45712afbb46c30a))

## 0.1.0 (2026-10-05)


### ⚠ BREAKING CHANGES

* Go import path changed.
* **jshook:** handle() gets one event object instead of an array of binding contexts.
* **api:** status fields renamed; oci and sideEffects removed from the schema.
* **access:** scripts lose every right not in spec.permissions; kube.apply needs get, create, patch.
* **api:** spec.bindings is required on JSHook and the script's config() is no longer read. Scripts export handle() only. The wire format of JSAdmission is unchanged.
* **jsregistry:** scripts keep no top-level state between calls.

### Features

* **access:** run every hook and policy as its own ServiceAccount ([032d8bc](https://github.com/efuturetoday/gojsop/commit/032d8bcc93ba44bae80f838c9942d1722783cc84))
* **admission:** JSAdmission CRD with central VWC/MWC and JS dispatcher ([b84e972](https://github.com/efuturetoday/gojsop/commit/b84e9723e36e95c6a303430e8c48eb451403cb14))
* **api:** define JSHook v1alpha1 spec and status ([46a44de](https://github.com/efuturetoday/gojsop/commit/46a44dedabc1b88c0fd834211d4327115436ac38))
* **api:** move JSHook bindings into spec.bindings ([b0c144d](https://github.com/efuturetoday/gojsop/commit/b0c144d6971181f1f2d822d4fca9757bd36cdfe3))
* **api:** polish CRDs before first users ([b012ab2](https://github.com/efuturetoday/gojsop/commit/b012ab2397902713edd0ceb87eeef32c41cb0f9c))
* configmap-sync E2E demo on kind ([7bbf670](https://github.com/efuturetoday/gojsop/commit/7bbf670e5ff97bf8f5e5644918d5bc41acebc6df))
* **controllers:** react to build states, close REG-1, narrow OPS-3 ([991d842](https://github.com/efuturetoday/gojsop/commit/991d842c3e920aa4dfcbeb9f30f6e4a91afe05d1))
* **dispatcher:** dynamic informers + per-hook FIFO queue ([ff9bacd](https://github.com/efuturetoday/gojsop/commit/ff9bacdb117e52f2318d72ed3d246560de26159a))
* **dispatcher:** emit Synchronization context on Subscribe ([619103d](https://github.com/efuturetoday/gojsop/commit/619103d165a592d52e830f5670b4fd19cc045100))
* **events:** emit corev1.Event for JSHook/JSAdmission lifecycle ([b7e24fd](https://github.com/efuturetoday/gojsop/commit/b7e24fd729cda669b1fc783959da82856c26b9ca))
* inline loader + reconcile that calls hook config() ([9219ac5](https://github.com/efuturetoday/gojsop/commit/9219ac5ad5e7dea1ed78da454e7ccc791a7cde18))
* **jsadmission:** bind read-only kube.* surface, BuildOptions on registry ([367f9c4](https://github.com/efuturetoday/gojsop/commit/367f9c4b07ae1f4c653b6c042ddd49d16c7b190f))
* **jsadmission:** enforcement Deny, Warn, Audit ([29a9ac1](https://github.com/efuturetoday/gojsop/commit/29a9ac1b25098e4b83e54139ac14014130cf8b19))
* **jsadmission:** rescue VM on panic and OOM in review ([b495d17](https://github.com/efuturetoday/gojsop/commit/b495d178951581abb84cfb5cabd759ff3dc5ed06))
* **jsengine:** own QuickJS-ng wasm engine, close EXEC-8 ([10a5f0a](https://github.com/efuturetoday/gojsop/commit/10a5f0a08dd548f5a52e565b685dd172c0bd2923))
* **jshook:** handle(event) with initial and all() ([d4fc6cd](https://github.com/efuturetoday/gojsop/commit/d4fc6cd5bf8946e674e4630a6bc455810368ece8))
* **jslog:** give scripts a console ([6dcf3a6](https://github.com/efuturetoday/gojsop/commit/6dcf3a63462a8142e851f6465d702ea99a97bc69))
* **jsregistry:** fan out Watch to every subscriber ([3bf551b](https://github.com/efuturetoday/gojsop/commit/3bf551b18c5e6763890b153b73e14351a2db33bb))
* **jsregistry:** per-key state and async Ensure ([e7878c3](https://github.com/efuturetoday/gojsop/commit/e7878c3a299c0d788367814e92bc7953f3c5d71a))
* **jsregistry:** run every call single shot from snapshot ([a3d7972](https://github.com/efuturetoday/gojsop/commit/a3d797282126c93ce6777dc4b67a41601f5bae47))
* **jssource:** add ConfigMap source loader with watch-driven reconcile ([f468797](https://github.com/efuturetoday/gojsop/commit/f4687970b5be6ad6c0a52a0636baed2dab3e5cfb))
* **runtime:** BindingContext type + Engine.Handle ([120baa3](https://github.com/efuturetoday/gojsop/commit/120baa3cc85fe1affcce1f972eced6bfc6121a9e))
* **runtime:** enforce spec.resources.memoryMB via qjs MemoryLimit ([0ae0d80](https://github.com/efuturetoday/gojsop/commit/0ae0d806d46a2ce31c9e9602d4e3772fd6d636f0))
* **runtime:** engine skeleton with persistent qjs instance ([b3408b7](https://github.com/efuturetoday/gojsop/commit/b3408b71ff419d4bae2ef3e412497df06dfdf38a))
* **runtime:** kube.apply/get/list/delete host functions ([1fcac85](https://github.com/efuturetoday/gojsop/commit/1fcac854c2d0377aa7f6d2fe00a2c3de5e4b2799))
* **runtime:** persistent instance registry ([930edb4](https://github.com/efuturetoday/gojsop/commit/930edb4a73515b9038786f5d3cf91d03704f192e))
* **runtime:** wire memory/panic/timeout/manual restart triggers ([520fe4e](https://github.com/efuturetoday/gojsop/commit/520fe4e3c7168a19a23d44d993fb96428244eb5c))
* scaffold JSHook v1alpha1 API + controller ([78edcfb](https://github.com/efuturetoday/gojsop/commit/78edcfb8629431eb88fe2f47b657dd2e543e2906))


### Bug Fixes

* **dispatcher:** hashable queue keys + cache config() across reconciles ([28d3e01](https://github.com/efuturetoday/gojsop/commit/28d3e018e08b90ea1450fd49373817aa1631137c))
* **dispatcher:** keep d.mu out of cache sync, close DISP-8 ([d582a8c](https://github.com/efuturetoday/gojsop/commit/d582a8c0179b3b5fb51f4615a1bcfbff54a605d5))
* **dispatcher:** survive a cancelled handle(), close DISP-12 ([aabd213](https://github.com/efuturetoday/gojsop/commit/aabd213becffb19a6de2ebe3294c6afdf51699b7))
* **jsadmission:** answer admission requests on every replica ([0acb140](https://github.com/efuturetoday/gojsop/commit/0acb14010f7ba3e4eacc52cb1c133a447290611e))
* **jsadmission:** follow CA renewal, exclude system namespaces ([61b3b1a](https://github.com/efuturetoday/gojsop/commit/61b3b1aa6280d69e6b7d1dd9eb1bef22446bca80))
* **jsadmission:** surface registrar sync failure, drop patch on deny (ADM-1, ADM-2) ([ba6844a](https://github.com/efuturetoday/gojsop/commit/ba6844aace062b9dc4af9b91afb7f9f0bd618154))
* **jsengine:** close EXEC-5, add -race to make test (GATE-3) ([e490290](https://github.com/efuturetoday/gojsop/commit/e490290dde1ffa329ac3ef9bd07aad1eec1eb3ea))
* **jshook:** mark schedule and onStartup bindings inactive in status ([091c44c](https://github.com/efuturetoday/gojsop/commit/091c44c817a4c45fd3e38a4bd696e76e169da886))
* **jsregistry:** calls never build, close REG-8 ([6d6b03a](https://github.com/efuturetoday/gojsop/commit/6d6b03a2b926859b274f8ff95708bf6f4ef50d5f))
* **jsregistry:** key VMs by kind and name, close REG-9 ([b287419](https://github.com/efuturetoday/gojsop/commit/b287419e5171bffe056c76d6bf21558d3b7dcda5))
* **jsregistry:** rebuild on limits change, bound rescue build, add concurrent test ([92dce0f](https://github.com/efuturetoday/gojsop/commit/92dce0fd1bf60110fe6e36da86c5dddb29b09ead))
* **lint:** clean golangci-lint, close GATE-21 ([ec9be2f](https://github.com/efuturetoday/gojsop/commit/ec9be2fb5a816c3ff864c1b93b00dcd303da5050))
* **runtime:** fall back to JSON.stringify when qjs returns empty ([22d2b84](https://github.com/efuturetoday/gojsop/commit/22d2b840d2e7f00e83e68c8d396eb6cd0ff2ce98))
* **runtime:** use qjs JsObjectOrMapToGoMap and ToJsValue, drop JSON detour ([97c13ce](https://github.com/efuturetoday/gojsop/commit/97c13ce32cf5db9ecd4c2b8da0c165d98cc18d93))


### Build System

* move module to github.com/efuturetoday/gojsop ([129870d](https://github.com/efuturetoday/gojsop/commit/129870dfa50e7c02ae064bcf1ba1cc3d539a7c08))
