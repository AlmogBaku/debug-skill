package dap

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// breakpointFlag is a repeatable --break flag that parses "file:line[:condition]".
type breakpointFlag []Breakpoint

func (b *breakpointFlag) String() string {
	keys := make([]string, len(*b))
	for i, bp := range *b {
		keys[i] = bp.LocationKey()
	}
	return strings.Join(keys, ", ")
}
func (b *breakpointFlag) Set(v string) error {
	bp, err := parseBreakpointSpec(v)
	if err != nil {
		return err
	}
	*b = append(*b, bp)
	return nil
}
func (b *breakpointFlag) Type() string { return "file:line[:condition]" }

// globalFlags holds flags shared across commands.
var globalFlags struct {
	jsonOutput   bool
	socketPath   string
	session      string
	contextLines int
}

// NewRootCmd creates the cobra root command with all subcommands.
func NewRootCmd(version string) *cobra.Command {
	root := &cobra.Command{
		Use:     "dap",
		Version: version,
		Short:   "Synchronous non-interactive CLI debugger via DAP",
		Long: `dap is a CLI tool for running debuggers via the Debug Adapter Protocol (DAP).
It is designed for agents but not limited to them.

Supported languages (auto-detected from file extension):
  .py              → debugpy   (Python)
  .go              → dlv       (Go)
  .js / .ts        → js-debug  (Node.js / TypeScript)
  .rs / .c / .cpp  → lldb-dap  (Rust / C / C++)

Key concept — auto-context: every execution command (debug, continue, step)
blocks until the program stops, then returns:
  - current location (file, line, function)
  - surrounding source lines  (current line marked with ">")
  - local variables with types and values
  - call stack
  - stdout/stderr output since last stop
No separate inspection calls needed.

When the program exits instead of stopping, output is:
  Program terminated
  Exit code: <n>

Daemon: started automatically on first 'dap debug', killed by 'dap stop'.
Sockets live at ~/.dap-cli/<session>.sock.

Typical workflow:
  dap debug app.py --break app.py:42   # start, stop at breakpoint → auto-context
  dap eval "my_var"                    # inspect a value mid-session
  dap inspect data --depth 2           # expand nested objects recursively
  dap step                             # step over → auto-context
  dap continue                         # run to next breakpoint → auto-context
  dap continue --to app.py:50          # run to a specific line (temp breakpoint)
  dap pause                            # interrupt a running program
  dap threads                          # list all threads
  dap restart                          # restart, preserving breakpoint changes
  dap stop                             # kill session

Best practices:
  - Always run 'dap stop' when done to release the daemon.
  - Use --session <name> to run multiple independent debug sessions in parallel.
  - Prefer --break over --stop-on-entry: land exactly where you need.
  - Use --json for machine-readable output; key fields: location, source,
    locals, stack, output, exit_code, reason.`,
		Example: `  dap debug app.py --break app.py:42
  dap debug main.go --break main.go:8
  dap debug --attach localhost:5678 --backend debugpy`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().BoolVar(&globalFlags.jsonOutput, "json", false, "Output in JSON format")
	root.PersistentFlags().StringVar(&globalFlags.socketPath, "socket", "", "Daemon socket path (overrides --session)")
	root.PersistentFlags().StringVar(&globalFlags.session, "session", "default", "Session name (each session runs an independent daemon)")
	root.PersistentFlags().IntVar(&globalFlags.contextLines, "context-lines", 0, "Number of source lines before/after current line (0 = default of 2)")

	// Compute effective socket path: --socket overrides --session
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if globalFlags.socketPath == "" {
			globalFlags.socketPath = SessionSocketPath(globalFlags.session)
		}
		return nil
	}

	root.AddCommand(
		newDebugCmd(),
		newStopCmd(),
		newStepCmd(),
		newContinueCmd(),
		newPauseCmd(),
		newContextCmd(),
		newEvalCmd(),
		newInspectCmd(),
		newOutputCmd(),
		newThreadsCmd(),
		newThreadCmd(),
		newRestartCmd(),
		newBreakCmd(),
		newDaemonCmd(),
	)

	return root
}

