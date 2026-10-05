import type { Request, Response } from "@gojsop/types";

export function validate(req: Request): Response {
  const warnings: string[] = [];
  for (const c of req.object?.spec?.containers ?? []) {
    if (c.image?.endsWith(":latest")) {
      return { allowed: false, message: `${c.image} uses :latest` };
    }
    if (!c.image?.includes(":")) {
      warnings.push(`container ${c.name} has no tag`);
    }
  }
  return { allowed: true, warnings };
}
