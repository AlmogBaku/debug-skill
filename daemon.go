package dap

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	godap "github.com/google/go-dap"
)

// DefaultSocketDir is the directory for the daemon socket.
var DefaultSocketDir = filepath.Join(os.Getenv("HOME"), ".dap-cli")

// SessionSocketPath returns the socket path for a named session.
func SessionSocketPath(name string) string {
	return filepath.Join(DefaultSocketDir, name+".sock")
}

// DefaultSocketPath returns the default socket path (session "default").
func DefaultSocketPath() string {
	return SessionSocketPath("default")
}

// Daemon holds the stateful debug session.
type Daemon struct {
	client   *DAPClient
	backend  Backend
	adapterCmd *exec.Cmd

	// Async event dispatch
	expectCh chan godap.Message

	// Output buffer (bounded at write time)
	mu            sync.Mutex
	outputLines   []string       // complete lines, capped at maxOutputLines
	outputPartial strings.Builder // last incomplete line (no \n yet)

	// Session state
	threadID      int
	frameID       int
	captureOutput bool // only capture output after first stop

	// Cleanup function for temp binaries (e.g. Go, Rust compilation)
	cleanupFn func()

	// Adapter address and config for child session creation (js-debug multi-session)
	adapterAddr            string
	sessionBreaks          []string // stored "file:line" breakpoints for child session re-init
	sessionExceptionFilters []string // stored exception filter IDs for child session re-init

	// Socket
	listener net.Listener
	socketPath string

	// Idle timeout
	idleTimer *time.Timer
}

const maxOutputLines = 200
const defaultIdleTimeout = 10 * time.Minute

