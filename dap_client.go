package dap

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"

	godap "github.com/google/go-dap"
)

// DAPClient is a DAP protocol client built on google/go-dap.
// All send methods are fire-and-forget; responses come through ReadMessage.
type DAPClient struct {
	rwc    io.ReadWriteCloser
	reader *bufio.Reader
	seq    int
}

// newDAPClient creates a new DAPClient over a TCP connection.
func newDAPClient(addr string) (*DAPClient, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connecting to DAP server at %s: %w", addr, err)
	}
	return newDAPClientFromRWC(conn), nil
}

// newDAPClientFromRWC creates a new DAPClient with the given ReadWriteCloser.
func newDAPClientFromRWC(rwc io.ReadWriteCloser) *DAPClient {
	return &DAPClient{rwc: rwc, reader: bufio.NewReader(rwc), seq: 1}
}

// Close closes the underlying connection.
func (c *DAPClient) Close() {
	if c.rwc != nil {
		_ = c.rwc.Close()
	}
}

// ReadMessage reads the next DAP protocol message.
// It skips messages with unsupported event/command types (e.g. debugpy's custom events).
func (c *DAPClient) ReadMessage() (godap.Message, error) {
	for {
		msg, err := godap.ReadProtocolMessage(c.reader)
		if err != nil {
			// go-dap returns errors for unsupported event types (e.g. debugpySockets).
			// Skip these and keep reading.
			if strings.Contains(err.Error(), "is not supported") {
				continue
			}
			return nil, err
		}
		return msg, nil
	}
}

func (c *DAPClient) newRequest(command string) *godap.Request {
	r := &godap.Request{}
	r.Type = "request"
	r.Command = command
	r.Seq = c.seq
	c.seq++
	return r
}

func (c *DAPClient) send(request godap.Message) error {
	return godap.WriteProtocolMessage(c.rwc, request)
}

func toRawMessage(in any) json.RawMessage {
	out, _ := json.Marshal(in)
	return out
}

// InitializeRequest sends an 'initialize' request.
func (c *DAPClient) InitializeRequest(adapterID string) error {
	request := &godap.InitializeRequest{Request: *c.newRequest("initialize")}
	request.Arguments = godap.InitializeRequestArguments{
		AdapterID:              adapterID,
		PathFormat:             "path",
		LinesStartAt1:          true,
		ColumnsStartAt1:        true,
		SupportsVariableType:   true,
		SupportsVariablePaging: true,
		Locale:                 "en-us",
	}
	return c.send(request)
}

// LaunchRequestWithArgs sends a 'launch' request with backend-specific args.
func (c *DAPClient) LaunchRequestWithArgs(args map[string]any) error {
	request := &godap.LaunchRequest{Request: *c.newRequest("launch")}
	request.Arguments = toRawMessage(args)
	return c.send(request)
}

// AttachRequestWithArgs sends an 'attach' request with backend-specific args.
func (c *DAPClient) AttachRequestWithArgs(args map[string]any) error {
	request := &godap.AttachRequest{Request: *c.newRequest("attach")}
	request.Arguments = toRawMessage(args)
	return c.send(request)
}

// SetBreakpointsRequest sends a 'setBreakpoints' request.
func (c *DAPClient) SetBreakpointsRequest(file string, breakpoints []Breakpoint) error {
	request := &godap.SetBreakpointsRequest{Request: *c.newRequest("setBreakpoints")}
	request.Arguments = godap.SetBreakpointsArguments{
		Source:      godap.Source{Name: file, Path: file},
		Breakpoints: make([]godap.SourceBreakpoint, len(breakpoints)),
	}
	for i, bp := range breakpoints {
		request.Arguments.Breakpoints[i].Line = bp.Line
		request.Arguments.Breakpoints[i].Condition = bp.Condition
	}
	return c.send(request)
}

// SetFunctionBreakpointsRequest sends a 'setFunctionBreakpoints' request.
func (c *DAPClient) SetFunctionBreakpointsRequest(functions []string) error {
	request := &godap.SetFunctionBreakpointsRequest{Request: *c.newRequest("setFunctionBreakpoints")}
	request.Arguments = godap.SetFunctionBreakpointsArguments{
		Breakpoints: make([]godap.FunctionBreakpoint, len(functions)),
	}
	for i, f := range functions {
		request.Arguments.Breakpoints[i].Name = f
	}
	return c.send(request)
}

