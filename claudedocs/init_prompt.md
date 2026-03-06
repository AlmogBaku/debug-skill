# Implementation Task

You are implementing **dap-cli** — a CLI debugging tool for AI agents that wraps the Debug Adapter Protocol (DAP).

## Before You Start

1. Read all docs in `claudedocs/` — they contain the full design, API spec, and implementation plan.
2. Read `CLAUDE.md` for project conventions.
3. Read `README.md` for the user-facing overview.

## What to Build

A single Go binary (`dap`) with these key components:

1. **DAP client** — DAP protocol client built on `github.com/google/go-dap`.
2. **Backend interface** — Pluggable debug adapters: debugpy (Python), dlv (Go), js-debug (Node.js/TypeScript),
   lldb-dap (Rust/C/C++).
3. **IPC protocol** — Request/Response types for CLI-daemon communication.
4. **Daemon** — Background process with Unix socket server + async DAP event loop + idle timeout.
5. **Auto-context** — Aggregate stack + source + locals + output into one response.
6. **Formatting** — Text and JSON output formatters with truncation.
7. **CLI** — Cobra commands that connect to daemon and print results.
8. **Entry point** — Cobra root command setup.

See `claudedocs/implementation.md` for file structure and phases.

## Principles

- **TDD**: Write tests first.
- **KISS**: Simple > clever. Flat file structure. No premature abstraction.
- **Short codebase**: Every line should earn its place.
