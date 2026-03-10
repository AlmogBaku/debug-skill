package dap

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// Breakpoint represents a source breakpoint with optional condition.
type Breakpoint struct {
	File      string `json:"file"`
	Line      int    `json:"line"`
	Condition string `json:"condition,omitempty"`
}

// LocationKey returns "file:line" — the identity key for merging/removing.
func (b Breakpoint) LocationKey() string {
	return fmt.Sprintf("%s:%d", b.File, b.Line)
}

// String returns a human-readable representation.
func (b Breakpoint) String() string {
	if b.Condition != "" {
		return fmt.Sprintf("%s:%d (when %s)", b.File, b.Line, b.Condition)
	}
	return fmt.Sprintf("%s:%d", b.File, b.Line)
}

// Request is sent from CLI to daemon over Unix socket.
type Request struct {
	Command string          `json:"command"`
	Args    json.RawMessage `json:"args,omitempty"`
}

// Response is sent from daemon to CLI over Unix socket.
type Response struct {
	Status string         `json:"status"` // "stopped", "terminated", "error", "ok"
	Error  string         `json:"error,omitempty"`
	Data   *ContextResult `json:"data,omitempty"`
}

// ExceptionInfo holds details about an exception that caused a stop.
type ExceptionInfo struct {
	ExceptionID string `json:"exception_id"`
	Description string `json:"description,omitempty"`
	Details     string `json:"details,omitempty"`
}

// ContextResult holds the auto-context returned by execution commands.
type ContextResult struct {
	Reason        string         `json:"reason,omitempty"`
	Location      *Location      `json:"location,omitempty"`
	Source        []SourceLine   `json:"source,omitempty"`
	Locals        []Variable     `json:"locals,omitempty"`
	Stack         []StackFrame   `json:"stack,omitempty"`
	Output        string         `json:"output,omitempty"`
	ExitCode      *int           `json:"exit_code,omitempty"`
	EvalResult    *EvalResult    `json:"eval_result,omitempty"`
	ExceptionInfo *ExceptionInfo `json:"exception_info,omitempty"`

	// Warnings from unverified breakpoints (drained on each response)
	Warnings []string `json:"warnings,omitempty"`

	// Break list results
	Breakpoints      []Breakpoint `json:"breakpoints,omitempty"`
	ExceptionFilters []string     `json:"exception_filters,omitempty"`
	IsBreakList      bool         `json:"is_break_list,omitempty"`
}

// Location identifies a position in source code.
type Location struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Function string `json:"function"`
}

// SourceLine is a single line of source code.
type SourceLine struct {
	Line    int    `json:"line"`
	Text    string `json:"text"`
	Current bool   `json:"current,omitempty"`
}

// Variable represents a local variable.
type Variable struct {
	Name  string `json:"name"`
	Type  string `json:"type,omitempty"`
	Value string `json:"value"`
	Len   int    `json:"len,omitempty"`
}

// StackFrame represents a frame in the call stack.
type StackFrame struct {
	Frame    int    `json:"frame"`
	Function string `json:"function"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
}

// EvalResult holds the result of an eval command.
type EvalResult struct {
	Value string `json:"value"`
	Type  string `json:"type,omitempty"`
}

// --- IPC command args ---

// BreakpointUpdates holds breakpoint modifications shared across commands.
type BreakpointUpdates struct {
	Breaks           []Breakpoint `json:"breaks,omitempty"`            // breakpoints to add (additive)
	RemoveBreaks     []Breakpoint `json:"remove_breaks,omitempty"`     // breakpoints to remove
	ExceptionFilters []string     `json:"exception_filters,omitempty"` // backend-specific filter IDs (additive, merged with existing)
}

// DebugArgs are arguments for the "debug" command.
type DebugArgs struct {
	Script           string       `json:"script"`
	Backend          string       `json:"backend,omitempty"`
	Breaks           []Breakpoint `json:"breaks,omitempty"`
	StopOnEntry      bool         `json:"stop_on_entry,omitempty"`
	Attach           string       `json:"attach,omitempty"` // "host:port" for remote
	ProgramArgs      []string     `json:"program_args,omitempty"`
	ExceptionFilters []string     `json:"exception_filters,omitempty"` // backend-specific filter IDs
}

// StepArgs are arguments for the "step" command.
type StepArgs struct {
	Mode string `json:"mode"` // "over", "in", "out"
	BreakpointUpdates
}

// PauseArgs are arguments for the "pause" command.
type PauseArgs struct {
	BreakpointUpdates
}

// ContinueArgs are arguments for the "continue" command.
type ContinueArgs struct {
	ContinueTo *Breakpoint `json:"continue_to,omitempty"`
	BreakpointUpdates
}

// EvalArgs are arguments for the "eval" command.
type EvalArgs struct {
	Expression string `json:"expression"`
	Frame      int    `json:"frame,omitempty"`
	BreakpointUpdates
}

// ContextArgs are arguments for the "context" command.
type ContextArgs struct {
	Frame int `json:"frame,omitempty"`
	BreakpointUpdates
}

// OutputArgs are arguments for the "output" command.
type OutputArgs struct {
	BreakpointUpdates
}

// BreakAddArgs are arguments for the "break_add" command.
type BreakAddArgs struct {
	Breaks           []Breakpoint `json:"breaks,omitempty"`
	ExceptionFilters []string     `json:"exception_filters,omitempty"`
}

// BreakRemoveArgs are arguments for the "break_remove" command.
type BreakRemoveArgs struct {
	Breaks           []Breakpoint `json:"breaks,omitempty"`
	ExceptionFilters []string     `json:"exception_filters,omitempty"`
}

// --- Length-prefixed JSON IPC ---

// WriteIPC writes a length-prefixed JSON message to w.
func WriteIPC(w io.Writer, msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(len(data))); err != nil {
		return fmt.Errorf("write length: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write data: %w", err)
	}
	return nil
}

// ReadIPC reads a length-prefixed JSON message from r into msg.
func ReadIPC(r io.Reader, msg any) error {
	var length uint32
	if err := binary.Read(r, binary.LittleEndian, &length); err != nil {
		return fmt.Errorf("read length: %w", err)
	}
	if length > 10*1024*1024 { // 10MB sanity limit
		return fmt.Errorf("message too large: %d bytes", length)
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return fmt.Errorf("read data: %w", err)
	}
	if err := json.Unmarshal(data, msg); err != nil {
		return fmt.Errorf("unmarshal: %w", err)
	}
	return nil
}
