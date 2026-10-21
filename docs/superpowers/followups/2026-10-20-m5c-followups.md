# M5c (Auto modes, retries and adaptive probes) follow-ups

Deferred items from the final review of `docs/superpowers/plans/2026-10-20-m5c-auto-retries-probes.md`.

## Operator

- **D11 window cap.** `status.remediations` counts consecutive remediations, and a fully passing scheduled probe run resets it, together with `status.retries`. A resource that something keeps reverting more slowly than one scheduled run apart therefore never reaches the cap, and its retries never back off: each drift is remediated at once. Decide whether the cap should count remediations within a time window instead (for example, 3 per hour), and whether the backoff should then survive a passing run.