func idleTimeout() time.Duration {
	if v := os.Getenv("DAP_IDLE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return defaultIdleTimeout
}

// appendOutput appends text to the bounded output buffer. Called under mu.
func (d *Daemon) appendOutput(text string) {
	if d.outputPartial.Len() > 0 {
		text = d.outputPartial.String() + text
		d.outputPartial.Reset()
	}
	lines := strings.Split(text, "\n")
	d.outputLines = append(d.outputLines, lines[:len(lines)-1]...)
	if len(d.outputLines) > maxOutputLines {
		trimmed := make([]string, maxOutputLines)
		copy(trimmed, d.outputLines[len(d.outputLines)-maxOutputLines:])
		d.outputLines = trimmed
	}
	d.outputPartial.WriteString(lines[len(lines)-1])
}

// outputString returns buffered output as a string and clears the buffer. Called under mu.
func (d *Daemon) outputString() string {
	lines := d.outputLines
	if d.outputPartial.Len() > 0 {
		lines = append(lines, d.outputPartial.String())
	}
	d.outputLines = nil
	d.outputPartial.Reset()
	return strings.Join(lines, "\n")
}

// readExpected reads the next expected message from the reader goroutine.
func (d *Daemon) readExpected() (godap.Message, error) {
	msg, ok := <-d.expectCh
	if !ok {
		return nil, io.EOF
	}
	return msg, nil
}

// readLoop continuously reads DAP messages and dispatches them.
//
// Event dispatch (whitelist):
//
//	ReadMessage()
//	  ├─ OutputEvent ──► buffer stdout/stderr (cap at maxOutputLines)
//	  ├─ StoppedEvent ──────┐
//	  ├─ TerminatedEvent ───┤
//	  ├─ InitializedEvent ──┼──► expectCh (consumed by waitForStopped, handleEval, etc.)
//	  ├─ ExitedEvent ───────┤
//	  ├─ ResponseMessage ───┘
//	  └─ default ──► DROP (ProcessEvent, ThreadEvent, ModuleEvent, etc.)
func (d *Daemon) readLoop() {
	defer close(d.expectCh)
	for {
		msg, err := d.client.ReadMessage()
		if err != nil {
			return
		}
		switch m := msg.(type) {
		case *godap.OutputEvent:
			if d.captureOutput {
				cat := m.Body.Category
				if cat == "stdout" || cat == "stderr" {
					d.mu.Lock()
					d.appendOutput(m.Body.Output)
					d.mu.Unlock()
				}
			}
		case *godap.StoppedEvent, *godap.TerminatedEvent, *godap.InitializedEvent, *godap.ExitedEvent:
			d.expectCh <- msg
		case godap.ResponseMessage:
			d.expectCh <- msg
		case *godap.StartDebuggingRequest:
			// Reverse request from js-debug to create a child debug session.
			// js-debug uses multi-session: parent = session manager, child = actual debuggee.
			// We respond with success, create a new connection for the child session,
			// do full init (initialize → launch → breakpoints → configDone),
			// and swap d.client so readLoop continues on the child.
			resp := &godap.StartDebuggingResponse{}
			resp.Type = "response"
			resp.Command = m.Command
			resp.RequestSeq = m.Seq
			resp.Seq = 0
			resp.Success = true
			d.client.send(resp)

			if d.adapterAddr == "" {
				log.Printf("startDebugging: no adapter address for child session")
				continue
			}
			if err := d.setupChildSession(m.Arguments.Configuration); err != nil {
				log.Printf("startDebugging: %v", err)
				continue
			}
		default:
			// Silently drop: ProcessEvent, ThreadEvent, ModuleEvent, ContinuedEvent, etc.
		}
	}
}

// waitForStopped waits for a StoppedEvent or TerminatedEvent, skipping responses and other events.
// ExitedEvent → captures exit code, continues waiting for TerminatedEvent.
func (d *Daemon) waitForStopped() (*ContextResult, error) {
	var exitCode *int
	for {
		msg, err := d.readExpected()
		if err != nil {
			return nil, fmt.Errorf("debug adapter disconnected unexpectedly: %w", err)
		}
		switch m := msg.(type) {
		case *godap.StoppedEvent:
			d.threadID = resolveThreadID(m.Body.ThreadId)
			ctx, err := getFullContext(d, d.threadID, 0)
			if err != nil {
				return nil, fmt.Errorf("getting context: %w", err)
			}
			ctx.Reason = m.Body.Reason
			return ctx, nil
		case *godap.ExitedEvent:
			ec := m.Body.ExitCode
			exitCode = &ec
		case *godap.TerminatedEvent:
			d.mu.Lock()
			output := d.outputString()
			d.mu.Unlock()
			d.stopSession()
			return &ContextResult{Output: output, ExitCode: exitCode}, nil
		case godap.ResponseMessage:
			if !m.GetResponse().Success {
				return nil, fmt.Errorf("debugger error: %s", m.GetResponse().Message)
			}
		}
	}
}

// Serve starts the daemon's Unix socket server.
func (d *Daemon) Serve(socketPath string) error {
	d.socketPath = socketPath

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(socketPath), 0700); err != nil {
		return fmt.Errorf("creating socket dir: %w", err)
	}

	// Remove stale socket
	os.Remove(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", socketPath, err)
	}
	d.listener = listener

	// Write PID file
	pidPath := socketPath + ".pid"
	os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0600)
	defer os.Remove(pidPath)

	// Cleanup on signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		d.cleanup()
		os.Exit(0)
	}()

	// Idle timeout: exit if no commands received within timeout
	d.idleTimer = time.AfterFunc(idleTimeout(), func() {
		log.Printf("daemon idle timeout, exiting")
		d.cleanup()
		os.Exit(0)
	})

	log.Printf("daemon listening on %s (pid %d)", socketPath, os.Getpid())

	for {
		conn, err := listener.Accept()
		if err != nil {
			if d.listener == nil {
				return nil // shut down
			}
			log.Printf("accept error: %v", err)
			continue
		}
		d.handleConnection(conn)
	}
}

func (d *Daemon) handleConnection(conn net.Conn) {
	defer conn.Close()

	var req Request
	if err := ReadIPC(conn, &req); err != nil {
		log.Printf("read request: %v", err)
		return
	}

	resp := d.dispatch(req)

	if err := WriteIPC(conn, resp); err != nil {
		log.Printf("write response: %v", err)
	}
}

func (d *Daemon) dispatch(req Request) *Response {
	if d.idleTimer != nil {
		d.idleTimer.Reset(idleTimeout())
	}
	switch req.Command {
	case "debug":
		return d.handleDebug(req.Args)
	case "step":
		return d.handleStep(req.Args)
	case "continue":
		return d.handleContinue(req.Args)
	case "context":
		return d.handleContext(req.Args)
	case "eval":
		return d.handleEval(req.Args)
	case "output":
		return d.handleOutput()
	case "stop":
		return d.handleStop()
	case "ping":
		return &Response{Status: "ok"}
	default:
		return &Response{Status: "error", Error: fmt.Sprintf("unknown command: %s", req.Command)}
	}
}

