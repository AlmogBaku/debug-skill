# Design: dap-cli — DAP Debugger CLI for AI Agents

## Problem

AI coding agents (Claude Code, Cursor, etc.) can't debug programs interactively. When an agent encounters a bug, it
resorts to adding print statements, re-running, and guessing. Real debuggers (pdb, debugpy, delve) are interactive —
they block waiting for input, which is incompatible with agents that use ephemeral CLI calls.

Existing solutions fall short:

- **MCP-based debuggers** (mcp-debugger): protocol overhead, error-prone, chatty APIs requiring many round-trips
- **VSCode-coupled** (Microsoft DebugMCP): requires VSCode running as a dependency
- **Too immature** (python-debugger-skill, debugger-cli): early-stage, missing features

## Solution

A CLI-native DAP debugger with a background daemon. The daemon holds the stateful debug session; the CLI sends ephemeral
commands via Unix socket. Agents call it through Bash — the simplest, most reliable tool interface.

```
dap <command>  (cobra CLI, thin client)
     |
     | Unix socket (length-prefixed JSON, request/response)
     v
  Daemon  (background process, holds DAP session, buffers events)
     |
     | TCP or stdio (DAP protocol via google/go-dap)
     v
  Debug Adapter  (debugpy / dlv / js-debug / lldb-dap)
     |
     v
  Target Process  (local or remote/container)
```

## Key Design Decisions

### 1. CLI over MCP

MCP adds protocol ceremony (tool discovery, typed schemas, session management) that doesn't help for debugging. CLI is
simpler: one Bash call = one command. Errors are just stderr. Output is just stdout. Agents already have Bash.

### 2. Auto-Context Pattern

Every execution command (`debug`, `continue`, `step`) blocks until the program stops, then automatically returns full
context: location + source + locals + stack + recent program output. This eliminates follow-up "where am I?" / "what are
the variables?" calls. Single biggest turn-saver.

### 3. Invisible Daemon

The daemon is an implementation detail. No `dap daemon start` command. `dap debug` auto-starts it; `dap stop` kills it.
The agent never thinks about session management.

### 4. Multi-Session Isolation

Each `--session` name maps to its own socket path and daemon process. No shared state, no mutex, no registry.

```
dap debug --session agent1 ...  →  ~/.dap-cli/agent1.sock  →  daemon pid 1
dap debug --session agent2 ...  →  ~/.dap-cli/agent2.sock  →  daemon pid 2
dap debug ...                   →  ~/.dap-cli/default.sock  →  daemon pid 3
```

Default session = `"default"` (backwards compatible). Each daemon has an idle timeout (10 min) to prevent orphans.

### 5. DAP Protocol (not bdb/pdb)

DAP gives us: exception breakpoints, conditional breakpoints, multi-thread debugging, remote attach, variable
modification, disassembly — all standardized across languages. Python's bdb is too limited.

### 6. google/go-dap Library

The only viable Go DAP library. Provides message types + wire protocol codec. We build the client on top (~240 lines).

## Inspirations

| Project                            | What we take                                                       | What we skip                                        |
|------------------------------------|--------------------------------------------------------------------|-----------------------------------------------------|
| **debugger-cli** (akiselev)        | Daemon architecture pattern (background process + Unix socket IPC) | Rust codebase, GPL license, no JSON output          |
| **python-debugger-skill** (alonw0) | Skill/methodology concept, truncation strategy                     | bdb-based (too limited), immature                   |
| **Microsoft DebugMCP**             | Tool naming, `get_debug_instructions` concept                      | VSCode coupling, all logic delegated to VSCode APIs |

## Backend Abstraction

Backend interface for debug adapter abstraction:

```go
type Backend interface {
// Local debugging: spawn the debug adapter process
Spawn(port string) (cmd *exec.Cmd, addr string, err error)
// How to connect: "tcp" or "stdio"
TransportMode() string
// DAP adapter identifier for InitializeRequest
AdapterID() string
// Build DAP launch argument map. Returns cleanup func for temp binaries (Go, Rust/C/C++ compilation).
LaunchArgs(program string, stopOnEntry bool, args []string) (map[string]any, func(), error)
// Build DAP attach argument maps
AttachArgs(pid int) (map[string]any, error)
RemoteAttachArgs(host string, port int) (map[string]any, error)
// StopOnEntryBreakpoint returns a function name for stop-on-entry.
// If empty, native stopOnEntry is used. (e.g. dlv returns "main.main")
StopOnEntryBreakpoint() string
}
```

**debugpy backend** (Python): spawns `python3 -m debugpy.adapter --host 127.0.0.1 --port PORT --log-stderr`, connects
via TCP.

**dlv backend** (Go): spawns `dlv dap --listen :PORT`, connects via TCP. Compiles Go source to temp binary before
launch.

