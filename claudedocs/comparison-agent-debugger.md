# Comparison: dap-cli vs agent-debugger

Comparison with [JoaquinCampo/agent-debugger](https://github.com/JoaquinCampo/agent-debugger) — a TypeScript/Node.js CLI debugger for AI agents, also built on DAP.

## Architecture Similarities

Both projects share nearly identical architecture:
```
CLI (stateless) → Unix socket → Daemon (holds session) → DAP → Debug Adapter → Target
```

Both support the same languages (Python/debugpy, Go/dlv, Node.js/js-debug, Rust+C/C++) and the same core operations (breakpoints, stepping, variables, eval, stack traces).

## What We Can Learn

### 1. Process Attach by PID
agent-debugger supports `attach --pid <PID>` — injects debugpy into a running process via lldb/gdb without restart or code changes. We only support `--attach host:port`. PID attach is high-value for debugging live servers.

### 2. Runtime Breakpoint Management
They support `break file:line[:condition]` mid-session. We only set breakpoints at launch. Our Phase 4 plans this but it's not built yet.

### 3. Conditional Breakpoints
`break file:line:condition` — pause only when an expression evaluates to true. We don't support conditions.

### 4. Source View Command
`source [file] [line]` — view source code of any file, centered around a line. We only show 5 lines around current position in auto-context.

### 5. SKILL.md — Agent Debugging Methodology
They ship a skill file with opinionated debugging rules for AI agents:
- "Read first, debug second"
- "Eval, don't dump"
- "Never step through loops"
- "Two strikes, new theory"

This teaches agents *how* to debug, not just gives them the tool. Worth adopting.

### 6. npm Distribution
`npm install -g agent-debugger` — broader reach than `go install`. Consider also publishing to npm/brew/apt.

## What We Do Better

### 1. Auto-Context (biggest differentiator)
Every execution command returns location + source + locals + stack + output in one response. agent-debugger requires separate calls: `step` → `vars` → `stack` → `source`. For AI agents where each call costs time and tokens, auto-context is a fundamental design win.

### 2. Go Binary (zero runtime dependencies)
Single static binary vs Node.js >= 18 requirement. No `node_modules`, no npm version conflicts. More reliable for a debugging tool.

### 3. Robust IPC (length-prefixed JSON vs NDJSON)
4-byte length-prefixed JSON can't be broken by newlines in payloads. Their NDJSON framing is fragile.

### 4. Output Capture
We capture and buffer stdout/stderr (bounded at 200 lines) and return it in auto-context. They silently drain output events. Agents need to see what the program printed.

### 5. Multi-Session Support
`--session <name>` gives independent daemon + socket. They're single-session only. Agents debugging multiple processes need this.

### 6. Idle Timeout
10-min auto-exit prevents orphan daemons. They rely on explicit `close` — if an agent crashes mid-session, their daemon lingers.

### 7. Bounded Output & Truncation
Strings (200 chars), collections (5 items), stack (20 frames), source (2 lines context), output (200 lines). Everything is bounded to prevent overwhelming the agent. They return up to 50 frames and don't truncate as aggressively.

### 8. JSON Output Mode
`--json` flag for machine-readable output. They return formatted text only.

### 9. Backend Edge Cases
js-debug child session swapping, Go/Rust auto-compilation, per-backend variable filtering — more battle-tested against real-world adapter quirks.

## Action Items

1. **PID attach** — `dap debug --pid 12345` (high priority)
2. **Runtime breakpoints** — `dap break add/remove/list` (already in Phase 4)
3. **Conditional breakpoints** — `--break file:line:condition`
4. **Source view command** — `dap source [file] [line]`
5. **Agent skill file** — Ship a SKILL.md with debugging methodology
6. **Distribution** — Consider npm wrapper, Homebrew formula