func (d *Daemon) handleDebug(rawArgs json.RawMessage) *Response {
	var args DebugArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return &Response{Status: "error", Error: fmt.Sprintf("invalid args: %v", err)}
	}

	// Select backend
	var backend Backend
	if args.Backend != "" {
		var err error
		backend, err = GetBackendByName(args.Backend)
		if err != nil {
			return &Response{Status: "error", Error: err.Error()}
		}
	} else if args.Script != "" {
		backend = DetectBackend(args.Script)
	} else if args.Attach != "" {
		return &Response{Status: "error", Error: "backend required for remote attach (e.g. --backend debugpy)"}
	} else {
		return &Response{Status: "error", Error: "script path or --attach required"}
	}
	d.backend = backend

	isRemote := args.Attach != ""

	if isRemote {
		// Connect directly to the remote DAP server
		client, err := newDAPClient(args.Attach)
		if err != nil {
			return &Response{Status: "error", Error: fmt.Sprintf("connecting to %s: %v", args.Attach, err)}
		}
		d.client = client
		d.expectCh = make(chan godap.Message, 64)
		go d.readLoop()

		if err := d.initializeDAP(backend); err != nil {
			return &Response{Status: "error", Error: err.Error()}
		}

		host, portStr, splitErr := net.SplitHostPort(args.Attach)
		if splitErr != nil {
			d.stopSession()
			return &Response{Status: "error", Error: fmt.Sprintf("invalid attach address %q: %v", args.Attach, splitErr)}
		}
		remotePort, _ := strconv.Atoi(portStr)
		attachArgs, err := backend.RemoteAttachArgs(host, remotePort)
		if err != nil {
			d.stopSession()
			return &Response{Status: "error", Error: fmt.Sprintf("preparing attach: %v", err)}
		}
		if err := d.client.AttachRequestWithArgs(attachArgs); err != nil {
			d.stopSession()
			return &Response{Status: "error", Error: fmt.Sprintf("attach: %v", err)}
		}
	} else {
		// Local launch
		if err := d.startAdapter(backend); err != nil {
			return &Response{Status: "error", Error: err.Error()}
		}
		if err := d.initializeDAP(backend); err != nil {
			return &Response{Status: "error", Error: err.Error()}
		}

		launchArgs, cleanupFn, err := backend.LaunchArgs(args.Script, args.StopOnEntry || len(args.Breaks) == 0, args.ProgramArgs)
		if err != nil {
			d.stopSession()
			return &Response{Status: "error", Error: fmt.Sprintf("preparing launch: %v", err)}
		}
		d.cleanupFn = cleanupFn
		if err := d.client.LaunchRequestWithArgs(launchArgs); err != nil {
			d.stopSession()
			return &Response{Status: "error", Error: fmt.Sprintf("launch: %v", err)}
		}
	}

	// Wait for initialized event. The launch/attach response may arrive before or after
	// initialized (adapter-dependent), so we consume both but only block on initialized.
	initializedReceived := false
	for !initializedReceived {
		msg, err := d.readExpected()
		if err != nil {
			d.stopSession()
			return &Response{Status: "error", Error: fmt.Sprintf("waiting for initialized: %v", err)}
		}
		switch m := msg.(type) {
		case *godap.ErrorResponse:
			errMsg := m.Message
			if m.Body.Error != nil {
				errMsg = m.Body.Error.Format
			}
			d.stopSession()
			return &Response{Status: "error", Error: fmt.Sprintf("request failed: %s", errMsg)}
		case *godap.ExitedEvent, *godap.TerminatedEvent:
			d.stopSession()
			return &Response{Status: "error", Error: "debug adapter exited during initialization — check that the program path is valid and the debugger is installed"}
		case godap.ResponseMessage:
			if !m.GetResponse().Success {
				d.stopSession()
				return &Response{Status: "error", Error: fmt.Sprintf("request failed: %s", m.GetResponse().Message)}
			}
		case *godap.InitializedEvent:
			initializedReceived = true
		}
	}

	// Send all config requests without waiting for individual responses.
	// DAP is async — responses will be consumed by waitForStopped below.
	stopOnEntry := !isRemote && (args.StopOnEntry || len(args.Breaks) == 0)
	entryBP := backend.StopOnEntryBreakpoint()
	if stopOnEntry && entryBP != "" {
		if err := d.client.SetFunctionBreakpointsRequest([]string{entryBP}); err != nil {
			d.stopSession()
			return &Response{Status: "error", Error: fmt.Sprintf("set entry breakpoint: %v", err)}
		}
	}
	d.sessionBreaks = args.Breaks
	d.sessionExceptionFilters = args.ExceptionFilters
	breaksByFile := groupBreakpoints(args.Breaks)
	for file, lines := range breaksByFile {
		if err := d.client.SetBreakpointsRequest(file, lines); err != nil {
			d.stopSession()
			return &Response{Status: "error", Error: fmt.Sprintf("set breakpoints: %v", err)}
		}
	}
	exceptionFilters := args.ExceptionFilters
	if exceptionFilters == nil {
		exceptionFilters = []string{}
	}
	if err := d.client.SetExceptionBreakpointsRequest(exceptionFilters); err != nil {
		d.stopSession()
		return &Response{Status: "error", Error: fmt.Sprintf("set exception breakpoints: %v", err)}
	}
	if err := d.client.ConfigurationDoneRequest(); err != nil {
		d.stopSession()
		return &Response{Status: "error", Error: fmt.Sprintf("finalizing debug setup: %v", err)}
	}

	// Wait for first stop. If we get an "entry" stop and have breakpoints, continue past it.
	for {
		ctx, err := d.waitForStopped()
		if err != nil {
			d.stopSession()
			return &Response{Status: "error", Error: err.Error()}
		}
		if ctx.Location == nil {
			// terminated
			return &Response{Status: "terminated", Data: ctx}
		}
		if len(args.Breaks) > 0 && !args.StopOnEntry && ctx.Reason == "entry" {
			d.mu.Lock()
			d.outputLines = nil
			d.outputPartial.Reset()
			d.mu.Unlock()
			if err := d.client.ContinueRequest(d.threadID); err != nil {
				d.stopSession()
				return &Response{Status: "error", Error: fmt.Sprintf("continue past entry: %v", err)}
			}
			continue
		}
		ctx.Output = "" // discard launch noise
		d.captureOutput = true
		return &Response{Status: "stopped", Data: ctx}
	}
}