**js-debug backend** (Node.js/TypeScript): spawns `@vscode/js-debug` standalone DAP server, connects via TCP.
Multi-session architecture — handles `StartDebuggingRequest` reverse request to create child sessions. Detects `.js`,
`.ts`, `.mjs`, `.cjs` files.

**lldb-dap backend** (Rust/C/C++): spawns `lldb-dap --connection listen://127.0.0.1:PORT`, connects via TCP. Compiles
Rust source to temp binary before launch. Native `stopOnEntry` works.

## Daemon Architecture

### Async Event Model

The daemon uses an async model with a whitelist-based event dispatch:

```
CLI request arrives via Unix socket
  → daemon handler sends DAP request
  → blocks waiting on response channel

Reader goroutine (runs continuously):
  → reads DAP messages from debug adapter
  → dispatches by type:
      StoppedEvent        → expectCh (triggers context collection)
      TerminatedEvent     → expectCh
      InitializedEvent    → expectCh
      ExitedEvent         → expectCh
      ResponseMessage     → expectCh
      OutputEvent         → appends to output buffer (capped at 200 lines, bounded at write time)
      StartDebuggingReq   → creates child session (js-debug multi-session)
      everything else     → silently dropped
```

### Session Lifecycle

1. `dap debug script.py` → CLI checks for running daemon → none found → forks daemon process
2. Daemon starts, listens on Unix socket `~/.dap-cli/<session>.sock`, starts idle timer (10 min)
3. CLI connects, sends `debug` command — idle timer resets on every command
4. Daemon spawns debug adapter, connects DAP, runs initialization sequence
5. Subsequent CLI calls (`step`, `eval`, etc.) connect to same socket
6. `dap stop` → daemon disconnects DAP, kills adapter, removes socket, exits
7. If no commands for 10 min, daemon exits automatically (prevents orphans)

### IPC Protocol

Length-prefixed JSON over Unix socket. Each CLI invocation opens a connection, sends one request, reads one response,
closes.

```json
// Request
{
  "command": "step",
  "args": {
    "mode": "over"
  }
}

// Success response (auto-context)
{
  "status": "stopped",
  "reason": "step",
  "location": {
    "file": "app.py",
    "line": 42,
    "function": "process_items"
  },
  "source": [
    ...
  ],
  "locals": [
    ...
  ],
  "stack": [
    ...
  ],
  "output": "recent program output..."
}

// Error response
{
  "status": "error",
  "error": "debugger not started"
}

// Termination response
{
  "status": "terminated",
  "exit_code": 0,
  "output": "final output..."
}
```

## Remote Debugging

### Flow: `dap debug --attach container-host:5678 --backend debugpy`

**In the container:**

```bash
python -m debugpy --listen 0.0.0.0:5678 --wait-for-client script.py
```

**On the agent's machine:**

```bash
dap debug --attach container-host:5678 --backend debugpy --break handler.py:20
```

**Internal sequence:**

1. Daemon skips `backend.Spawn()` — no local process
2. Connects directly: `net.Dial("tcp", "container-host:5678")`
3. DAP `initialize` → `attach` (backend-specific args) → `setBreakpoints` → `configurationDone`
4. Waits for `StoppedEvent` → returns auto-context
5. All subsequent commands work identically to local debugging

The agent doesn't know or care that the target is remote. Same commands, same output.

## Auto-Context Format

### Text (default)

```
Stopped: breakpoint
Function: process_items
File: app.py:42

Source:
  40 |     for item in items:
  41 |         result = transform(item)
  42>|         if result is None:
  43 |             errors.append(item)
  44 |             continue

Locals:
  item (dict) = {"name": "test", "value": 42}
  result (NoneType) = None
  errors (list[3]) = [{...}, {...}, {...}]

Stack:
  #0 process_items at app.py:42
  #1 main at app.py:15
  #2 <module> at run.py:3

Output:
  Processing item: test
  Warning: transform returned None
```

### JSON (`--json` flag)

```json
{
  "status": "stopped",
  "reason": "breakpoint",
  "location": {
    "file": "app.py",
    "line": 42,
    "function": "process_items"
  },
  "source": [
    {
      "line": 40,
      "text": "    for item in items:"
    },
    {
      "line": 42,
      "text": "        if result is None:",
      "current": true
    }
  ],
  "locals": [
    {
      "name": "item",
      "type": "dict",
      "value": "{\"name\": \"test\"}",
      "len": 2
    }
  ],
  "stack": [
    {
      "frame": 0,
      "function": "process_items",
      "file": "app.py",
      "line": 42
    }
  ],
  "output": "Processing item: test\nWarning: transform returned None\n"
}
```

### Truncation Rules

- String values: max 200 chars
- Collections: max 5 items preview + total count
- Stack: max 20 frames
- Source: 5 lines (2 before, current, 2 after)
- Output: last 200 lines since previous stop (bounded at write time to prevent OOM)
