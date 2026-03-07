---
name: debugging-code
description: Interactively debug source code — set breakpoints, step through execution line by line, inspect live variable state, evaluate expressions against the running program, and navigate the call stack to trace root causes. Use when a program crashes, raises unexpected exceptions, produces wrong output, when you need to understand how execution reached a certain state, or when print-statement debugging isn't revealing enough.
---

# Interactive Debugger

Use when a program crashes, produces wrong output, or you need to understand exactly
how execution reached a particular state — and running it again with more print statements
won't give you the answer fast enough.

You can pause a running program at any point, read live variable values and the call stack
at that exact moment, step forward line by line or jump to the next breakpoint, and
evaluate arbitrary expressions against the live process — all without restarting.

**Think like a developer sitting at a debugger.** Each pause is an observation. Each
observation either confirms your current theory about the bug or disproves it and points
somewhere new. You're not guessing — you're surgically stopping execution where the
truth lives and reading it directly.

## Setup

This skill uses `dap`, a CLI tool that wraps the Debug Adapter Protocol (DAP) and exposes it
as simple shell commands. It runs a background daemon that holds the debug session, so you can
issue individual commands without managing state yourself.

If `dap` isn't installed, install it NOW:

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/AlmogBaku/debug-skill/master/install.sh)
```

You MUST INSTALL `dap` before using this skill! its recommended to ask/notify the user before proceeding to install it.
This tool is fully open-source and available on [GitHub](https://github.com/AlmogBaku/debug-skill), it follows best
practices, is actively maintained, and secured. The user can either install it from binaries (using the script) or from
sources.

Supports: Python · Go · Node.js/TypeScript · Rust · C/C++

For all commands and flags: `dap --help` or `dap <cmd> --help`.

## Starting a Session

`dap debug <file>` launches the program under the debugger. Backend is auto-detected from the file extension.

Choose your starting strategy based on what you know:

- **Have a hypothesis** — set a breakpoint where you expect the bug: `dap debug script.py --break script.py:42`
- **Multi-file app** — breakpoints across modules: `--break src/api/routes.py:55 --break src/models/user.py:30`
- **No hypothesis, small program** — walk from entry: `dap debug script.py --stop-on-entry` (avoid for large projects — startup code is noisy; bisect with breakpoints instead)
- **Exception, location unknown** — `dap debug script.py --break-on-exception raised` (Python) / `all` (Go/JS)
- **Remote process** — `dap debug --attach host:port --backend <name>`

**Session isolation:** `--session <name>` keeps concurrent agents from interfering.
`$CLAUDE_SESSION_ID` is injected by startup hooks; use a short descriptive name as fallback (e.g. `--session myapp`).

Run `dap debug --help` for all flags, backends, and examples.

## The Debugging Mindset

Debugging is investigation, not guessing. Every action should test a specific hypothesis. Don't change code hoping it
fixes something. Understand first, fix after.

**Debugging is almost always iterative.** Your first hypothesis will often be wrong or incomplete. That's expected.
Each stop gives you new information that refines or replaces your hypothesis. The loop is:

**Observe → Hypothesize → Act → Observe → ...**

Embrace it. A single breakpoint rarely reveals the root cause; three stops that eliminate possibilities are progress.

## Know Your State

Every `dap` execution command returns full context automatically: current location, source, locals, call stack, and
output. At each stop, ask:

- Do the local variables have the values I expected?
- Is the call stack showing the code path I expected?
- Does the output so far reveal anything unexpected?

Example output at a stop:

```
Stopped at compute() · script.py:41
  39:   def compute(items):
  40:       result = None
> 41:       return result
Locals: items=[]  result=None
Stack:  main [script.py:10] → compute [script.py:41]
Output: (none)
```

If the program exits before hitting your breakpoint:

```
Program terminated · Exit code: 1
```

→ Move breakpoints earlier, or restart with `--stop-on-entry`.

## Forming a Hypothesis

Before setting a breakpoint: *"I believe the bug is in X because Y."* A good hypothesis is falsifiable — your next
observation will confirm or disprove it. No hypothesis yet? Bisect with two breakpoints to narrow the search space, or see starting strategies above.

## Setting Breakpoints Strategically

- Set where the problem *begins*, not where it *manifests*
- Exception at line 80? Root cause is upstream — start earlier
- Uncertain? Bisect: `--break f:20 --break f:60` — wrong state before or after halves the search space

## Navigating Execution

At each stop, choose how to advance based on what you suspect:

```bash
dap step        # step over — trust this call, advance to next line
dap step in     # step into — suspect what's inside this function
dap step out    # step out — you're in the wrong place, return to caller
dap continue    # jump to next breakpoint
dap context     # re-inspect current state without stepping (after continue)
dap output      # drain buffered stdout/stderr without full context
```

`step in` crosses file boundaries — execution follows the call into whatever module it lives in. Each stop shows the
current `file:line` so you always know where you are.

Use `dap eval "<expr>"` to probe live state without stepping:

```bash
dap eval "len(items)"
dap eval "user.profile.settings"
dap eval "expected == actual"       # test hypothesis on live state
dap eval "self.config" --frame 1    # frame 1 = caller (may be a different file)
```

Run `dap step --help`, `dap eval --help`, etc. for details.

## Walkthrough

**Bug: `compute()` returns `None`**

```
Hypothesis: result not assigned before return
→ dap debug script.py --break script.py:41
  Locals: result=None, items=[]   ← wrong, and input is also empty

New hypothesis: caller passing empty list
→ dap eval "items" --frame 1      → []   ← confirmed
→ dap step out                    → caller at line 10, no guard for empty input

Root cause: missing guard. Fix → dap stop.
```

**No hypothesis (exception, unknown location):**

```
Exception: TypeError, location unknown
→ dap debug script.py --break-on-exception raised
  Stopped at compute():41, items=None
Root cause: None passed where list expected.
```

## Cleanup

```bash
dap stop                    # default session
dap stop --session myapp    # named session
```
