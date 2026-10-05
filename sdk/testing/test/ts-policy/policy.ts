import type { Request, Response } from "@gojsop/types";
import { isLatest, refuse } from "./rules";

export function validate(req: Request): Response {
  for (const c of req.object?.spec?.containers ?? []) {
    if (c.image === "boom:1") refuse(c.image);
    if (isLatest(c.image)) return { allowed: false, message: `${c.image} uses :latest` };
  }
  return { allowed: true };
}
