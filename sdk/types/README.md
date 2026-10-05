# @gojsop/types

Types for [gojsop](https://github.com/efuturetoday/gojsop) scripts:

```ts
import type { Request, Response } from "@gojsop/types";

export function validate(req: Request): Response {
  return { allowed: true };
}
```

Importing the package also declares the global `kube`.
