function validate(req) {
  const warnings = [];
  for (const c of req.object?.spec?.containers ?? []) {
    if (c.image?.endsWith(":latest")) {
      return { allowed: false, message: c.image + " uses :latest" };
    }
    if (!c.image?.includes(":")) {
      warnings.push("container " + c.name + " has no tag");
    }
  }
  return { allowed: true, warnings };
}
