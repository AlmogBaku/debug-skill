package dap

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestOutputBufferBoundedAtWrite(t *testing.T) {
	d := &Daemon{}

	// Write more than maxOutputLines lines — buffer must never exceed cap
	total := maxOutputLines + 500
	for i := 0; i < total; i++ {
		d.appendOutput(fmt.Sprintf("line %d\n", i))
	}

	if len(d.outputLines) > maxOutputLines {
		t.Errorf("expected at most %d lines in buffer, got %d", maxOutputLines, len(d.outputLines))
	}

	// Last line in buffer should be the last complete line written
	lastExpected := fmt.Sprintf("line %d", total-1)
	last := d.outputLines[len(d.outputLines)-1]
	if last != lastExpected {
		t.Errorf("expected last buffered line %q, got %q", lastExpected, last)
	}
}

func TestOutputBufferUnderLimit(t *testing.T) {
	d := &Daemon{}

	for i := 0; i < 10; i++ {
		d.appendOutput(fmt.Sprintf("line %d\n", i))
	}

	// All 10 lines should be present (no trimming below cap)
	if len(d.outputLines) != 10 {
		t.Errorf("expected 10 lines, got %d", len(d.outputLines))
	}
}

func TestHandleOutput(t *testing.T) {
	d := &Daemon{}
	d.appendOutput("hello\n")
	d.appendOutput("world\n")

	resp := d.handleOutput(nil)

	if resp.Status != "ok" {
		t.Fatalf("expected status ok, got %q", resp.Status)
	}
	if resp.Data == nil {
		t.Fatal("expected Data to be set")
	}
	if resp.Data.Output != "hello\nworld" {
		t.Errorf("expected output %q, got %q", "hello\nworld", resp.Data.Output)
	}

	// Buffer should be cleared
	if d.outputLines != nil || d.outputPartial.Len() != 0 {
		t.Error("buffer should be cleared after handleOutput")
	}
}

func TestTempBinaryCleanup(t *testing.T) {
	// Verify cleanup function is called in stopSession
	called := false
	d := &Daemon{
		cleanupFn: func() { called = true },
	}

	d.stopSession()

	if !called {
		t.Error("cleanupFn was not called during stopSession")
	}
	if d.cleanupFn != nil {
		t.Error("cleanupFn should be nil after stopSession")
	}
}

func TestTempBinaryCleanup_NilSafe(t *testing.T) {
	// Verify stopSession with nil cleanupFn doesn't panic
	d := &Daemon{}
	d.stopSession() // should not panic
}

func TestParseBreakpointSpec(t *testing.T) {
	tests := []struct {
		name     string
		spec     string
		wantFile string // suffix check (absolute paths vary)
		wantLine int
		wantCond string
		wantErr  bool
	}{
		{
			name:     "file:line",
			spec:     "/app.py:10",
			wantFile: "/app.py",
			wantLine: 10,
		},
		{
			name:     "file:line:condition",
			spec:     "/app.py:10:x > 5",
			wantFile: "/app.py",
			wantLine: 10,
			wantCond: "x > 5",
		},
		{
			name:     "condition with colons",
			spec:     "/app.py:10:a:b",
			wantFile: "/app.py",
			wantLine: 10,
			wantCond: "a:b",
		},
		{
			name:     "empty trailing condition",
			spec:     "/app.py:10:",
			wantFile: "/app.py",
			wantLine: 10,
		},
		{
			name:    "no line",
			spec:    "app.py",
			wantErr: true,
		},
		{
			name:    "non-numeric line",
			spec:    "/app.py:abc",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bp, err := parseBreakpointSpec(tt.spec)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseBreakpointSpec(%q) error = %v, wantErr %v", tt.spec, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if bp.Line != tt.wantLine {
				t.Errorf("line = %d, want %d", bp.Line, tt.wantLine)
			}
			if bp.Condition != tt.wantCond {
				t.Errorf("condition = %q, want %q", bp.Condition, tt.wantCond)
			}
			if tt.wantFile != "" && bp.File != tt.wantFile {
				t.Errorf("file = %q, want suffix %q", bp.File, tt.wantFile)
			}
		})
	}
}

