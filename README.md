# dap-cli

A CLI debugger for AI agents. Debug Python, Go, Node.js, Rust, and C++ programs through simple Bash commands.

## Why

AI coding agents can't use interactive debuggers — they need stateless CLI commands. `dap` bridges this gap with a
background daemon that holds the debug session while the CLI sends ephemeral commands.

## Install

```bash
go install github.com/AlmogBaku/debug-skill/cmd/dap@latest
```

## Usage

```bash
# Debug a Python script — stops at breakpoint, returns full context
dap debug app.py --break app.py:42
dap step
dap eval "len(items)"
dap continue
dap stop

# Debug Go
dap debug main.go --break main.go:15

# Debug Node.js / TypeScript
dap debug server.js --break server.js:10

# Debug Rust
dap debug hello.rs --break hello.rs:4

# Attach to remote debugger (e.g. debugpy in a container)
dap debug --attach container:5678 --backend debugpy --break handler.py:20

# Pass arguments to the program
dap debug app.py --break app.py:10 -- --config prod.yaml --verbose
```

Every execution command (`debug`, `step`, `continue`) returns full context automatically — current location, source
code, local variables, stack trace, and program output. No extra calls needed.

## How It Works

```
dap <command> → Unix socket → Daemon → DAP protocol → debugpy/dlv/js-debug/lldb-dap → Your program
```

The daemon starts automatically on `dap debug` and exits on `dap stop` (or after 10 min idle). It's invisible — you
never manage it directly.

## Commands

| Command                       | Description                                     |
|-------------------------------|-------------------------------------------------|
| `dap debug <script>`          | Start debugging (local or `--attach host:port`) |
| `dap stop`                    | End session                                     |
| `dap step [in\|out\|over]`    | Step (default: over)                            |
| `dap continue`                | Resume execution                                |
| `dap context [--frame N]`     | Re-fetch current state                          |
| `dap eval <expr> [--frame N]` | Evaluate expression                             |
| `dap output`                  | Drain buffered stdout/stderr since last stop    |

**Global flags:** `--json` (JSON output), `--session <name>` (named sessions), `--socket <path>` (custom socket)

## Multi-Session

Multiple agents can debug independently using named sessions:

```bash
dap debug app.py --session agent1 --break app.py:10
dap debug main.go --session agent2 --break main.go:8
dap stop --session agent1    # only stops agent1
```

Each session runs its own daemon process. Omit `--session` for the default session.

## Supported Languages

| Language           | Backend     | Status    |
|--------------------|-------------|-----------|
| Python             | debugpy     | Supported |
| Go                 | dlv (Delve) | Supported |
| Node.js/TypeScript | js-debug    | Supported |
| Rust/C/C++         | lldb-dap    | Supported |

Backends are auto-detected from file extension, or set explicitly with `--backend`.

## License

MIT