// noDaemonError checks if the error is a connection failure and returns a user-friendly message.
func noDaemonError(err error) error {
	if err != nil && strings.Contains(err.Error(), "connecting to daemon") {
		return fmt.Errorf("no active debug session (run 'dap debug' first)")
	}
	return err
}

// runDaemonCommand marshals args, sends command to daemon, prints response, exits on error.
func runDaemonCommand(command string, args any) error {
	var rawArgs json.RawMessage
	if args != nil {
		rawArgs, _ = json.Marshal(args)
	}
	resp, err := SendCommand(globalFlags.socketPath, &Request{Command: command, Args: rawArgs})
	if err != nil {
		return noDaemonError(err)
	}
	fmt.Print(FormatResponse(resp, globalFlags.jsonOutput))
	if resp.Status == "error" {
		os.Exit(1)
	}
	return nil
}

// addBreakpointFlags registers --break, --remove-break, --break-on-exception flags on a command.
func addBreakpointFlags(cmd *cobra.Command, breaks, removeBreaks *breakpointFlag, exceptionFilters *[]string) {
	cmd.Flags().Var(breaks, "break", "Add a breakpoint (repeatable: --break a.py:10 or --break \"a.py:10:x > 5\")")
	cmd.Flags().Var(removeBreaks, "remove-break", "Remove a breakpoint by location (repeatable: --remove-break a.py:10)")
	cmd.Flags().StringArrayVar(exceptionFilters, "break-on-exception", nil,
		"Add exception breakpoint filters (repeatable, merged with existing).\n"+
			"Filter IDs are backend-specific (see 'dap debug --help').")
}

// breakpointUpdatesFromFlags builds a BreakpointUpdates from CLI flag values.
func breakpointUpdatesFromFlags(breaks, removeBreaks breakpointFlag, exceptionFilters []string) BreakpointUpdates {
	return BreakpointUpdates{
		Breaks:           []Breakpoint(breaks),
		RemoveBreaks:     []Breakpoint(removeBreaks),
		ExceptionFilters: exceptionFilters,
	}
}

// --- Commands ---