func (d *Daemon) handleStep(rawArgs json.RawMessage) *Response {
	if d.client == nil {
		return &Response{Status: "error", Error: "no active debug session (program may have terminated) — run 'dap debug' to start a new session"}
	}

	var args StepArgs
	if rawArgs != nil {
		json.Unmarshal(rawArgs, &args)
	}
	if args.Mode == "" {
		args.Mode = "over"
	}

	threadID := resolveThreadID(d.threadID)

	switch args.Mode {
	case "over":
		if err := d.client.NextRequest(threadID); err != nil {
			return &Response{Status: "error", Error: fmt.Sprintf("step over: %v", err)}
		}
	case "in":
		if err := d.client.StepInRequest(threadID); err != nil {
			return &Response{Status: "error", Error: fmt.Sprintf("step in: %v", err)}
		}
	case "out":
		if err := d.client.StepOutRequest(threadID); err != nil {
			return &Response{Status: "error", Error: fmt.Sprintf("step out: %v", err)}
		}
	default:
		return &Response{Status: "error", Error: fmt.Sprintf("invalid step mode %q — use: in, out, over", args.Mode)}
	}

	return d.awaitStopResult()
}

func (d *Daemon) handleContinue(_ json.RawMessage) *Response {
	if d.client == nil {
		return &Response{Status: "error", Error: "no active debug session (program may have terminated) — run 'dap debug' to start a new session"}
	}

	threadID := resolveThreadID(d.threadID)

	if err := d.client.ContinueRequest(threadID); err != nil {
		return &Response{Status: "error", Error: fmt.Sprintf("continue: %v", err)}
	}

	return d.awaitStopResult()
}

func (d *Daemon) handleContext(rawArgs json.RawMessage) *Response {
	if d.client == nil {
		return &Response{Status: "error", Error: "no active debug session (program may have terminated) — run 'dap debug' to start a new session"}
	}

	var args ContextArgs
	if rawArgs != nil {
		json.Unmarshal(rawArgs, &args)
	}

	threadID := resolveThreadID(d.threadID)

	ctx, err := getFullContext(d, threadID, args.Frame)
	if err != nil {
		return &Response{Status: "error", Error: err.Error()}
	}
	return &Response{Status: "stopped", Data: ctx}
}

