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
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	godap "github.com/google/go-dap"
)

// defaultSocketDir returns the directory for the daemon socket.
func defaultSocketDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".dap-cli")
}

// SessionSocketPath returns the socket path for a named session.
func SessionSocketPath(name string) string {
	return filepath.Join(defaultSocketDir(), name+".sock")
}

// DefaultSocketPath returns the default socket path (session "default").
func DefaultSocketPath() string {
	return SessionSocketPath("default")
}

// Daemon holds the stateful debug session.
type Daemon struct {
	clientMu sync.Mutex // guards d.client pointer; never held during I/O
	client   *DAPClient
	backend  Backend

	adapterCmd *exec.Cmd

	// Async event dispatch
	expectCh chan godap.Message

	// Output buffer (bounded at write time)
	mu            sync.Mutex
	outputLines   []string        // complete lines, capped at maxOutputLines
	outputPartial strings.Builder // last incomplete line (no \n yet)

	// Session state
	threadID      int
	frameID       int
	frameIDs      []int // DAP frame IDs for the current stop, indexed by stack position
	captureOutput bool  // only capture output after first stop

	// Unverified breakpoint warnings, drained on next response
	breakWarnings []string

	// Requested breakpoint lines per file, for detecting line adjustments
	requestedBreakLines map[string]map[int]bool

	// Cleanup function for temp binaries (e.g. Go, Rust compilation)
	cleanupFn func()

	// Adapter address and config for child session creation (js-debug multi-session)
	adapterAddr string
	// sessionBreaks and sessionExceptionFilters are only accessed from handler methods
	// (single-threaded dispatch via Serve) — no mutex needed.
	// d.client is guarded by clientMu for pointer swaps (readLoop↔handlers); once
	// requireSession passes, handlers use d.client directly (sequential dispatch).
	sessionBreaks           []Breakpoint // stored breakpoints for child session re-init
	sessionExceptionFilters []string     // stored exception filter IDs for child session re-init

	// Socket
	listener   net.Listener
	socketPath string

	// Idle timeout
	idleTimer *time.Timer
}

const maxOutputLines = 200
const defaultIdleTimeout = 10 * time.Minute

const errNoSession = "no active debug session (program may have terminated) — run 'dap debug' to start a new session"

func errResponse(msg string) *Response {
	return &Response{Status: "error", Error: msg}
}

func errResponsef(format string, args ...any) *Response {
	return &Response{Status: "error", Error: fmt.Sprintf(format, args...)}
}

func (d *Daemon) requireSession() *Response {
	if d.getClient() == nil {
		return errResponse(errNoSession)
	}
	return nil
}

func (d *Daemon) getClient() *DAPClient {
	d.clientMu.Lock()
	defer d.clientMu.Unlock()
	return d.client
}

func (d *Daemon) setClient(c *DAPClient) {
	d.clientMu.Lock()
	d.client = c
	d.clientMu.Unlock()
}

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

// addBreakWarning appends a warning about an unverified breakpoint.
func (d *Daemon) addBreakWarning(w string) {
	d.mu.Lock()
	d.breakWarnings = append(d.breakWarnings, w)
	d.mu.Unlock()
}

// drainBreakWarnings returns accumulated warnings and clears them.
func (d *Daemon) drainBreakWarnings() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.breakWarnings) == 0 {
		return nil
	}
	w := d.breakWarnings
	d.breakWarnings = nil
	return w
}

// attachWarnings drains any pending break warnings into a response.
func (d *Daemon) attachWarnings(resp *Response) {
	warnings := d.drainBreakWarnings()
	if len(warnings) == 0 {
		return
	}
	if resp.Data == nil {
		resp.Data = &ContextResult{}
	}
	resp.Data.Warnings = warnings
}

// recordRequestedBreaks stores the requested line numbers for a file,
// so we can detect line adjustments in SetBreakpointsResponse.
func (d *Daemon) recordRequestedBreaks(file string, breaks []Breakpoint) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.requestedBreakLines == nil {
		d.requestedBreakLines = make(map[string]map[int]bool)
	}
	lines := make(map[int]bool, len(breaks))
	for _, b := range breaks {
		lines[b.Line] = true
	}
	d.requestedBreakLines[file] = lines
}