func newDebugCmd() *cobra.Command {
	var (
		breaks           breakpointFlag
		attach           string
		pid              int
		backend          string
		stopOnEntry      bool
		exceptionFilters []string
	)

	cmd := &cobra.Command{
		Use:   "debug <script> [flags]",
		Short: "Start a debug session",
		Long: `Start a debug session. Auto-starts the daemon if not already running.

Backend is auto-detected from the script extension. Override with --backend.
Use --break file:line[:condition] to set breakpoints (repeatable). Quote if condition has
spaces: --break "file:line:condition". Use --stop-on-entry to stop at the first line.
Use -- to pass arguments to the debugged program.
Use --attach to connect to an already-running remote DAP server (skips local spawn,
requires --backend).

Blocks until the program hits a breakpoint or exits, then returns auto-context.`,
		Example: `  dap debug app.py --break app.py:42
  dap debug app.py --break "app.py:42:x > 5"
  dap debug app.py --break app.py:10 --break app.py:20
  dap debug main.go --break main.go:8
  dap debug server.js --break server.js:15
  dap debug hello.rs --stop-on-entry
  dap debug app.py -- --config prod.yaml --verbose
  dap debug --attach localhost:5678 --backend debugpy --break handler.py:15
  dap debug --pid 12345 --backend debugpy   # attach to running process`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && attach == "" && pid == 0 {
				return fmt.Errorf("script path, --attach, or --pid required")
			}

			socketPath, err := EnsureDaemon(globalFlags.socketPath)
			if err != nil {
				return err
			}

			debugArgs := DebugArgs{
				Breaks:           []Breakpoint(breaks),
				StopOnEntry:      stopOnEntry,
				Attach:           attach,
				PID:              pid,
				Backend:          backend,
				ExceptionFilters: exceptionFilters,
				ContextLines:     globalFlags.contextLines,
			}
			if len(args) > 0 {
				debugArgs.Script = args[0]
			}

			// Capture program args after --
			if dashIdx := cmd.ArgsLenAtDash(); dashIdx >= 0 {
				allArgs := cmd.Flags().Args()
				if dashIdx < len(allArgs) {
					debugArgs.ProgramArgs = allArgs[dashIdx:]
				}
			}

			rawArgs, _ := json.Marshal(debugArgs)
			resp, err := SendCommand(socketPath, &Request{Command: "debug", Args: rawArgs})
			if err != nil {
				return err
			}
			fmt.Print(FormatResponse(resp, globalFlags.jsonOutput))
			if resp.Status == "error" {
				os.Exit(1)
			}
			return nil
		},
	}

	cmd.Flags().Var(&breaks, "break", "Add a breakpoint (repeatable: --break a.py:10 or --break \"a.py:10:x > 5\")")
	cmd.Flags().StringVar(&attach, "attach", "", "Attach to remote debugger at host:port")
	cmd.Flags().IntVar(&pid, "pid", 0, "Attach to a running process by PID (requires --backend)")
	cmd.Flags().StringVar(&backend, "backend", "", "Debugger backend (debugpy, dlv, js-debug, lldb-dap); auto-detected from file extension")
	cmd.Flags().BoolVar(&stopOnEntry, "stop-on-entry", false, "Stop at first line")
	cmd.Flags().StringArrayVar(&exceptionFilters, "break-on-exception", nil,
		"Stop on exception; repeatable (e.g. --break-on-exception raised).\n"+
			"Filter IDs are backend-specific:\n"+
			"  debugpy (Python): raised, uncaught, userUnhandled\n"+
			"  dlv (Go):         all, uncaught\n"+
			"  js-debug (Node):  all, uncaught\n"+
			"  lldb-dap:         on-throw, on-catch")

	return cmd
}

func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "End the debug session and kill the daemon",
		Long: `End the debug session. Kills the debug adapter and daemon.
Safe to call even if no session is active.`,
		Example: `  dap stop
  dap stop --session agent1`,
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := SendCommand(globalFlags.socketPath, &Request{Command: "stop"})
			if err != nil {
				// If daemon is not running, that's fine
				if strings.Contains(err.Error(), "connecting to daemon") {
					fmt.Println("No active debug session")
					return nil
				}
				return err
			}
			fmt.Print(FormatResponse(resp, globalFlags.jsonOutput))
			return nil
		},
	}
}

func newStepCmd() *cobra.Command {
	var (
		breaks           breakpointFlag
		removeBreaks     breakpointFlag
		exceptionFilters []string
	)

	cmd := &cobra.Command{
		Use:   "step [in|out|over]",
		Short: "Step through code (default: over)",
		Long: `Step through code. Blocks until stopped, then returns auto-context
(location, source, locals, stack, output).

Modes:
  over  step over function calls (default)
  in    step into the next function call
  out   step out of the current function

Optionally update breakpoints before stepping (same flags as 'continue').`,
		Example: `  dap step           # step over (default)
  dap step in        # step into function
  dap step out       # step out of function
  dap step --break app.py:42   # add breakpoint, then step`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mode := "over"
			if len(args) > 0 {
				mode = args[0]
			}
			return runDaemonCommand("step", StepArgs{
				Mode:              mode,
				ContextLines:      globalFlags.contextLines,
				BreakpointUpdates: breakpointUpdatesFromFlags(breaks, removeBreaks, exceptionFilters),
			})
		},
	}
	addBreakpointFlags(cmd, &breaks, &removeBreaks, &exceptionFilters)
	return cmd
}