// SetExceptionBreakpointsRequest sends a 'setExceptionBreakpoints' request.
func (c *DAPClient) SetExceptionBreakpointsRequest(filters []string) error {
	request := &godap.SetExceptionBreakpointsRequest{Request: *c.newRequest("setExceptionBreakpoints")}
	request.Arguments.Filters = filters
	return c.send(request)
}

// ConfigurationDoneRequest sends a 'configurationDone' request.
func (c *DAPClient) ConfigurationDoneRequest() error {
	request := &godap.ConfigurationDoneRequest{Request: *c.newRequest("configurationDone")}
	return c.send(request)
}

// ContinueRequest sends a 'continue' request.
func (c *DAPClient) ContinueRequest(threadID int) error {
	request := &godap.ContinueRequest{Request: *c.newRequest("continue")}
	request.Arguments.ThreadId = threadID
	return c.send(request)
}

// NextRequest sends a 'next' (step over) request.
func (c *DAPClient) NextRequest(threadID int) error {
	request := &godap.NextRequest{Request: *c.newRequest("next")}
	request.Arguments.ThreadId = threadID
	return c.send(request)
}

// StepInRequest sends a 'stepIn' request.
func (c *DAPClient) StepInRequest(threadID int) error {
	request := &godap.StepInRequest{Request: *c.newRequest("stepIn")}
	request.Arguments.ThreadId = threadID
	return c.send(request)
}

// StepOutRequest sends a 'stepOut' request.
func (c *DAPClient) StepOutRequest(threadID int) error {
	request := &godap.StepOutRequest{Request: *c.newRequest("stepOut")}
	request.Arguments.ThreadId = threadID
	return c.send(request)
}

// PauseRequest sends a 'pause' request.
func (c *DAPClient) PauseRequest(threadID int) error {
	request := &godap.PauseRequest{Request: *c.newRequest("pause")}
	request.Arguments.ThreadId = threadID
	return c.send(request)
}

// StackTraceRequest sends a 'stackTrace' request.
func (c *DAPClient) StackTraceRequest(threadID, startFrame, levels int) error {
	request := &godap.StackTraceRequest{Request: *c.newRequest("stackTrace")}
	request.Arguments.ThreadId = threadID
	request.Arguments.StartFrame = startFrame
	request.Arguments.Levels = levels
	return c.send(request)
}

// ScopesRequest sends a 'scopes' request.
func (c *DAPClient) ScopesRequest(frameID int) error {
	request := &godap.ScopesRequest{Request: *c.newRequest("scopes")}
	request.Arguments.FrameId = frameID
	return c.send(request)
}

// VariablesRequest sends a 'variables' request.
func (c *DAPClient) VariablesRequest(variablesReference int) error {
	request := &godap.VariablesRequest{Request: *c.newRequest("variables")}
	request.Arguments.VariablesReference = variablesReference
	return c.send(request)
}

// EvaluateRequest sends an 'evaluate' request.
func (c *DAPClient) EvaluateRequest(expression string, frameID int, context string) error {
	request := &godap.EvaluateRequest{Request: *c.newRequest("evaluate")}
	request.Arguments.Expression = expression
	request.Arguments.FrameId = frameID
	request.Arguments.Context = context
	return c.send(request)
}

// SetVariableRequest sends a 'setVariable' request.
func (c *DAPClient) SetVariableRequest(variablesRef int, name, value string) error {
	request := &godap.SetVariableRequest{Request: *c.newRequest("setVariable")}
	request.Arguments.VariablesReference = variablesRef
	request.Arguments.Name = name
	request.Arguments.Value = value
	return c.send(request)
}

// DisconnectRequest sends a 'disconnect' request.
func (c *DAPClient) DisconnectRequest(terminateDebuggee bool) error {
	request := &godap.DisconnectRequest{Request: *c.newRequest("disconnect")}
	request.Arguments = &godap.DisconnectArguments{
		TerminateDebuggee: terminateDebuggee,
	}
	return c.send(request)
}

// TerminateRequest sends a 'terminate' request.
func (c *DAPClient) TerminateRequest() error {
	request := &godap.TerminateRequest{Request: *c.newRequest("terminate")}
	return c.send(request)
}

// ThreadsRequest sends a 'threads' request.
func (c *DAPClient) ThreadsRequest() error {
	request := &godap.ThreadsRequest{Request: *c.newRequest("threads")}
	return c.send(request)
}
