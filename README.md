<p align="center">
  <img src=".github/logo.png" alt="debug-skill" width="220">
</p>

<p align="center">
  <strong>Let your coding agent debug like a human developer.</strong><br>
  Set breakpoints, step through code, evaluate expressions — the way you actually debug.
</p>
<p align="center">
  <a href="https://github.com/AlmogBaku/debug-skill/releases/latest"><img src="https://img.shields.io/github/v/release/AlmogBaku/debug-skill?style=flat-square&color=00c853" alt="Latest Release"></a>
  <a href="https://github.com/AlmogBaku/debug-skill/actions/workflows/release.yml"><img src="https://img.shields.io/github/actions/workflow/status/AlmogBaku/debug-skill/release.yml?style=flat-square" alt="Build Status"></a>
  <a href="https://github.com/AlmogBaku/debug-skill/blob/master/go.mod"><img src="https://img.shields.io/github/go-mod/go-version/AlmogBaku/debug-skill?style=flat-square" alt="Go Version"></a>
  <a href="https://github.com/AlmogBaku/debug-skill/blob/master/LICENSE"><img src="https://img.shields.io/github/license/AlmogBaku/debug-skill?style=flat-square" alt="License: MIT"></a>
  <a href="https://github.com/AlmogBaku/debug-skill/stargazers"><img src="https://img.shields.io/github/stars/AlmogBaku/debug-skill?style=flat-square&color=yellow" alt="GitHub Stars"></a>
</p>

<p align="center">
  If this saves you a debugging session, please <a href="https://github.com/AlmogBaku/debug-skill"><strong>star the repo</strong></a> — it helps others find it.
</p>

---

AI coding agents (Claude Code, Codex, Opencode, Cursor) are stuck with `print` statements and guesswork. **debug-skill** gives them what human developers have: a real debugger they can actually use. It ships two things:

- **A Claude Code skill** — teaches Claude *how* to debug: when to set breakpoints, how to step through code, how to
  inspect state