func newContinueCmd() *cobra.Command {
	var (
		breaks           breakpointFlag
		removeBreaks     breakpointFlag
		exceptionFilters []string
		continueTo       string
	)

	cmd := &cobra.Command{
		Use:   "continue",
		Short: "Resume execution until next breakpoint or exit",
		Long: `Resume execution until the next breakpoint or program exit.
Blocks until stopped, then returns auto-context.
If the program exits, prints "Program terminated" and the exit code.

Optionally add or remove breakpoints before continuing:
  --break adds breakpoints (additive, merged with existing)
  --remove-break removes specific breakpoints
  --break-on-exception adds exception breakpoint filters (merged with existing)
  --to sets a temporary breakpoint (auto-removed after stop)`,
		Example: `  dap continue
  dap continue --break app.py:42              # add a breakpoint and continue
  dap continue --break "app.py:42:x > 5"      # conditional breakpoint
  dap continue --remove-break app.py:10       # remove a breakpoint and continue
  dap continue --break-on-exception raised    # set exception breakpoints and continue
  dap continue --to app.py:50                 # run to file:line (temp breakpoint)
  dap continue --session worker               # resume in a named session
  dap continue --json                         # machine-readable output`,
		RunE: func(cmd *cobra.Command, args []string) error {
			contArgs := ContinueArgs{
				ContextLines:      globalFlags.contextLines,
				BreakpointUpdates: breakpointUpdatesFromFlags(breaks, removeBreaks, exceptionFilters),
			}
			if continueTo != "" {
				bp, err := parseBreakpointSpec(continueTo)
				if err != nil {
					return err
				}
				contArgs.ContinueTo = &bp
			}
			return runDaemonCommand("continue", contArgs)
		},
	}
	cmd.Flags().StringVar(&continueTo, "to", "", "Run to file:line (temporary breakpoint, auto-removed after stop)")
	addBreakpointFlags(cmd, &breaks, &removeBreaks, &exceptionFilters)
	return cmd
}

func newPauseCmd() *cobra.Command {
	var (
		breaks           breakpointFlag
		removeBreaks     breakpointFlag
		exceptionFilters []string
	)

	cmd := &cobra.Command{
		Use:   "pause",
		Short: "Pause a running program",
		Long: `Pause a running program. Sends a pause request and returns immediately.
The blocking command (continue/step/debug) that started execution will receive
the stop event and return auto-context.

Optionally update breakpoints before pausing (same flags as 'continue').`,
		Example: `  dap pause
  dap pause --break app.py:42   # add breakpoint, then pause`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonCommand("pause", PauseArgs{
				BreakpointUpdates: breakpointUpdatesFromFlags(breaks, removeBreaks, exceptionFilters),
			})
		},
	}
	addBreakpointFlags(cmd, &breaks, &removeBreaks, &exceptionFilters)
	return cmd
}

func newContextCmd() *cobra.Command {
	var (
		frame            int
		breaks           breakpointFlag
		removeBreaks     breakpointFlag
		exceptionFilters []string
	)

	cmd := &cobra.Command{
		Use:   "context",
		Short: "Re-fetch full context without stepping",
		Long: `Re-fetch the current auto-context without stepping: location, source snippet,
local variables, call stack, and buffered output since last stop.
Use --frame to inspect a different stack frame (0 = innermost, default).

Optionally update breakpoints (same flags as 'continue').`,
		Example: `  dap context
  dap context --frame 2   # inspect caller's frame
  dap context --break app.py:42   # add breakpoint and re-fetch context`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonCommand("context", ContextArgs{
				Frame:             frame,
				ContextLines:      globalFlags.contextLines,
				BreakpointUpdates: breakpointUpdatesFromFlags(breaks, removeBreaks, exceptionFilters),
			})
		},
	}
	cmd.Flags().IntVar(&frame, "frame", 0, "Stack frame to inspect")
	addBreakpointFlags(cmd, &breaks, &removeBreaks, &exceptionFilters)
	return cmd
}

