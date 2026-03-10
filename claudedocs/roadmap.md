# dap-cli Feature Roadmap

## Context

Phases 1-3 complete. Skill file and shell completions done. This covers remaining Phase 4 + ideas from [agent-debugger](https://github.com/JoaquinCampo/agent-debugger), trimmed to what matters.

**Dropped**: `dap set` (redundant — `dap eval` already handles assignments), `dap disassemble` (low value for agents), `dap source` (agents can read files directly).

---

## Sprint 1: Quick Wins

### 1. `dap pause`
- Halt a running/hung program. `PauseRequest` already in dap_client.go.
- Returns auto-context after pause.

### 2. `dap continue --to <file:line>`
- Run to a specific location without manual breakpoint management.
- Temp breakpoint → continue → remove after hit.

### 3. Exception info in auto-context
- When stopped on exception, include exception type + message in context.
- `ExceptionInfoRequest` exists in DAP. Add to context.go automatically.

---

## Sprint 2: Runtime Breakpoints

### 4. `dap break add <file:line> [--condition <expr>] [--log <msg>] [--hit-count <expr>]`
- Add breakpoints mid-session. Needs breakpoint registry in daemon.
- `SourceBreakpoint` already has `Condition`, `LogMessage`, `HitCondition` fields.
- Inspired by agent-debugger's `break file:line:condition`.

### 5. `dap break remove <file:line>`
- Remove breakpoints mid-session. Depends on #4's registry.

### 6. `dap break list`
- Show active breakpoints with their conditions. Depends on #4's registry.

### 7. Conditional breakpoints on `dap debug --break`
- Extend `--break file:line` to `--break file:line?condition`.
- Parse condition, pass through to `SourceBreakpoint.Condition`.

### 8. Breakpoint verification feedback
- Surface DAP's `verified` + `message` from `SetBreakpointsResponse`.
- Warn when a breakpoint doesn't bind.

---

## Sprint 3: Inspection

### 9. `dap inspect <variable> [--depth N]`
- Recursively expand nested objects/dicts/arrays beyond auto-context truncation.
- Resolves `variablesReference` chains up to depth.

### 10. Configurable context window — `--context-lines N`
- Parameterize the ±2 lines default. Global flag on any command that returns context.

---

## Sprint 4: Attach & Threading

### 11. PID attach — `dap debug --pid <PID>` *(from agent-debugger)*
- Debug a live running process without restart. High value for servers/hung processes.
- debugpy: inject via lldb/gdb. dlv: `dlv attach`. lldb-dap: native attach.

### 12. `dap threads`
- List threads/goroutines. `ThreadsRequest` in DAP.

### 13. `dap thread <id>`
- Switch active thread for subsequent context/eval/step commands.

### 14. `dap restart`
- Re-run with same config. Some backends support DAP restart, others need teardown/relaunch.

---

## Sprint 5: Infrastructure

### 15. GitHub Actions CI
- `make all` + E2E tests on push. Install all 4 backend adapters.

### 16. Distribution — Homebrew formula *(inspired by agent-debugger's npm)*
- Homebrew tap for easy install. Consider npm wrapper later if demand exists.

### 17. Data breakpoints — `dap watch <variable>` (stretch)
- Break on value change. Complex, backend support varies. Build only if needed.

---

## Verification

Each sprint: unit tests (mock DAP), E2E tests (Python minimum), `make all` passes.

---

## Files to Modify

| File | Changes |
|------|---------|
| `cli.go` | New commands: pause, break, inspect, threads, thread, restart. Flags: --to, --pid, --condition, --context-lines |
| `daemon.go` | Handlers, breakpoint registry, PID attach, restart, thread switching |
| `backend.go` | PID attach args per backend |
| `dap_client.go` | SetBreakpointsRequest with conditions, ExceptionInfoRequest, ThreadsRequest |
| `context.go` | Exception info, configurable source window |
| `protocol.go` | New command consts and request/response types |
| `format.go` | Formatting for threads, exceptions, breakpoint list, inspect tree |
