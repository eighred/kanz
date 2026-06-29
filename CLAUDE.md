# Claude Global Rules 
## Core Principles 
- Read only necessary files 
- Never scan entire repository unnecessarily 
- Prefer minimal diffs 
- Preserve architecture 
- Reuse existing utilities 
- Avoid unnecessary abstractions 
- Keep outputs concise 
- No tutorials unless requested 
- No unnecessary explanations 

--- 

## Engineering Standards 
- Production-grade code only 
- Maintain existing conventions 
- Prioritize readability 
- Prefer simple implementations 
- Avoid dependency bloat 
- Handle meaningful edge cases 

--- 

## Output Rules 
- Return concise responses 
- Prefer unified diffs 
- Do not print unchanged code 
- Keep responses under 200 lines 
- Do not narrate actions 

---

## Workflow 
1. Identify minimal viable modification 
2. Inspect only required files 
3. Implement smallest correct solution 
4. Verify mentally 
5. Return concise output

---

## Command: `/forge-tasks` — Continuous Improvement Backlog

Trigger: the user types `/forge-tasks` (aliases: "generate next tasks", "forge the next tranche", "what's next"). On trigger, produce the next tranche of tasks that genuinely move this system toward production / Aladdin-class parity, then update `KANZ_TASKS.md`. Run it repeatedly — each invocation extends or refills the backlog.

### Ground truth first (no fabrication)
- Read `KANZ_TASKS.md` (TODO, IN PROGRESS, the `PATH TO PARITY` roadmap, and the **carried-forward** notes in DONE) and `KANZ_BRAIN.md` (Key Decisions, Assumptions/anti-decisions). 
- Inspect the actual code for the gap each candidate task names — the seam, the Sim/Stub default, the in-memory store, the composition-root TODO. A task must close a **real** gap visible in the repo, not a guess. 
- Never invent work to pad the list. If the only honest remaining work is one task, write one. If a "gap" turns out already done, say so and drop it. 

### Select by leverage, not by volume
- Rank candidates by: (1) unblocks the most downstream work, (2) retires a carried-forward seam / makes a stubbed analytic real, (3) closes a correctness, security, or regulatory gap, (4) ROI toward a shippable institutional capability. 
- Respect dependency gates (real data + durable state before calibration; correctness before scale; everything before certification). 
- Prefer depth that makes an existing capability production-true over breadth that adds another scaffold.

### Task quality bar (every generated task)
- Board convention: module-prefixed id, lettered subtasks, **scoped 1–3 engineer-days**, ending in ` · ` the concrete file/dir path. 
- State the **specific seam/stub it retires** and the **existing pattern it reuses** (cite it) — preserve architecture, no parallel re-implementations. 
- Carry an implicit **acceptance bar**: tests (board convention) + `go build/vet/gofmt` clean + arch boundary test unchanged + the relevant carried-forward note retired. 
- No dependency bloat (justify any new dep against the no-bloat rule); no speculative abstraction; no busywork tests. 
- Do not duplicate anything already in DONE.

### Maintain the board
- Append/extend epics in `PATH TO PARITY` (or open a new dependency-sequenced phase when the roadmap is exhausted); keep the milestone/sequencing map honest. 
- Keep `TODO` as the **active tranche** only (the current sprint's pull); refill it from the roadmap when it drains. 
- When an epic completes: mark its subtasks done, add the one-line `KANZ_BRAIN.md` Key-Decision bullet, and retire its carried-forward note. 
- Output a concise summary of what was added and why it's the highest-leverage next step — recommend the single first task to start.