// checkBreakpointWarnings inspects a SetBreakpointsResponse for unverified
// or line-adjusted breakpoints and records warnings.
func (d *Daemon) checkBreakpointWarnings(m *godap.SetBreakpointsResponse) {
	d.mu.Lock()
	requested := d.requestedBreakLines
	d.mu.Unlock()

	for _, bp := range m.Body.Breakpoints {
		if !bp.Verified {
			w := fmt.Sprintf("breakpoint at line %d not verified", bp.Line)
			if bp.Message != "" {
				w = fmt.Sprintf("breakpoint at line %d not verified: %s", bp.Line, bp.Message)
			}
			d.addBreakWarning(w)
			continue
		}
		// Check for line adjustment: adapter moved the breakpoint to a different line
		if bp.Source != nil && requested != nil {
			file := bp.Source.Path
			if lines, ok := requested[file]; ok && !lines[bp.Line] {
				// Find which requested line was adjusted
				for reqLine := range lines {
					// Heuristic: report adjusted if this response line wasn't requested
					d.addBreakWarning(fmt.Sprintf("breakpoint at %s:%d was adjusted to line %d", filepath.Base(file), reqLine, bp.Line))
					delete(lines, reqLine)
					break
				}
			}
		}
	}
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
		case *godap.SetBreakpointsResponse:
			d.checkBreakpointWarnings(m)
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
			_ = d.client.send(resp)

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

			// Fetch exception info when stopped on an exception
			if m.Body.Reason == "exception" {
				if err := d.client.ExceptionInfoRequest(d.threadID); err == nil {
					if emsg, eerr := d.readExpected(); eerr == nil {
						if eresp, ok := emsg.(*godap.ExceptionInfoResponse); ok && eresp.Success {
							ctx.ExceptionInfo = &ExceptionInfo{
								ExceptionID: eresp.Body.ExceptionId,
								Description: eresp.Body.Description,
							}
							if eresp.Body.Details != nil {
								ctx.ExceptionInfo.Details = eresp.Body.Details.Message
							}
						}
					}
				}
			}

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
	_ = os.Remove(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", socketPath, err)
	}
	d.listener = listener

	// Write PID file
	pidPath := socketPath + ".pid"
	_ = os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0600)
	defer func() { _ = os.Remove(pidPath) }()

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
	defer func() { _ = conn.Close() }()

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
	resp := d.dispatchCommand(req)
	if resp.Status != "error" {
		d.attachWarnings(resp)
	}
	return resp
}

func (d *Daemon) dispatchCommand(req Request) *Response {
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
		return d.handleOutput(req.Args)
	case "break_list":
		return d.handleBreakList()
	case "break_add":
		return d.handleBreakAdd(req.Args)
	case "break_remove":
		return d.handleBreakRemove(req.Args)
	case "break_clear":
		return d.handleBreakClear()
	case "pause":
		return d.handlePause(req.Args)
	case "stop":
		return d.handleStop()
	case "ping":
		return &Response{Status: "ok"}
	default:
		return errResponsef("unknown command: %s", req.Command)
	}
}

