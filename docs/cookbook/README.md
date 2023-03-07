# decision-gateway cookbook

Operator-level recipes for putting the gateway in front of markup-svc (and any future decision-shaped backend). Each recipe is one page, names the relevant ADRs and `cmd/decision-gateway` flags, and ends with a "what to check after" section.

## Recipes

| Recipe | When to use |
|---|---|
| [three-service-smoke.md](three-service-smoke.md) | Run markup-svc + decision-gateway + traffic-gen on one host and verify the wire works end-to-end |
| [compose-stack.md](compose-stack.md) | Bring the full platform up in one command via `docker compose up` (uses the published ghcr.io images) |

## How these recipes are written

Each recipe answers one operational question. The format:

1. **Problem** — one sentence stating what the operator is trying to do.
2. **Recipe** — copy-paste commands.
3. **What's happening** — one paragraph explaining the mechanism.
4. **What to check after** — concrete signals (log lines, response shapes) that confirm the recipe worked.
5. **Relevant ADRs and flags** — pointers into the design docs.

If a recipe and an ADR disagree, the ADR is the source of truth — file a follow-up to fix the recipe.