func (d *Daemon) handleEval(rawArgs json.RawMessage) *Response {
	if d.client == nil {
		return &Response{Status: "error", Error: "no active debug session (program may have terminated) — run 'dap debug' to start a new session"}
	}

	var args EvalArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return &Response{Status: "error", Error: fmt.Sprintf("invalid args: %v", err)}
	}

	frameID := args.Frame
	if frameID == 0 {
		frameID = d.frameID
	}

	if err := d.client.EvaluateRequest(args.Expression, frameID, "repl"); err != nil {
		return &Response{Status: "error", Error: fmt.Sprintf("evaluate: %v", err)}
	}

	for {
		msg, err := d.readExpected()
		if err != nil {
			return &Response{Status: "error", Error: fmt.Sprintf("reading eval response: %v", err)}
		}
		switch resp := msg.(type) {
		case *godap.EvaluateResponse:
			if !resp.Success {
				return &Response{Status: "error", Error: fmt.Sprintf("eval failed: %s", resp.Message)}
			}
			return &Response{
				Status: "ok",
				Data: &ContextResult{
					EvalResult: &EvalResult{
						Value: resp.Body.Result,
						Type:  resp.Body.Type,
					},
				},
			}
		case *godap.ExitedEvent, *godap.TerminatedEvent:
			return &Response{Status: "error", Error: "program terminated during evaluation"}
		case godap.ResponseMessage:
			if !resp.GetResponse().Success {
				return &Response{Status: "error", Error: fmt.Sprintf("eval failed: %s", resp.GetResponse().Message)}
			}
			return &Response{Status: "ok", Data: &ContextResult{EvalResult: &EvalResult{Value: "(no result)"}}}
		}
	}
}

func (d *Daemon) handleOutput() *Response {
	d.mu.Lock()
	output := d.outputString()
	d.mu.Unlock()
	return &Response{Status: "ok", Data: &ContextResult{Output: output}}
}

func (d *Daemon) handleStop() *Response {
	d.stopSession()
	// Schedule daemon exit after responding
	go func() {
		time.Sleep(100 * time.Millisecond)
		d.cleanup()
		os.Exit(0)
	}()
	return &Response{Status: "ok"}
}

func (d *Daemon) stopSession() {
	if d.client != nil {
		d.client.DisconnectRequest(true)
		d.client.Close()
		d.client = nil
	}
	if d.adapterCmd != nil && d.adapterCmd.Process != nil {
		d.adapterCmd.Process.Kill()
		d.adapterCmd.Wait()
		d.adapterCmd = nil
	}
	if d.cleanupFn != nil {
		d.cleanupFn()
		d.cleanupFn = nil
	}
}

func (d *Daemon) cleanup() {
	d.stopSession()
	if d.listener != nil {
		d.listener.Close()
		d.listener = nil
	}
	os.Remove(d.socketPath)
	os.Remove(d.socketPath + ".pid")
}

// startAdapter spawns the debug adapter and connects the DAP client.
func (d *Daemon) startAdapter(backend Backend) error {
	port := findFreePort()
	cmd, addr, err := backend.Spawn(fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("starting debug adapter: %w", err)
	}
	d.adapterCmd = cmd
	d.adapterAddr = addr
	client, err := newDAPClient(addr)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return fmt.Errorf("connecting to adapter: %w", err)
	}
	d.client = client
	d.expectCh = make(chan godap.Message, 64)
	go d.readLoop()
	return nil
}

// initializeDAP sends the initialize request and waits for the response.
func (d *Daemon) initializeDAP(backend Backend) error {
	if err := d.client.InitializeRequest(backend.AdapterID()); err != nil {
		d.stopSession()
		return fmt.Errorf("initialize: %w", err)
	}
	for {
		msg, err := d.readExpected()
		if err != nil {
			d.stopSession()
			return fmt.Errorf("waiting for init response: %w", err)
		}
		switch resp := msg.(type) {
		case *godap.InitializeResponse:
			if !resp.Success {
				d.stopSession()
				return fmt.Errorf("initialize failed: %s", resp.Message)
			}
			return nil
		case *godap.ExitedEvent, *godap.TerminatedEvent:
			d.stopSession()
			return fmt.Errorf("debug adapter exited during initialization")
		}
	}
}

