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

It supports debugging with a remote debugger (e.g. when the program is running in a container)
and with local debuggers (e.g. when the program is running locally).

## Starting a Session

Use `dap debug` to launch a program under the debugger:

```bash
# Single breakpoint
dap debug python script.py --break script.py:42

# Multiple breakpoints — bisect to narrow root cause
dap debug go ./cmd/server --break main.go:15 --break main.go:30

# Breakpoints across files
dap debug python app.py --break app.py:42 --break db.py:15

# No hypothesis yet — stop at program entry
dap debug python script.py --stop-on-entry

# With session isolation
dap debug python script.py --break script.py:42 --session myapp

# Attach to a remote debugger (e.g. running in a container)
dap debug --attach localhost:5678 --backend debugpy --break handler.py:15
```

**Session isolation:** `--session <name>` is optional but recommended to isolate from other concurrent agents.
`$CLAUDE_SESSION_ID` is injected by startup hooks but may be unset — use a short descriptive name as fallback
(e.g. `--session myapp`).

## What You Get Back

Every execution command (`dap debug`, `dap step`, `dap continue`) returns full context automatically.
No follow-up calls needed — read it, think, act.

```
Stopped: breakpoint
Function: process_order
File: app.py:42

Source:
   40 | total = 0
   41 | for item in items:
   42>| price = item.get_price()
   43 | total += price
   44 | return total

Locals:
  items (list) = [<Item>, <Item>]
  total (int) = 0

Stack:
  #0 process_order at app.py:42
  #1 handle_request at server.py:88
```

At each stop, ask:
- Do the local variables have the values I expected?
- Is the call stack showing the code path I expected?

If the program crashes or exits, dap returns the exit code and any buffered output instead.

## The Debugging Mindset

Debugging is investigation, not guessing. Understand first, fix after.

**Match your effort to the difficulty:**

- **Obvious bug** (clear error message, typo, off-by-one) — just fix it, no debugger needed.
- **Unclear bug** (1-2 suspects) — form 1-2 hypotheses, set breakpoints, check, fix.
- **Hard bug** (lost, bizarre, multiple systems) — stop. Think from first principles. List 3+ hypotheses ranked by likelihood. Eliminate them one by one.

Start simple. Escalate only when you're stuck. As bugs get harder, invest more in hypothesizing, exploring via the
debugger, and reasoning about what you see.

**The loop:** Hypothesize → Breakpoint → Observe → Eliminate → Fix → Verify.

## Forming Hypotheses

Before setting a breakpoint: *"I believe the bug is in X because Y."* Start with 1-2 hypotheses — that's usually enough.

If those don't pan out and you're stuck, pause. Think from first principles. Write down 3+ hypotheses ranked by
likelihood. Label them (H1, H2, H3) so you can track what each observation proves or disproves.

A good hypothesis is falsifiable — your next observation will confirm or disprove it.
No hypothesis yet? Use `--stop-on-entry` and start from the top.

## Setting Breakpoints Strategically

- Set where the problem *begins*, not where it *manifests*
- Exception at line 80? Root cause is upstream — start earlier
- Uncertain? Bisect: `--break f:20 --break f:60` — wrong state before or after halves the search space

## Navigating Execution

```bash
dap step        # step over (trust this call, advance)
dap step in     # enter this function (suspect what's inside)
dap step out    # return to caller (you're in the wrong place)
dap continue    # jump to next breakpoint
```

## Interactive Exploration While Paused

Use `dap eval "<expr>"` to probe without stepping:

```bash
dap eval "len(items)"
dap eval "user.profile.settings"
dap eval "expected == actual"       # test hypothesis on live state
dap eval "self.config" --frame 1    # inspect different stack frame
```

In interpreted languages (Python, JS), evaluate arbitrary expressions against live state — fastest way to confirm or
rule out a theory without re-running.

Use `dap context` to re-inspect current state without stepping (useful after `continue`).
Use `dap context --frame N` to view locals and source in a different stack frame.
Use `dap output` to drain buffered stdout/stderr without full context.

## Confirm or Eliminate

After each observation, map it back to your hypotheses:

- *"H1 eliminated — `items` is not empty at line 42, so it's not a loading issue."*
- *"H2 confirmed — `user.role` is `null` here, that's the cause."*

If all hypotheses are eliminated, form new ones from what you learned. Don't keep poking without a theory.

Trace backward from the anomaly: wrong output → wrong calculation → unexpected input → value set incorrectly.
Keep asking "where did this wrong value come from?" Fix at the source, not the symptom.

## Example

Bug: `process_order` returns 0 for valid orders.

Hypothesis: `items` is empty when it shouldn't be.

```bash
dap debug python app.py --break app.py:42
# Stopped at line 42. Locals show: items = [], total = 0
# H1 confirmed — items is empty.

dap eval "len(db.get_items(order_id))"
# Returns 3 — items exist in DB, not passed correctly.

dap step out
# In caller: get_items() returns items, but caller passes [] to process_order.
# Root cause found. Fix the caller. Stop the session.

dap stop
```

## Verify the Fix

After applying a fix, re-run with the debugger. Set the same breakpoints. Check that the state is correct where it was
wrong before. For simple fixes a quick sanity check is enough. For hard bugs, be thorough — run the full reproduction.

## Cleanup

```bash
dap stop                    # default session
dap stop --session myapp    # named session
```

If a command fails, or for further tool information, run `dap <cmd> --help` for exact flags.
