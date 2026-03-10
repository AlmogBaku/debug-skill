package dap

import (
	"strings"
	"testing"
)

func TestFormatText_Stopped(t *testing.T) {
	result := &ContextResult{
		Reason: "breakpoint",
		Location: &Location{
			File:     "app.py",
			Line:     42,
			Function: "process_items",
		},
		Source: []SourceLine{
			{Line: 40, Text: "    for item in items:"},
			{Line: 41, Text: "        result = transform(item)"},
			{Line: 42, Text: "        if result is None:", Current: true},
			{Line: 43, Text: "            errors.append(item)"},
			{Line: 44, Text: "            continue"},
		},
		Locals: []Variable{
			{Name: "item", Type: "dict", Value: `{"name": "test"}`},
			{Name: "result", Type: "NoneType", Value: "None"},
		},
		Stack: []StackFrame{
			{Frame: 0, Function: "process_items", File: "app.py", Line: 42},
			{Frame: 1, Function: "main", File: "app.py", Line: 15},
		},
		Output: "Processing item: test\n",
	}

	text := FormatText(result)

	checks := []string{
		"Stopped: breakpoint",
		"Function: process_items",
		"File: app.py:42",
		"42>|",
		"item (dict) = ",
		"#0 process_items at app.py:42",
		"Processing item: test",
	}
	for _, check := range checks {
		if !strings.Contains(text, check) {
			t.Errorf("FormatText missing %q in:\n%s", check, text)
		}
	}
}

func TestFormatText_Eval(t *testing.T) {
	result := &ContextResult{
		EvalResult: &EvalResult{Value: "42", Type: "int"},
	}
	text := FormatText(result)
	if !strings.Contains(text, "42 (type: int)") {
		t.Errorf("expected eval result, got: %q", text)
	}
}

func TestFormatJSON(t *testing.T) {
	result := &ContextResult{
		Reason:   "step",
		Location: &Location{File: "test.py", Line: 10, Function: "foo"},
	}
	json := FormatJSON(result)
	if !strings.Contains(json, `"reason": "step"`) {
		t.Errorf("expected JSON with reason, got: %s", json)
	}
}

func TestFormatResponse_Error(t *testing.T) {
	resp := &Response{Status: "error", Error: "something went wrong"}
	text := FormatResponse(resp, false)
	if !strings.Contains(text, "something went wrong") {
		t.Errorf("expected error message, got: %q", text)
	}
}

func TestFormatText_ExceptionInfo(t *testing.T) {
	result := &ContextResult{
		Reason: "exception",
		Location: &Location{
			File:     "app.py",
			Line:     3,
			Function: "convert",
		},
		ExceptionInfo: &ExceptionInfo{
			ExceptionID: "ValueError",
			Description: "invalid literal for int() with base 10: 'abc'",
		},
	}
	text := FormatText(result)
	if !strings.Contains(text, "Exception: ValueError") {
		t.Errorf("expected Exception: ValueError, got:\n%s", text)
	}
	if !strings.Contains(text, "invalid literal") {
		t.Errorf("expected exception description, got:\n%s", text)
	}
}

func TestFormatText_ExceptionInfoWithDetails(t *testing.T) {
	result := &ContextResult{
		Reason: "exception",
		ExceptionInfo: &ExceptionInfo{
			ExceptionID: "RuntimeError",
			Description: "something went wrong",
			Details:     "stack trace details here",
		},
	}
	text := FormatText(result)
	if !strings.Contains(text, "Exception: RuntimeError") {
		t.Errorf("expected RuntimeError, got:\n%s", text)
	}
	if !strings.Contains(text, "stack trace details here") {
		t.Errorf("expected details, got:\n%s", text)
	}
}

func TestFormatResponse_Terminated(t *testing.T) {
	exitCode := 0
	resp := &Response{
		Status: "terminated",
		Data:   &ContextResult{ExitCode: &exitCode, Output: "hello\n"},
	}
	text := FormatResponse(resp, false)
	if !strings.Contains(text, "Program terminated") {
		t.Errorf("expected terminated, got: %q", text)
	}
	if !strings.Contains(text, "hello") {
		t.Errorf("expected output, got: %q", text)
	}
}
