---
name: dap-setup
description: Install and configure the dap debugger CLI and its language backends (debugpy, js-debug, dlv, lldb-dap). Use this when dap is not installed, a backend is missing, or debugging fails due to a setup issue (e.g. js-debug not found, lldb-dap too old, Go developer mode disabled).
---

# Debugger Setup

This skill installs and configures `dap` and its language-specific debug adapters.
Run this before using the `debugging-code` skill if `dap` is not yet installed or a backend is missing.

## Install dap

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/AlmogBaku/debug-skill/master/install.sh)
```

Notify the user before running this — it installs a binary to `~/.local/bin`.
Verify with `dap --version`.

## Install Language Backends

Each language requires a separate debug adapter. Install only what you need.

### Python — debugpy
```bash
pip install debugpy
# or, if pip is restricted:
pip install debugpy --break-system-packages
```
Verify: `python3 -m debugpy --version`

### Node.js / TypeScript — js-debug (standalone, no VS Code needed)
```bash
VERSION=$(curl -fsSL https://api.github.com/repos/microsoft/vscode-js-debug/releases/latest | grep '"tag_name"' | sed 's/.*"v\([^"]*\)".*/\1/')
mkdir -p ~/.dap-cli/js-debug
curl -fsSL "https://github.com/microsoft/vscode-js-debug/releases/download/v${VERSION}/js-debug-dap-v${VERSION}.tar.gz" \
  | tar -xz -C ~/.dap-cli/js-debug
```
`dap` finds it automatically. Alternatively, set `DAP_JS_DEBUG_PATH` to an existing `dapDebugServer.js`.

### Go — Delve
```bash
brew install delve       # macOS
# or: go install github.com/go-delve/delve/cmd/dlv@latest
```
**macOS only:** Delve requires developer mode to attach to processes:
```bash
sudo DevToolsSecurity -enable
```

### C / C++ / Rust — lldb-dap

The Xcode Command Line Tools ship `lldb-dap` v17, which is too old. Install v18+ via Homebrew:
```bash
brew install llvm
```
`dap` automatically prefers the Homebrew version (`/opt/homebrew/opt/llvm/bin/lldb-dap`) over the system one.

On Linux:
```bash
apt install lldb
```

## Known Limitations

**Attach mode — use absolute paths for breakpoints.**
Relative paths silently fail to match. Always pass the full path:
```bash
# Wrong (silently misses):
dap debug --attach localhost:5679 --backend debugpy --break mymodule.py:42

# Correct:
dap debug --attach localhost:5679 --backend debugpy --break /abs/path/to/mymodule.py:42
```

**Conditional breakpoints are not supported.**
`--break` only accepts `file:line`. Workaround: add a temporary `if <condition>: pass` and break on that line.

**Multiprocessing / subprocess workers cannot be debugged directly.**
`dap` attaches to the main process only — breakpoints in spawned workers will never be hit and the session will hang.
Workaround: start each worker with `debugpy --listen <port>` and attach a separate `dap` session per worker.
