# CLI API Reference

## Commands

### Session

#### `dap debug <script> [flags]`

Start a debug session. Auto-starts daemon if needed.

**Flags:**

- `--attach <host:port>` — Attach to remote DAP server (skips local spawn, requires `--backend`)
- `--backend <name>` — Debugger backend: `debugpy` (Python), `dlv` (Go), `js-debug` (Node.js/TypeScript), `lldb-dap` (
  Rust/C/C++)
- `--break <file:line>` — Set initial breakpoint (repeatable)
- `--stop-on-entry` — Stop at first line instead of running to breakpoint
- `--` — Separator for program arguments

**Examples:**

```bash
dap debug app.py --break app.py:42
dap debug app.py --break app.py:10 --break app.py:20 --stop-on-entry
dap debug --attach localhost:5678 --backend debugpy --break handler.py:15
dap debug main.go --break main.go:8
dap debug server.js --break server.js:15
dap debug hello.rs --break hello.rs:4
dap debug app.py -- --config prod.yaml --verbose
```

**Returns:** Auto-context at first stop point.

#### `dap stop`

End the debug session. Kills debug adapter and daemon.

---

### Execution

All execution commands block until the program stops and return auto-context.

#### `dap continue`

Resume execution until next breakpoint or program exit.

#### `dap step [in|out|over]`

Step through code. Default: `over`.

```bash
dap step           # step over (default)
dap step in        # step into function
dap step out       # step out of current function
```

---

### Inspection

#### `dap context [--frame N]`

Re-fetch full context without stepping. Same format as auto-context.

```bash
dap context
dap context --frame 2    # inspect a different stack frame
```

#### `dap output`

Drain and print buffered program output (stdout/stderr) since the last stop. Clears the buffer.

```bash
dap output
```

Useful when the program is running (e.g. between `continue` and the next breakpoint) or to fetch output without
re-fetching the full context.

---

#### `dap eval <expression> [--frame N]`

Evaluate an expression in the current (or specified) frame.

```bash
dap eval "len(items)"
dap eval "x + y"
dap eval "self.config" --frame 1
```

---

## Global Flags

- `--json` — JSON output format (available on all commands)
- `--session <name>` — Session name (default: `"default"`). Each session runs an independent daemon on its own socket (
  `~/.dap-cli/<name>.sock`). Allows multiple agents to debug simultaneously without interfering.
- `--socket <path>` — Custom daemon socket path (overrides `--session`)

### Multi-Session Usage

```bash
# Agent 1 debugs Python
dap debug app.py --session agent1 --break app.py:10

# Agent 2 debugs Go (fully independent)
dap debug main.go --session agent2 --break main.go:8

# Stop only agent1's session
dap stop --session agent1

# Omit --session for default session (backwards compatible)
dap debug app.py --break app.py:10
```

## Exit Codes

- `0` — Success
- `1` — Error (message on stderr)