func (d *Daemon) handleDebug(rawArgs json.RawMessage) *Response {
	var args DebugArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return errResponsef("invalid args: %v", err)
	}

	// Select backend
	var backend Backend
	if args.Backend != "" {
		var err error
		backend, err = GetBackendByName(args.Backend)
		if err != nil {
			return errResponse(err.Error())
		}
	} else if args.Script != "" {
		backend = DetectBackend(args.Script)
	} else if args.Attach != "" {
		return errResponse("backend required for remote attach (e.g. --backend debugpy)")
	} else {
		return errResponse("script path or --attach required")
	}
	d.backend = backend
	d.stopSession() // clean up any previous session

	isRemote := args.Attach != ""

	if isRemote {
		// Connect directly to the remote DAP server
		client, err := newDAPClient(args.Attach)
		if err != nil {
			return errResponsef("connecting to %s: %v", args.Attach, err)
		}
		d.setClient(client)
		d.expectCh = make(chan godap.Message, 64)
		go d.readLoop()

		if err := d.initializeDAP(backend); err != nil {
			return errResponse(err.Error())
		}

		host, portStr, splitErr := net.SplitHostPort(args.Attach)
		if splitErr != nil {
			d.stopSession()
			return errResponsef("invalid attach address %q: %v", args.Attach, splitErr)
		}
		remotePort, _ := strconv.Atoi(portStr)
		attachArgs, err := backend.RemoteAttachArgs(host, remotePort)
		if err != nil {
			d.stopSession()
			return errResponsef("preparing attach: %v", err)
		}
		if err := d.client.AttachRequestWithArgs(attachArgs); err != nil {
			d.stopSession()
			return errResponsef("attach: %v", err)
		}
	} else {
		// Local launch
		if err := d.startAdapter(backend); err != nil {
			return errResponse(err.Error())
		}
		if err := d.initializeDAP(backend); err != nil {
			return errResponse(err.Error())
		}

		launchArgs, cleanupFn, err := backend.LaunchArgs(args.Script, args.StopOnEntry || len(args.Breaks) == 0, args.ProgramArgs)
		if err != nil {
			d.stopSession()
			return errResponsef("preparing launch: %v", err)
		}
		d.cleanupFn = cleanupFn
		if err := d.client.LaunchRequestWithArgs(launchArgs); err != nil {
			d.stopSession()
			return errResponsef("launch: %v", err)
		}
	}

	// Wait for initialized event. The launch/attach response may arrive before or after
	// initialized (adapter-dependent), so we consume both but only block on initialized.
	initializedReceived := false
	for !initializedReceived {
		msg, err := d.readExpected()
		if err != nil {
			d.stopSession()
			return errResponsef("waiting for initialized: %v", err)
		}
		switch m := msg.(type) {
		case *godap.ErrorResponse:
			errMsg := m.Message
			if m.Body.Error != nil {
				errMsg = m.Body.Error.Format
			}
			d.stopSession()
			return errResponsef("request failed: %s", errMsg)
		case *godap.ExitedEvent, *godap.TerminatedEvent:
			d.stopSession()
			return errResponse("debug adapter exited during initialization — check that the program path is valid and the debugger is installed")
		case godap.ResponseMessage:
			if !m.GetResponse().Success {
				d.stopSession()
				return errResponsef("request failed: %s", m.GetResponse().Message)
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
			return errResponsef("set entry breakpoint: %v", err)
		}
	}
	d.sessionBreaks = args.Breaks
	d.sessionExceptionFilters = args.ExceptionFilters
	breaksByFile := groupBreakpoints(args.Breaks)
	for file, bps := range breaksByFile {
		d.recordRequestedBreaks(file, bps)
		if err := d.client.SetBreakpointsRequest(file, bps); err != nil {
			d.stopSession()
			return errResponsef("set breakpoints: %v", err)
		}
	}
	exceptionFilters := args.ExceptionFilters
	if exceptionFilters == nil {
		exceptionFilters = []string{}
	}
	if err := d.client.SetExceptionBreakpointsRequest(exceptionFilters); err != nil {
		d.stopSession()
		return errResponsef("set exception breakpoints: %v", err)
	}
	if err := d.client.ConfigurationDoneRequest(); err != nil {
		d.stopSession()
		return errResponsef("finalizing debug setup: %v", err)
	}

	// Wait for first stop. If we get an "entry" stop and have breakpoints, continue past it.
	for {
		ctx, err := d.waitForStopped()
		if err != nil {
			d.stopSession()
			return errResponse(err.Error())
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
				return errResponsef("continue past entry: %v", err)
			}
			continue
		}
		ctx.Output = "" // discard launch noise
		d.captureOutput = true
		return &Response{Status: "stopped", Data: ctx}
	}
}