func TestBreakWarnings(t *testing.T) {
	d := &Daemon{}

	// Initially empty
	if w := d.drainBreakWarnings(); w != nil {
		t.Errorf("expected nil, got %v", w)
	}

	// Accumulate warnings
	d.addBreakWarning("line 999 not found")
	d.addBreakWarning("line 888 not found")

	// Drain returns all and clears
	w := d.drainBreakWarnings()
	if len(w) != 2 {
		t.Fatalf("expected 2 warnings, got %d", len(w))
	}
	if w[0] != "line 999 not found" || w[1] != "line 888 not found" {
		t.Errorf("unexpected warnings: %v", w)
	}

	// After drain, empty again
	if w := d.drainBreakWarnings(); w != nil {
		t.Errorf("expected nil after drain, got %v", w)
	}
}

func TestAttachWarnings(t *testing.T) {
	d := &Daemon{}

	// No warnings — no-op, nil Data stays nil
	resp := &Response{Status: "ok"}
	d.attachWarnings(resp)
	if resp.Data != nil {
		t.Errorf("expected nil Data when no warnings, got %+v", resp.Data)
	}

	// With warnings, nil Data → creates Data
	d.addBreakWarning("bp not verified")
	resp = &Response{Status: "ok"}
	d.attachWarnings(resp)
	if resp.Data == nil {
		t.Fatal("expected Data to be created")
	}
	if len(resp.Data.Warnings) != 1 || resp.Data.Warnings[0] != "bp not verified" {
		t.Errorf("unexpected warnings: %v", resp.Data.Warnings)
	}

	// With warnings, existing Data
	d.addBreakWarning("another warning")
	resp = &Response{Status: "stopped", Data: &ContextResult{Reason: "breakpoint"}}
	d.attachWarnings(resp)
	if len(resp.Data.Warnings) != 1 || resp.Data.Warnings[0] != "another warning" {
		t.Errorf("unexpected warnings: %v", resp.Data.Warnings)
	}
	if resp.Data.Reason != "breakpoint" {
		t.Errorf("existing fields should be preserved, reason = %q", resp.Data.Reason)
	}
}

func TestFormatTextWarnings(t *testing.T) {
	r := &ContextResult{
		Reason: "breakpoint",
		Location: &Location{
			File:     "/app.py",
			Line:     10,
			Function: "main",
		},
		Warnings: []string{"breakpoint at /app.py:999 not verified: line not found"},
	}

	text := FormatText(r)
	if !strings.Contains(text, "Warnings:") {
		t.Errorf("expected Warnings section, got:\n%s", text)
	}
	if !strings.Contains(text, "⚠") {
		t.Errorf("expected ⚠ marker, got:\n%s", text)
	}
	if !strings.Contains(text, "line not found") {
		t.Errorf("expected warning message, got:\n%s", text)
	}
}

func TestFormatTextNoWarnings(t *testing.T) {
	r := &ContextResult{
		Reason: "breakpoint",
	}
	text := FormatText(r)
	if strings.Contains(text, "Warnings:") {
		t.Errorf("should not have Warnings section when empty, got:\n%s", text)
	}
}

func TestRequireSession(t *testing.T) {
	d := &Daemon{}

	// No client — should return error response
	resp := d.requireSession()
	if resp == nil {
		t.Fatal("expected error response when client is nil")
	}
	if resp.Status != "error" {
		t.Errorf("expected status error, got %q", resp.Status)
	}
	if !strings.Contains(resp.Error, "no active debug session") {
		t.Errorf("expected 'no active debug session' in error, got %q", resp.Error)
	}
}

