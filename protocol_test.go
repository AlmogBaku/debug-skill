package dap

import (
	"bytes"
	"testing"
)

func TestWriteReadIPC(t *testing.T) {
	var buf bytes.Buffer

	// Write a request
	req := Request{Command: "step", Args: []byte(`{"mode":"over"}`)}
	if err := WriteIPC(&buf, &req); err != nil {
		t.Fatalf("WriteIPC: %v", err)
	}

	// Read it back
	var got Request
	if err := ReadIPC(&buf, &got); err != nil {
		t.Fatalf("ReadIPC: %v", err)
	}

	if got.Command != "step" {
		t.Errorf("Command = %q, want %q", got.Command, "step")
	}
	if string(got.Args) != `{"mode":"over"}` {
		t.Errorf("Args = %q, want %q", got.Args, `{"mode":"over"}`)
	}
}

func TestWriteReadIPC_Response(t *testing.T) {
	var buf bytes.Buffer

	exitCode := 0
	resp := Response{
		Status: "stopped",
		Data: &ContextResult{
			Reason: "breakpoint",
			Location: &Location{
				File:     "test.py",
				Line:     42,
				Function: "main",
			},
			ExitCode: &exitCode,
		},
	}
	if err := WriteIPC(&buf, &resp); err != nil {
		t.Fatalf("WriteIPC: %v", err)
	}

	var got Response
	if err := ReadIPC(&buf, &got); err != nil {
		t.Fatalf("ReadIPC: %v", err)
	}

	if got.Status != "stopped" {
		t.Errorf("Status = %q, want %q", got.Status, "stopped")
	}
	if got.Data == nil {
		t.Fatal("Data is nil")
	}
	if got.Data.Reason != "breakpoint" {
		t.Errorf("Reason = %q, want %q", got.Data.Reason, "breakpoint")
	}
	if got.Data.Location.File != "test.py" {
		t.Errorf("File = %q, want %q", got.Data.Location.File, "test.py")
	}
}

func TestReadIPC_TooLarge(t *testing.T) {
	var buf bytes.Buffer
	// Write a length that exceeds the 10MB limit
	_ = WriteIPC(&buf, &struct{ X string }{X: "test"})
	// Manually corrupt the length to be huge
	data := buf.Bytes()
	data[0] = 0xFF
	data[1] = 0xFF
	data[2] = 0xFF
	data[3] = 0x7F

	var got Request
	err := ReadIPC(bytes.NewReader(data), &got)
	if err == nil {
		t.Fatal("expected error for oversized message")
	}
}