// applyBreakpointUpdates processes breakpoint add/remove/exception filter changes.
// Returns nil on success, error *Response on failure.
func (d *Daemon) applyBreakpointUpdates(bu BreakpointUpdates) *Response {
	if len(bu.Breaks) > 0 || len(bu.RemoveBreaks) > 0 {
		if err := d.updateBreakpoints(bu.Breaks, bu.RemoveBreaks); err != nil {
			return errResponsef("set breakpoints: %v", err)
		}
	}
	if len(bu.ExceptionFilters) > 0 {
		existing := make(map[string]bool)
		for _, f := range d.sessionExceptionFilters {
			existing[f] = true
		}
		for _, f := range bu.ExceptionFilters {
			if !existing[f] {
				d.sessionExceptionFilters = append(d.sessionExceptionFilters, f)
			}
		}
		if err := d.client.SetExceptionBreakpointsRequest(d.sessionExceptionFilters); err != nil {
			return errResponsef("set exception breakpoints: %v", err)
		}
	}
	return nil
}

func (d *Daemon) handleStep(rawArgs json.RawMessage) *Response {
	if resp := d.requireSession(); resp != nil {
		return resp
	}

	var args StepArgs
	if rawArgs != nil {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return errResponsef("invalid args: %v", err)
		}
	}
	if args.Mode == "" {
		args.Mode = "over"
	}

	if errResp := d.applyBreakpointUpdates(args.BreakpointUpdates); errResp != nil {
		return errResp
	}

	threadID := resolveThreadID(d.threadID)

	switch args.Mode {
	case "over":
		if err := d.client.NextRequest(threadID); err != nil {
			return errResponsef("step over: %v", err)
		}
	case "in":
		if err := d.client.StepInRequest(threadID); err != nil {
			return errResponsef("step in: %v", err)
		}
	case "out":
		if err := d.client.StepOutRequest(threadID); err != nil {
			return errResponsef("step out: %v", err)
		}
	default:
		return errResponsef("invalid step mode %q — use: in, out, over", args.Mode)
	}

	return d.awaitStopResult()
}

func (d *Daemon) handleContinue(rawArgs json.RawMessage) *Response {
	if resp := d.requireSession(); resp != nil {
		return resp
	}

	var args ContinueArgs
	if rawArgs != nil {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return errResponsef("invalid args: %v", err)
		}
	}

	if errResp := d.applyBreakpointUpdates(args.BreakpointUpdates); errResp != nil {
		return errResp
	}

	// --to: add temp breakpoint before continuing
	if args.ContinueTo != nil {
		if err := d.updateBreakpoints([]Breakpoint{*args.ContinueTo}, nil); err != nil {
			return errResponsef("set temp breakpoint: %v", err)
		}
	}

	threadID := resolveThreadID(d.threadID)

	if err := d.client.ContinueRequest(threadID); err != nil {
		return errResponsef("continue: %v", err)
	}

	resp := d.awaitStopResult()

	// --to: remove temp breakpoint after stop (whether stopped or terminated)
	if args.ContinueTo != nil {
		if d.getClient() != nil {
			_ = d.updateBreakpoints(nil, []Breakpoint{*args.ContinueTo})
		} else {
			// Session ended — just clean sessionBreaks directly
			d.sessionBreaks = mergeBreakpoints(d.sessionBreaks, nil, []Breakpoint{*args.ContinueTo})
		}
	}

	return resp
}

func (d *Daemon) handlePause(rawArgs json.RawMessage) *Response {
	if resp := d.requireSession(); resp != nil {
		return resp
	}

	var args PauseArgs
	if rawArgs != nil {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return errResponsef("invalid args: %v", err)
		}
	}

	if errResp := d.applyBreakpointUpdates(args.BreakpointUpdates); errResp != nil {
		return errResp
	}

	threadID := resolveThreadID(d.threadID)

	if err := d.client.PauseRequest(threadID); err != nil {
		return errResponsef("pause: %v", err)
	}

	return d.awaitStopResult()
}

func (d *Daemon) handleContext(rawArgs json.RawMessage) *Response {
	if resp := d.requireSession(); resp != nil {
		return resp
	}

	var args ContextArgs
	if rawArgs != nil {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return errResponsef("invalid args: %v", err)
		}
	}

	if errResp := d.applyBreakpointUpdates(args.BreakpointUpdates); errResp != nil {
		return errResp
	}

	threadID := resolveThreadID(d.threadID)

	ctx, err := getFullContext(d, threadID, args.Frame)
	if err != nil {
		return errResponse(err.Error())
	}
	return &Response{Status: "stopped", Data: ctx}
}

