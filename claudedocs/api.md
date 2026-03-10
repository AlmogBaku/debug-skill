# CLI API Reference

## Commands

### Session

#### `dap debug <script> [flags]`

Start a debug session. Auto-starts daemon if needed.

**Flags:**

- `--attach <host:port>` — Attach to remote DAP server (skips local spawn, requires `--backend`)
- `--backend <name>` — Debugger backend: `debugpy` (Python), `dlv` (Go), `js-debug` (Node.js/TypeScript), `lldb-dap` (
  Rust/C/C++)
- `--break <file:line[:condition]>` — Set initial breakpoint (repeatable). For conditions with spaces, quote the spec: `--break "app.py:42:x > 5"`
- `--stop-on-entry` — Stop at first line instead of running to breakpoint
- `--break-on-exception <filter>` — Stop on exception; repeatable. Filter IDs are backend-specific:
  - `debugpy` (Python): `raised`, `uncaught`, `userUnhandled`
  - `dlv` (Go): `all`, `uncaught`
  - `js-debug` (Node): `all`, `uncaught`
  - `lldb-dap`: `on-throw`, `on-catch`
- `--` — Separator for program arguments

**Examples:**

```bash
dap debug app.py --break app.py:42
dap debug app.py --break "app.py:42:x > 5"
dap debug app.py --break app.py:10 --break app.py:20 --stop-on-entry
dap debug --attach localhost:5678 --backend debugpy --break handler.py:15
dap debug main.go --break main.go:8
dap debug server.js --break server.js:15
dap debug hello.rs --break hello.rs:4
dap debug app.py -- --config prod.yaml --verbose
dap debug app.py --break-on-exception raised
dap debug app.py --break-on-exception uncaught
```

**Returns:** Auto-context at first stop point.

#### `dap stop`

End the debug session. Kills debug adapter and daemon.

---

### Execution

All execution commands block until the program stops and return auto-context.

#### `dap continue`

Resume execution until next breakpoint or program exit.

**Flags:**
- `--break <file:line[:condition]>` — Add a breakpoint before continuing (repeatable, additive with existing breakpoints). Quote conditions: `--break "app.py:42:x > 5"`
- `--remove-break <file:line>` — Remove a breakpoint before continuing (repeatable)
- `--break-on-exception <filter>` — Set exception breakpoints before continuing (repeatable, replaces current filters)

```bash
dap continue --break app.py:42              # add a breakpoint and continue
dap continue --break "app.py:50:len(items) == 0"  # conditional breakpoint
dap continue --remove-break app.py:10       # remove a breakpoint and continue
dap continue --break-on-exception raised    # set exception breakpoints and continue
```

#### `dap step [in|out|over]`

Step through code. Default: `over`.

**Flags:**
- `--break <file:line[:condition]>` — Add a breakpoint before stepping (repeatable). Quote conditions: `--break "app.py:42:x > 5"`
- `--remove-break <file:line>` — Remove a breakpoint before stepping (repeatable)
- `--break-on-exception <filter>` — Set exception breakpoints before stepping (repeatable, replaces current)

```bash
dap step           # step over (default)
dap step in        # step into function
dap step out       # step out of current function
dap step --break app.py:42   # add breakpoint, then step
```

---

### Inspection

#### `dap context [--frame N]`

Re-fetch full context without stepping. Same format as auto-context.

**Flags:**
- `--frame N` — Stack frame to inspect (0 = innermost, default)
- `--break <file:line[:condition]>` — Add a breakpoint (repeatable). Quote conditions: `--break "app.py:42:x > 5"`
- `--remove-break <file:line>` — Remove a breakpoint (repeatable)
- `--break-on-exception <filter>` — Set exception breakpoints (repeatable, replaces current)

```bash
dap context
dap context --frame 2    # inspect a different stack frame
dap context --break app.py:42   # add breakpoint and re-fetch context
```

#### `dap output`

Drain and print buffered program output (stdout/stderr) since the last stop. Clears the buffer.

**Flags:**
- `--break <file:line[:condition]>` — Add a breakpoint (repeatable). Quote conditions: `--break "app.py:42:x > 5"`
- `--remove-break <file:line>` — Remove a breakpoint (repeatable)
- `--break-on-exception <filter>` — Set exception breakpoints (repeatable, replaces current)

```bash
dap output
dap output --break app.py:42   # add breakpoint and drain output
```

Useful when the program is running (e.g. between `continue` and the next breakpoint) or to fetch output without
re-fetching the full context.

---

#### `dap eval <expression> [--frame N]`

Evaluate an expression in the current (or specified) frame.

**Flags:**
- `--frame N` — Stack frame for evaluation context
- `--break <file:line[:condition]>` — Add a breakpoint (repeatable). Quote conditions: `--break "app.py:42:x > 5"`
- `--remove-break <file:line>` — Remove a breakpoint (repeatable)
- `--break-on-exception <filter>` — Set exception breakpoints (repeatable, replaces current)

```bash
dap eval "len(items)"
dap eval "x + y"
dap eval "self.config" --frame 1
```

---

### Breakpoint Management

#### `dap break list`

List all breakpoints and exception filters in the current session.

```bash
dap break list
dap break list --json
```

#### `dap break add <file:line[:condition]> [...]`

Add one or more breakpoints or exception filters. Setting a breakpoint on a line that already has one replaces the condition.

**Flags:**
- `--break-on-exception <filter>` — Set exception breakpoint filters (repeatable, replaces current filters)

```bash
dap break add app.py:42
dap break add "app.py:42:x > 5"
dap break add "app.py:50:len(items) == 0"
dap break add app.py:10 app.py:20
dap break add --break-on-exception raised
```

#### `dap break remove <file:line> [file:line...]`

Remove one or more breakpoints or exception filters. Alias: `dap break rm`.

**Flags:**
- `--break-on-exception <filter>` — Remove exception filter (repeatable)

```bash
dap break remove app.py:42
dap break rm app.py:10 app.py:20
dap break remove --break-on-exception raised
```

#### `dap break clear`

Remove all breakpoints and exception filters.

```bash
dap break clear
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

## Breakpoint Verification

When breakpoints are set, the debug adapter verifies each one. If a breakpoint cannot be bound (e.g. invalid line number, file not yet loaded), or if the adapter adjusts it to a different line, a warning is included in the next response output. Warnings appear in a `Warnings:` section and do not prevent debugging — they are informational.

## Exit Codes

- `0` — Success
- `1` — Error (message on stderr)