func newEvalCmd() *cobra.Command {
	var (
		frame            int
		breaks           breakpointFlag
		removeBreaks     breakpointFlag
		exceptionFilters []string
	)

	cmd := &cobra.Command{
		Use:   "eval <expression>",
		Short: "Evaluate an expression",
		Long: `Evaluate an expression in the current (or specified) stack frame.
Use --frame to evaluate in a parent frame's scope.

Optionally update breakpoints (same flags as 'continue').`,
		Example: `  dap eval "len(items)"
  dap eval "x + y"
  dap eval "self.config" --frame 1`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonCommand("eval", EvalArgs{
				Expression:        args[0],
				Frame:             frame,
				BreakpointUpdates: breakpointUpdatesFromFlags(breaks, removeBreaks, exceptionFilters),
			})
		},
	}
	cmd.Flags().IntVar(&frame, "frame", 0, "Stack frame for evaluation context")
	addBreakpointFlags(cmd, &breaks, &removeBreaks, &exceptionFilters)
	return cmd
}

func newInspectCmd() *cobra.Command {
	var (
		depth int
		frame int
	)

	cmd := &cobra.Command{
		Use:   "inspect <variable>",
		Short: "Inspect a variable (expand nested objects)",
		Long: `Inspect a variable by name, recursively expanding nested objects.
Searches local variables first, then all scopes (globals, etc.).
Use --depth to control expansion depth (default 1, max 5).
Use --frame to inspect in a parent stack frame.`,
		Example: `  dap inspect data
  dap inspect data --depth 2
  dap inspect self --frame 1 --depth 3`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonCommand("inspect", InspectArgs{
				Variable: args[0],
				Depth:    depth,
				Frame:    frame,
			})
		},
	}
	cmd.Flags().IntVar(&depth, "depth", 1, "Expansion depth (max 5)")
	cmd.Flags().IntVar(&frame, "frame", 0, "Stack frame to inspect")
	return cmd
}

func newOutputCmd() *cobra.Command {
	var (
		breaks           breakpointFlag
		removeBreaks     breakpointFlag
		exceptionFilters []string
	)

	cmd := &cobra.Command{
		Use:   "output",
		Short: "Drain and print buffered program output (stdout/stderr) since last stop",
		Long: `Drain and print buffered stdout/stderr since the last stop. Clears the buffer.
Use when the program is running (between 'continue' and next breakpoint), or to
check output without re-fetching full context.

Optionally update breakpoints (same flags as 'continue').`,
		Example: `  dap output
  dap output --json               # machine-readable output
  dap output --session worker`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonCommand("output", OutputArgs{
				BreakpointUpdates: breakpointUpdatesFromFlags(breaks, removeBreaks, exceptionFilters),
			})
		},
	}
	addBreakpointFlags(cmd, &breaks, &removeBreaks, &exceptionFilters)
	return cmd
}

func newThreadsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "threads",
		Short: "List all threads",
		Long: `List all threads in the debugged program.
The current thread is marked with *.`,
		Example: `  dap threads
  dap threads --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonCommand("threads", nil)
		},
	}
}

func newThreadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "thread <id>",
		Short: "Switch to a different thread",
		Long: `Switch to a different thread by ID and return its context.
Use 'dap threads' to list available thread IDs.`,
		Example: `  dap thread 2
  dap thread 1 --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.Atoi(args[0])
			if err != nil {
				return fmt.Errorf("invalid thread ID %q: must be a number", args[0])
			}
			return runDaemonCommand("thread", ThreadArgs{
				ThreadID:     id,
				ContextLines: globalFlags.contextLines,
			})
		},
	}
}

func newRestartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Restart the debug session preserving breakpoints",
		Long: `Restart the debug session using the same arguments as the last 'dap debug' call.
Kills the current session and starts a new one. Any breakpoints added or removed
mid-session (via 'dap break add/remove') are preserved.`,
		Example: `  dap restart
  dap restart --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonCommand("restart", nil)
		},
	}
}

func newBreakCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "break",
		Short: "Manage breakpoints",
		Long: `Manage breakpoints in the current debug session.