func (d *Daemon) handleEval(rawArgs json.RawMessage) *Response {
	if resp := d.requireSession(); resp != nil {
		return resp
	}

	var args EvalArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return errResponsef("invalid args: %v", err)
	}

	if errResp := d.applyBreakpointUpdates(args.BreakpointUpdates); errResp != nil {
		return errResp
	}

	frameID := d.frameID // default: current (innermost) frame
	if args.Frame > 0 {
		if args.Frame >= len(d.frameIDs) {
			return errResponsef("frame %d out of range (stack has %d frames)", args.Frame, len(d.frameIDs))
		}
		frameID = d.frameIDs[args.Frame]
	}

	if err := d.client.EvaluateRequest(args.Expression, frameID, "repl"); err != nil {
		return errResponsef("evaluate: %v", err)
	}

	for {
		msg, err := d.readExpected()
		if err != nil {
			return errResponsef("reading eval response: %v", err)
		}
		switch resp := msg.(type) {
		case *godap.EvaluateResponse:
			if !resp.Success {
				return errResponsef("eval failed: %s", resp.Message)
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
			return errResponse("program terminated during evaluation")
		case godap.ResponseMessage:
			if !resp.GetResponse().Success {
				return errResponsef("eval failed: %s", resp.GetResponse().Message)
			}
			return &Response{Status: "ok", Data: &ContextResult{EvalResult: &EvalResult{Value: "(no result)"}}}
		}
	}
}

func (d *Daemon) handleOutput(rawArgs json.RawMessage) *Response {
	if d.getClient() != nil {
		var args OutputArgs
		if rawArgs != nil {
			if err := json.Unmarshal(rawArgs, &args); err != nil {
				return errResponsef("invalid args: %v", err)
			}
		}
		if errResp := d.applyBreakpointUpdates(args.BreakpointUpdates); errResp != nil {
			return errResp
		}
	}

	d.mu.Lock()
	output := d.outputString()
	d.mu.Unlock()
	return &Response{Status: "ok", Data: &ContextResult{Output: output}}
}

func (d *Daemon) handleBreakList() *Response {
	if resp := d.requireSession(); resp != nil {
		return resp
	}
	breaks := make([]Breakpoint, len(d.sessionBreaks))
	copy(breaks, d.sessionBreaks)
	sort.Slice(breaks, func(i, j int) bool {
		return breaks[i].LocationKey() < breaks[j].LocationKey()
	})
	filters := make([]string, len(d.sessionExceptionFilters))
	copy(filters, d.sessionExceptionFilters)
	sort.Strings(filters)
	return &Response{Status: "ok", Data: &ContextResult{Breakpoints: breaks, ExceptionFilters: filters, IsBreakList: true}}
}

func (d *Daemon) handleBreakAdd(rawArgs json.RawMessage) *Response {
	if resp := d.requireSession(); resp != nil {
		return resp
	}

	var args BreakAddArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return errResponsef("invalid args: %v", err)
	}

	if len(args.Breaks) > 0 {
		if err := d.updateBreakpoints(args.Breaks, nil); err != nil {
			return errResponsef("set breakpoints: %v", err)
		}
	}

	if len(args.ExceptionFilters) > 0 {
		existing := make(map[string]bool)
		for _, f := range d.sessionExceptionFilters {
			existing[f] = true
		}
		for _, f := range args.ExceptionFilters {
			if !existing[f] {
				d.sessionExceptionFilters = append(d.sessionExceptionFilters, f)
			}
		}
		if err := d.client.SetExceptionBreakpointsRequest(d.sessionExceptionFilters); err != nil {
			return errResponsef("set exception breakpoints: %v", err)
		}
	}

	return &Response{Status: "ok"}
}