func TestMalformedJSONArgs(t *testing.T) {
	d := &Daemon{}
	// Set a fake client so requireSession passes
	d.client = &DAPClient{}

	malformed := json.RawMessage(`{invalid`)

	tests := []struct {
		name    string
		handler func(json.RawMessage) *Response
	}{
		{"handleStep", d.handleStep},
		{"handleContinue", d.handleContinue},
		{"handleContext", d.handleContext},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := tt.handler(malformed)
			if resp.Status != "error" {
				t.Errorf("expected error status, got %q", resp.Status)
			}
			if !strings.Contains(resp.Error, "invalid args") {
				t.Errorf("expected 'invalid args' in error, got %q", resp.Error)
			}
		})
	}
}

func TestMergeBreakpoints(t *testing.T) {
	tests := []struct {
		name     string
		existing []Breakpoint
		add      []Breakpoint
		remove   []Breakpoint
		want     []Breakpoint
	}{
		{
			name:     "add to empty",
			existing: nil,
			add:      []Breakpoint{{File: "/app.py", Line: 10}, {File: "/app.py", Line: 20}},
			want:     []Breakpoint{{File: "/app.py", Line: 10}, {File: "/app.py", Line: 20}},
		},
		{
			name:     "additive merge",
			existing: []Breakpoint{{File: "/app.py", Line: 10}},
			add:      []Breakpoint{{File: "/app.py", Line: 20}},
			want:     []Breakpoint{{File: "/app.py", Line: 10}, {File: "/app.py", Line: 20}},
		},
		{
			name:     "deduplicate",
			existing: []Breakpoint{{File: "/app.py", Line: 10}},
			add:      []Breakpoint{{File: "/app.py", Line: 10}},
			want:     []Breakpoint{{File: "/app.py", Line: 10}},
		},
		{
			name:     "remove existing",
			existing: []Breakpoint{{File: "/app.py", Line: 10}, {File: "/app.py", Line: 20}},
			remove:   []Breakpoint{{File: "/app.py", Line: 10}},
			want:     []Breakpoint{{File: "/app.py", Line: 20}},
		},
		{
			name:     "add and remove different",
			existing: []Breakpoint{{File: "/app.py", Line: 10}},
			add:      []Breakpoint{{File: "/app.py", Line: 30}},
			remove:   []Breakpoint{{File: "/app.py", Line: 10}},
			want:     []Breakpoint{{File: "/app.py", Line: 30}},
		},
		{
			name:     "remove nonexistent is no-op",
			existing: []Breakpoint{{File: "/app.py", Line: 10}},
			remove:   []Breakpoint{{File: "/app.py", Line: 99}},
			want:     []Breakpoint{{File: "/app.py", Line: 10}},
		},
		{
			name:     "empty inputs",
			existing: nil,
			add:      nil,
			remove:   nil,
			want:     []Breakpoint{},
		},
		{
			name:     "replace condition on same location",
			existing: []Breakpoint{{File: "/app.py", Line: 10}},
			add:      []Breakpoint{{File: "/app.py", Line: 10, Condition: "x>5"}},
			want:     []Breakpoint{{File: "/app.py", Line: 10, Condition: "x>5"}},
		},
		{
			name:     "remove ignores condition",
			existing: []Breakpoint{{File: "/app.py", Line: 10, Condition: "x>5"}},
			remove:   []Breakpoint{{File: "/app.py", Line: 10}},
			want:     []Breakpoint{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeBreakpoints(tt.existing, tt.add, tt.remove)
			if len(got) != len(tt.want) {
				t.Fatalf("expected %d breakpoints %v, got %d: %v", len(tt.want), tt.want, len(got), got)
			}
			for i, w := range tt.want {
				if got[i].LocationKey() != w.LocationKey() {
					t.Errorf("breakpoint[%d] location = %q, want %q", i, got[i].LocationKey(), w.LocationKey())
				}
				if got[i].Condition != w.Condition {
					t.Errorf("breakpoint[%d] condition = %q, want %q", i, got[i].Condition, w.Condition)
				}
			}
		})
	}
}
