# Security policy

gojsop runs user code inside your cluster and hands it Kubernetes rights, so
we take security reports seriously.

## Reporting a vulnerability

Please do **not** open a public issue. Report it privately through
[GitHub's private vulnerability reporting](https://github.com/efuturetoday/gojsop/security/advisories/new).

Include what you found, how to reproduce it, and what an attacker could do
with it. We aim to answer within a week and to agree on a disclosure date
with you.

## Supported versions

gojsop is in alpha. Only the latest release gets security fixes.

## In scope

Especially interesting:

- a script that escapes its sandbox, reaches the network, or exceeds its
  memory or time limit,
- a script that gets rights beyond its `spec.permissions`, or a user who
  creates a hook or policy with rights they do not hold,
- an admission policy that can be bypassed, or that blocks the cluster
  beyond its rules.