- **The `dap` CLI** — a stateless CLI wrapper around
  the [Debug Adapter Protocol](https://microsoft.github.io/debug-adapter-protocol/) so any agent can drive a real
  debugger from Bash

---

## Demo

[![Watch the demo](.github/demo.gif)](https://youtu.be/ZxXvTtilZr0)

---

## The Skill

> Install the skill, and Claude debugs your code the way *you* would — not with print statements.

The `debugging-code` skill gives Claude structured knowledge of the debugging workflow: setting breakpoints, stepping
through execution, inspecting locals and the call stack, evaluating expressions mid-run. It uses the `dap` CLI as its
tool.

### Install for Claude Code

Via the plugin marketplace — no manual setup needed:

```
/plugin marketplace add AlmogBaku/debug-skill
/plugin install debugging-code@debug-skill-marketplace
```

### Install for other agents

Via [skills.sh](https://skills.sh) — works with Cursor, GitHub Copilot, Windsurf, Cline, and [20+ more agents](https://skills.sh/docs):

```bash
npx skills add AlmogBaku/debug-skill
# or: bunx skills add AlmogBaku/debug-skill
```

Or manually copy `skills/debugging-code/SKILL.md` into your agent's system prompt or context.

---

## The `dap` CLI

`dap` wraps the Debug Adapter Protocol behind simple, stateless CLI commands. A background daemon holds the session; the
CLI sends one command and gets back the full context — no interactive terminal required.

### Install

```bash
# One-liner (Linux & macOS)
bash <(curl -fsSL https://raw.githubusercontent.com/AlmogBaku/debug-skill/master/install.sh)
```

<details>
<summary>Other install methods</summary>

```bash
# Go install
go install github.com/AlmogBaku/debug-skill/cmd/dap@latest
```

Or download a pre-built binary from the [releases page](https://github.com/AlmogBaku/debug-skill/releases/latest).

</details>

### Quick Start

```bash
# Set a breakpoint and start — stops automatically, returns full context
dap debug app.py --break app.py:42

# Inspect and step
dap eval "len(items)"
dap step

# Resume or stop
dap continue
dap stop
```

Every command returns **full context automatically**: current location, surrounding source, local variables, call stack,
and program output. No follow-up calls needed.

### Usage Examples

```bash
# Python
dap debug app.py --break app.py:42

# Go
dap debug main.go --break main.go:15

# Node.js / TypeScript
dap debug server.js --break server.js:10

# Rust / C / C++
dap debug hello.rs --break hello.rs:4

# Attach to a remote debugger (e.g. debugpy in a container)
dap debug --attach container:5678 --backend debugpy --break handler.py:20

# Pass arguments to the program
dap debug app.py --break app.py:10 -- --config prod.yaml --verbose
```

### Commands

| Command                       | Description                                     |
|-------------------------------|-------------------------------------------------|
| `dap debug <script>`          | Start debugging (local or `--attach host:port`) |
| `dap stop`                    | End session                                     |
| `dap step [in\|out\|over]`    | Step (default: over)                            |
| `dap continue`                | Resume execution                                |
| `dap context [--frame N]`     | Re-fetch current state                          |
| `dap eval <expr> [--frame N]` | Evaluate expression in current frame            |
| `dap output`                  | Drain buffered stdout/stderr since last stop    |

**Global flags:** `--json` (machine-readable output), `--session <name>` (named sessions), `--socket <path>` (custom
socket path)

### Supported Languages

| Language           | Backend     | Auto-detected |
|--------------------|-------------|:-------------:|
| Python             | debugpy     |      yes      |
| Go                 | dlv (Delve) |      yes      |
| Node.js/TypeScript | js-debug    |      yes      |
| Rust / C / C++     | lldb-dap    |      yes      |

Backend is inferred from the file extension. Override with `--backend <name>`.

### How It Works

```
dap <cmd>  →  Unix socket  →  Daemon  →  DAP protocol  →  debugpy / dlv / js-debug / lldb-dap  →  your program
```

The daemon starts automatically on `dap debug` and shuts down on `dap stop` (or after 10 min idle). It's invisible — you
never manage it directly.

### Multi-Session

Multiple agents can debug independently with named sessions:

```bash
dap debug app.py  --session agent1 --break app.py:10
dap debug main.go --session agent2 --break main.go:8

dap stop --session agent1   # stops agent1 only
```

Each session has its own daemon and socket. Omit `--session` to use the default session.

---

## Troubleshooting & Known Limitations

### Node.js / TypeScript — js-debug not found

`dap` does not bundle `js-debug`. Install it standalone (no VS Code needed):

```bash
VERSION=$(curl -fsSL https://api.github.com/repos/microsoft/vscode-js-debug/releases/latest | grep '"tag_name"' | sed 's/.*"v\([^"]*\)".*/\1/')
mkdir -p ~/.dap-cli/js-debug
curl -fsSL "https://github.com/microsoft/vscode-js-debug/releases/download/v${VERSION}/js-debug-dap-v${VERSION}.tar.gz" \
  | tar -xz -C ~/.dap-cli/js-debug
```

Or point `DAP_JS_DEBUG_PATH` at an existing `dapDebugServer.js` (e.g. from a VS Code extension).

### C/C++/Rust on macOS — lldb-dap too old

The Xcode Command Line Tools ship `lldb-dap` v17, which lacks the `--connection` flag `dap` requires.
Install a newer version via Homebrew — `dap` will prefer it automatically:

```bash
brew install llvm
```

### Go on macOS — developer mode required

Delve needs ptrace access, gated behind macOS developer mode:

```bash
sudo DevToolsSecurity -enable
```

### Attach mode — breakpoints silently not hit

When using `--attach`, breakpoints must use **absolute paths**. Relative paths silently fail to match:

```bash
# Wrong:
dap debug --attach localhost:5679 --backend debugpy --break mymodule.py:42

# Correct:
dap debug --attach localhost:5679 --backend debugpy --break /absolute/path/to/mymodule.py:42
```

### Multiprocessing / subprocess workers

`dap` attaches to the main process only. Breakpoints inside spawned worker processes will never be hit.
Workaround: start each worker with `debugpy --listen <port>` and attach a separate named `dap` session per worker.

### Conditional breakpoints

Not supported — `--break` only takes `file:line`. Workaround: add a temporary `if <cond>: pass` in source and break on that line.

---

## Contributing

PRs and issues welcome. See `claudedocs/` for architecture details and `CLAUDE.md` for code conventions.

## Support the Project

If debug-skill saves you from a painful debugging session, consider [starring the repo](https://github.com/AlmogBaku/debug-skill/stargazers) — it helps others find it and keeps the project going.

## License

MIT