Use subcommands to list, add, remove, or clear breakpoints.`,
	}

	// break list
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List all breakpoints and exception filters",
		Example: `  dap break list
  dap break list --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonCommand("break_list", nil)
		},
	}

	// break add
	var (
		addBreaks           breakpointFlag
		addExceptionFilters []string
	)
	addCmd := &cobra.Command{
		Use:   "add <file:line[:condition]> [...]",
		Short: "Add breakpoints",
		Long: `Add one or more breakpoints. Breakpoints are specified as positional
arguments (file:line or "file:line:condition") or via --break-on-exception for exception filters.
Quote specs with conditions: dap break add "app.py:42:x > 5"`,
		Example: `  dap break add app.py:42
  dap break add "app.py:42:x > 5"
  dap break add app.py:10 app.py:20
  dap break add --break-on-exception raised`,
		RunE: func(cmd *cobra.Command, args []string) error {
			allBreaks := make([]Breakpoint, len(addBreaks))
			copy(allBreaks, addBreaks)
			for _, a := range args {
				bp, err := parseBreakpointSpec(a)
				if err != nil {
					return err
				}
				allBreaks = append(allBreaks, bp)
			}
			if len(allBreaks) == 0 && len(addExceptionFilters) == 0 {
				return fmt.Errorf("specify at least one breakpoint (file:line[:condition]) or --break-on-exception")
			}
			return runDaemonCommand("break_add", BreakAddArgs{
				Breaks:           allBreaks,
				ExceptionFilters: addExceptionFilters,
			})
		},
	}
	addCmd.Flags().Var(&addBreaks, "break", "Add a breakpoint (repeatable, alternative to positional args: --break \"a.py:10:x > 5\")")
	addCmd.Flags().StringArrayVar(&addExceptionFilters, "break-on-exception", nil,
		"Add exception breakpoint filters (repeatable, merged with existing). Filter IDs are backend-specific (see 'dap debug --help').")

	// break remove
	var (
		rmBreaks           breakpointFlag
		rmExceptionFilters []string
	)
	removeCmd := &cobra.Command{
		Use:     "remove <file:line> [file:line...]",
		Aliases: []string{"rm"},
		Short:   "Remove breakpoints",
		Long: `Remove one or more breakpoints by location. Breakpoints are specified as positional
arguments (file:line) or via --break-on-exception for exception filters.
Condition is ignored for removal — only file:line matters.`,
		Example: `  dap break remove app.py:42
  dap break remove app.py:10 app.py:20
  dap break remove --break-on-exception raised`,
		RunE: func(cmd *cobra.Command, args []string) error {
			allBreaks := make([]Breakpoint, len(rmBreaks))
			copy(allBreaks, rmBreaks)
			for _, a := range args {
				bp, err := parseBreakpointSpec(a)
				if err != nil {
					return err
				}
				allBreaks = append(allBreaks, bp)
			}
			if len(allBreaks) == 0 && len(rmExceptionFilters) == 0 {
				return fmt.Errorf("specify at least one breakpoint (file:line) or --break-on-exception")
			}
			return runDaemonCommand("break_remove", BreakRemoveArgs{
				Breaks:           allBreaks,
				ExceptionFilters: rmExceptionFilters,
			})
		},
	}
	removeCmd.Flags().Var(&rmBreaks, "break", "Remove a breakpoint (repeatable, alternative to positional args)")
	removeCmd.Flags().StringArrayVar(&rmExceptionFilters, "break-on-exception", nil,
		"Remove exception filter (repeatable). Filter IDs are backend-specific (see 'dap debug --help').")

	// break clear
	clearCmd := &cobra.Command{
		Use:     "clear",
		Short:   "Remove all breakpoints and exception filters",
		Example: `  dap break clear`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonCommand("break_clear", nil)
		},
	}

	cmd.AddCommand(listCmd, addCmd, removeCmd, clearCmd)
	return cmd
}

// newDaemonCmd creates the hidden __daemon subcommand used for auto-starting.
func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "__daemon",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			StartDaemon(globalFlags.socketPath)
			return nil
		},
	}
	return cmd
}