func (d *Daemon) handleBreakRemove(rawArgs json.RawMessage) *Response {
	if resp := d.requireSession(); resp != nil {
		return resp
	}

	var args BreakRemoveArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return errResponsef("invalid args: %v", err)
	}

	if len(args.Breaks) > 0 {
		if err := d.updateBreakpoints(nil, args.Breaks); err != nil {
			return errResponsef("remove breakpoints: %v", err)
		}
	}

	if len(args.ExceptionFilters) > 0 {
		removeSet := make(map[string]bool)
		for _, f := range args.ExceptionFilters {
			removeSet[f] = true
		}
		var remaining []string
		for _, f := range d.sessionExceptionFilters {
			if !removeSet[f] {
				remaining = append(remaining, f)
			}
		}
		d.sessionExceptionFilters = remaining
		filters := remaining
		if filters == nil {
			filters = []string{}
		}
		if err := d.client.SetExceptionBreakpointsRequest(filters); err != nil {
			return errResponsef("set exception breakpoints: %v", err)
		}
	}

	return &Response{Status: "ok"}
}

func (d *Daemon) handleBreakClear() *Response {
	if resp := d.requireSession(); resp != nil {
		return resp
	}

	// Clear all file breakpoints by sending empty sets for each affected file
	affectedFiles := make(map[string]bool)
	for _, b := range d.sessionBreaks {
		affectedFiles[b.File] = true
	}
	for file := range affectedFiles {
		if err := d.client.SetBreakpointsRequest(file, nil); err != nil {
			return errResponsef("clear breakpoints: %v", err)
		}
	}
	d.sessionBreaks = nil

	// Clear exception filters
	d.sessionExceptionFilters = nil
	if err := d.client.SetExceptionBreakpointsRequest([]string{}); err != nil {
		return errResponsef("clear exception breakpoints: %v", err)
	}

	return &Response{Status: "ok"}
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
	d.clientMu.Lock()
	c := d.client
	d.client = nil
	d.clientMu.Unlock()
	if c != nil {
		_ = c.DisconnectRequest(true)
		c.Close()
	}
	if d.adapterCmd != nil && d.adapterCmd.Process != nil {
		_ = d.adapterCmd.Process.Kill()
		_ = d.adapterCmd.Wait()
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
		_ = d.listener.Close()
		d.listener = nil
	}
	_ = os.Remove(d.socketPath)
	_ = os.Remove(d.socketPath + ".pid")
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
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("connecting to adapter: %w", err)
	}
	d.setClient(client)
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
	if err := childClient.InitializeRequest(d.backend.AdapterID()); err != nil {
		childClient.Close()
		return fmt.Errorf("child initialize: %w", err)
	}
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
	if err := childClient.LaunchRequestWithArgs(config); err != nil {
		log.Printf("child launch: %v", err)
	}

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
	for file, bps := range breaksByFile {
		d.recordRequestedBreaks(file, bps)
		if err := childClient.SetBreakpointsRequest(file, bps); err != nil {
			log.Printf("child set breakpoints: %v", err)
		}
	}
	childExceptionFilters := d.sessionExceptionFilters
	if childExceptionFilters == nil {
		childExceptionFilters = []string{}
	}
	if err := childClient.SetExceptionBreakpointsRequest(childExceptionFilters); err != nil {
		log.Printf("child set exception breakpoints: %v", err)
	}
	if err := childClient.ConfigurationDoneRequest(); err != nil {
		log.Printf("child configuration done: %v", err)
	}

	// Swap to child session — readLoop continues reading from child
	d.setClient(childClient)
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
		return errResponse(err.Error())
	}
	if ctx.Location == nil {
		return &Response{Status: "terminated", Data: ctx}
	}
	return &Response{Status: "stopped", Data: ctx}
}

// --- Helpers ---

// parseBreakpointSpec parses "file:line[:condition]" into a Breakpoint.
// File is resolved to absolute path. Empty trailing condition is ignored.
func parseBreakpointSpec(spec string) (Breakpoint, error) {
	parts := strings.SplitN(spec, ":", 3)
	if len(parts) < 2 {
		return Breakpoint{}, fmt.Errorf("invalid breakpoint spec %q: expected file:line[:condition]", spec)
	}
	line, err := strconv.Atoi(parts[1])
	if err != nil {
		return Breakpoint{}, fmt.Errorf("invalid breakpoint spec %q: line must be a number", spec)
	}
	file := parts[0]
	if abs, err := filepath.Abs(file); err == nil {
		file = abs
	}
	var condition string
	if len(parts) == 3 {
		condition = strings.TrimSpace(parts[2])
	}
	return Breakpoint{File: file, Line: line, Condition: condition}, nil
}

