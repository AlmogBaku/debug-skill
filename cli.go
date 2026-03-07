package dap

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// breakpointFlag is a repeatable --break flag that displays as "file:line" in help.
type breakpointFlag []string

func (b *breakpointFlag) String() string  { return strings.Join(*b, ", ") }
func (b *breakpointFlag) Set(v string) error { *b = append(*b, v); return nil }
func (b *breakpointFlag) Type() string    { return "file:line" }

// globalFlags holds flags shared across commands.
var globalFlags struct {
	jsonOutput bool
	socketPath string
	session    string
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
  dap step                             # step over → auto-context
  dap continue                         # run to next breakpoint → auto-context
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
		newContextCmd(),
		newEvalCmd(),
		newOutputCmd(),
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

// --- Commands ---

func newDebugCmd() *cobra.Command {
	var (
		breaks           breakpointFlag
		attach           string
		backend          string
		stopOnEntry      bool
		exceptionFilters []string
	)

	cmd := &cobra.Command{
		Use:   "debug <script> [flags]",
		Short: "Start a debug session",
		Long: `Start a debug session. Auto-starts the daemon if not already running.

Backend is auto-detected from the script extension. Override with --backend.
Use --break file:line to set breakpoints (repeatable). Use --stop-on-entry to stop at the first line.
Use -- to pass arguments to the debugged program.
Use --attach to connect to an already-running remote DAP server (skips local spawn,
requires --backend).

Blocks until the program hits a breakpoint or exits, then returns auto-context.`,
		Example: `  dap debug app.py --break app.py:42
  dap debug app.py --break app.py:10 --break app.py:20
  dap debug main.go --break main.go:8
  dap debug server.js --break server.js:15
  dap debug hello.rs --stop-on-entry
  dap debug app.py -- --config prod.yaml --verbose
  dap debug --attach localhost:5678 --backend debugpy --break handler.py:15`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && attach == "" {
				return fmt.Errorf("script path or --attach required")
			}

			socketPath, err := EnsureDaemon(globalFlags.socketPath)
			if err != nil {
				return err
			}

			debugArgs := DebugArgs{
				Breaks:           []string(breaks),
				StopOnEntry:      stopOnEntry,
				Attach:           attach,
				Backend:          backend,
				ExceptionFilters: exceptionFilters,
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

	cmd.Flags().Var(&breaks, "break", "Add a breakpoint (repeatable: --break a.py:10 --break b.py:20)")
	cmd.Flags().StringVar(&attach, "attach", "", "Attach to remote debugger at host:port")
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
	cmd := &cobra.Command{
		Use:   "step [in|out|over]",
		Short: "Step through code (default: over)",
		Long: `Step through code. Blocks until stopped, then returns auto-context
(location, source, locals, stack, output).

Modes:
  over  step over function calls (default)
  in    step into the next function call
  out   step out of the current function`,
		Example: `  dap step           # step over (default)
  dap step in        # step into function
  dap step out       # step out of function`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mode := "over"
			if len(args) > 0 {
				mode = args[0]
			}

			rawArgs, _ := json.Marshal(StepArgs{Mode: mode})
			resp, err := SendCommand(globalFlags.socketPath, &Request{Command: "step", Args: rawArgs})
			if err != nil {
				return noDaemonError(err)
			}
			fmt.Print(FormatResponse(resp, globalFlags.jsonOutput))
			if resp.Status == "error" {
				os.Exit(1)
			}
			return nil
		},
	}
	return cmd
}

func newContinueCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "continue",
		Short: "Resume execution until next breakpoint or exit",
		Long: `Resume execution until the next breakpoint or program exit.
Blocks until stopped, then returns auto-context.
If the program exits, prints "Program terminated" and the exit code.`,
		Example: `  dap continue
  dap continue --session worker   # resume in a named session
  dap continue --json             # machine-readable output`,
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := SendCommand(globalFlags.socketPath, &Request{Command: "continue"})
			if err != nil {
				return noDaemonError(err)
			}
			fmt.Print(FormatResponse(resp, globalFlags.jsonOutput))
			if resp.Status == "error" {
				os.Exit(1)
			}
			return nil
		},
	}
	return cmd
}

func newContextCmd() *cobra.Command {
	var frame int

	cmd := &cobra.Command{
		Use:   "context",
		Short: "Re-fetch full context without stepping",
		Long: `Re-fetch the current auto-context without stepping: location, source snippet,
local variables, call stack, and buffered output since last stop.
Use --frame to inspect a different stack frame (0 = innermost, default).`,
		Example: `  dap context
  dap context --frame 2   # inspect caller's frame`,
		RunE: func(cmd *cobra.Command, args []string) error {
			rawArgs, _ := json.Marshal(ContextArgs{Frame: frame})
			resp, err := SendCommand(globalFlags.socketPath, &Request{Command: "context", Args: rawArgs})
			if err != nil {
				return noDaemonError(err)
			}
			fmt.Print(FormatResponse(resp, globalFlags.jsonOutput))
			if resp.Status == "error" {
				os.Exit(1)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&frame, "frame", 0, "Stack frame to inspect")
	return cmd
}

func newEvalCmd() *cobra.Command {
	var frame int

	cmd := &cobra.Command{
		Use:   "eval <expression>",
		Short: "Evaluate an expression",
		Long: `Evaluate an expression in the current (or specified) stack frame.
Use --frame to evaluate in a parent frame's scope.`,
		Example: `  dap eval "len(items)"
  dap eval "x + y"
  dap eval "self.config" --frame 1`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rawArgs, _ := json.Marshal(EvalArgs{Expression: args[0], Frame: frame})
			resp, err := SendCommand(globalFlags.socketPath, &Request{Command: "eval", Args: rawArgs})
			if err != nil {
				return noDaemonError(err)
			}
			fmt.Print(FormatResponse(resp, globalFlags.jsonOutput))
			if resp.Status == "error" {
				os.Exit(1)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&frame, "frame", 0, "Stack frame for evaluation context")
	return cmd
}

func newOutputCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "output",
		Short: "Drain and print buffered program output (stdout/stderr) since last stop",
		Long: `Drain and print buffered stdout/stderr since the last stop. Clears the buffer.
Use when the program is running (between 'continue' and next breakpoint), or to
check output without re-fetching full context.`,
		Example: `  dap output
  dap output --json               # machine-readable output
  dap output --session worker`,
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := SendCommand(globalFlags.socketPath, &Request{Command: "output"})
			if err != nil {
				return noDaemonError(err)
			}
			fmt.Print(FormatResponse(resp, globalFlags.jsonOutput))
			if resp.Status == "error" {
				os.Exit(1)
			}
			return nil
		},
	}
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