// setupChildSession creates a child debug session for js-debug's multi-session architecture.
// It connects a new DAP client, initializes it, sends launch + breakpoints + configDone,
// and swaps d.client so readLoop continues reading from the child session.
func (d *Daemon) setupChildSession(config map[string]any) error {
	childClient, err := newDAPClient(d.adapterAddr)
	if err != nil {
		return fmt.Errorf("connecting child session: %w", err)
	}

	// Initialize child
	childClient.InitializeRequest(d.backend.AdapterID())
	for {
		cmsg, cerr := childClient.ReadMessage()
		if cerr != nil {
			childClient.Close()
			return fmt.Errorf("reading child init response: %w", cerr)
		}
		if _, ok := cmsg.(*godap.InitializeResponse); ok {
			break
		}
	}

	// Launch child with config from startDebugging
	childClient.LaunchRequestWithArgs(config)

	// Read until InitializedEvent (child may send it immediately)
	for {
		cmsg, cerr := childClient.ReadMessage()
		if cerr != nil {
			childClient.Close()
			return fmt.Errorf("reading child initialized: %w", cerr)
		}
		if _, ok := cmsg.(*godap.InitializedEvent); ok {
			break
		}
		// Skip responses (launch response may come first)
	}

	// Re-send breakpoints on child session
	breaksByFile := groupBreakpoints(d.sessionBreaks)
	for file, lines := range breaksByFile {
		childClient.SetBreakpointsRequest(file, lines)
	}
	childExceptionFilters := d.sessionExceptionFilters
	if childExceptionFilters == nil {
		childExceptionFilters = []string{}
	}
	childClient.SetExceptionBreakpointsRequest(childExceptionFilters)
	childClient.ConfigurationDoneRequest()

	// Swap to child session — readLoop continues reading from child
	d.client = childClient
	return nil
}

// resolveThreadID returns threadID if non-zero, otherwise defaults to 1.
func resolveThreadID(threadID int) int {
	if threadID == 0 {
		return 1
	}
	return threadID
}

// awaitStopResult calls waitForStopped and returns an appropriate response.
// Used by handleStep and handleContinue.
func (d *Daemon) awaitStopResult() *Response {
	ctx, err := d.waitForStopped()
	if err != nil {
		return &Response{Status: "error", Error: err.Error()}
	}
	if ctx.Location == nil {
		return &Response{Status: "terminated", Data: ctx}
	}
	return &Response{Status: "stopped", Data: ctx}
}

// --- Helpers ---

// groupBreakpoints parses "file:line" strings and groups by file.
func groupBreakpoints(breaks []string) map[string][]int {
	result := make(map[string][]int)
	for _, b := range breaks {
		parts := strings.SplitN(b, ":", 2)
		if len(parts) != 2 {
			continue
		}
		line, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}
		file := parts[0]
		// Resolve to absolute path
		if abs, err := filepath.Abs(file); err == nil {
			file = abs
		}
		result[file] = append(result[file], line)
	}
	return result
}

// findFreePort finds an available TCP port.
func findFreePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 5678 // fallback
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// StartDaemon starts the daemon process (called from __daemon subcommand).
func StartDaemon(socketPath string) {
	d := &Daemon{}
	if err := d.Serve(socketPath); err != nil {
		log.Fatalf("daemon: %v", err)
	}
}

// EnsureDaemon makes sure a daemon is running, starting one if needed.
// Returns the socket path.
func EnsureDaemon(socketPath string) (string, error) {
	// Try connecting to existing daemon
	conn, err := net.DialTimeout("unix", socketPath, 500*time.Millisecond)
	if err == nil {
		// Daemon is running, verify with ping
		WriteIPC(conn, &Request{Command: "ping"})
		var resp Response
		if err := ReadIPC(conn, &resp); err == nil && resp.Status == "ok" {
			conn.Close()
			return socketPath, nil
		}
		conn.Close()
		// Stale socket, remove it
		os.Remove(socketPath)
	}

	// Fork self as daemon
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("finding executable: %w", err)
	}

	cmd := exec.Command(exe, "__daemon", "--socket", socketPath)
	cmd.SysProcAttr = daemonSysProcAttr()
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("starting daemon: %w", err)
	}
	// Detach - don't wait for daemon
	cmd.Process.Release()

	// Wait for socket to appear
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if conn, err := net.DialTimeout("unix", socketPath, 200*time.Millisecond); err == nil {
			conn.Close()
			return socketPath, nil
		}
	}

	return "", fmt.Errorf("daemon did not start within 5s — check permissions on %s", filepath.Dir(socketPath))
}

// SendCommand sends a command to the daemon and returns the response.
func SendCommand(socketPath string, req *Request) (*Response, error) {
	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connecting to daemon: %w", err)
	}
	defer conn.Close()

	// Set a generous timeout for commands that block (debug, step, continue)
	conn.SetDeadline(time.Now().Add(5 * time.Minute))

	if err := WriteIPC(conn, req); err != nil {
		return nil, fmt.Errorf("sending command: %w", err)
	}

	var resp Response
	if err := ReadIPC(conn, &resp); err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	return &resp, nil
}