// mergeBreakpoints merges add into existing, removes remove, returns updated sorted list.
// Identity is by LocationKey (file:line). Adding a breakpoint at an existing location replaces it.
// Removing matches by LocationKey only (ignores condition).
func mergeBreakpoints(existing, add, remove []Breakpoint) []Breakpoint {
	removeSet := make(map[string]bool, len(remove))
	for _, b := range remove {
		removeSet[b.LocationKey()] = true
	}

	merged := make(map[string]Breakpoint)
	for _, b := range existing {
		if !removeSet[b.LocationKey()] {
			merged[b.LocationKey()] = b
		}
	}
	for _, b := range add {
		merged[b.LocationKey()] = b
	}

	updated := make([]Breakpoint, 0, len(merged))
	for _, b := range merged {
		updated = append(updated, b)
	}
	sort.Slice(updated, func(i, j int) bool {
		return updated[i].LocationKey() < updated[j].LocationKey()
	})
	return updated
}

// updateBreakpoints validates, merges breakpoints, and sends SetBreakpointsRequest per affected file.
// Inputs are already parsed Breakpoints with absolute paths.
func (d *Daemon) updateBreakpoints(add, remove []Breakpoint) error {
	// Check for overlap: same location in both add and remove
	removeSet := make(map[string]bool, len(remove))
	for _, b := range remove {
		removeSet[b.LocationKey()] = true
	}
	for _, a := range add {
		if removeSet[a.LocationKey()] {
			return fmt.Errorf("breakpoint %s appears in both --break and --remove-break", a.LocationKey())
		}
	}

	updated := mergeBreakpoints(d.sessionBreaks, add, remove)
	d.sessionBreaks = updated

	// Collect all files that were affected (need to re-send breakpoints for each)
	affectedFiles := make(map[string]bool)
	allBreaks := groupBreakpoints(updated)
	for file := range allBreaks {
		affectedFiles[file] = true
	}
	// Also include files from removed breakpoints (may now have zero breakpoints)
	for _, b := range remove {
		affectedFiles[b.File] = true
	}

	// Send SetBreakpointsRequest for each affected file
	for file := range affectedFiles {
		bps := allBreaks[file] // may be nil/empty if all were removed
		d.recordRequestedBreaks(file, bps)
		if err := d.client.SetBreakpointsRequest(file, bps); err != nil {
			return err
		}
	}

	return nil
}

// groupBreakpoints groups breakpoints by file.
func groupBreakpoints(breaks []Breakpoint) map[string][]Breakpoint {
	result := make(map[string][]Breakpoint)
	for _, b := range breaks {
		result[b.File] = append(result[b.File], b)
	}
	return result
}

// findFreePort finds an available TCP port.
func findFreePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 5678 // fallback
	}
	defer func() { _ = l.Close() }()
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
		_ = WriteIPC(conn, &Request{Command: "ping"})
		var resp Response
		if err := ReadIPC(conn, &resp); err == nil && resp.Status == "ok" {
			_ = conn.Close()
			return socketPath, nil
		}
		_ = conn.Close()
		// Stale socket, remove it
		_ = os.Remove(socketPath)
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
	_ = cmd.Process.Release()

	// Wait for socket to appear
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if conn, err := net.DialTimeout("unix", socketPath, 200*time.Millisecond); err == nil {
			_ = conn.Close()
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
	defer func() { _ = conn.Close() }()

	// Set a generous timeout for commands that block (debug, step, continue)
	_ = conn.SetDeadline(time.Now().Add(5 * time.Minute))

	if err := WriteIPC(conn, req); err != nil {
		return nil, fmt.Errorf("sending command: %w", err)
	}

	var resp Response
	if err := ReadIPC(conn, &resp); err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	return &resp, nil
}
