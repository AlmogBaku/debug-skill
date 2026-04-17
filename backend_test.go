package dap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveVenvPython_NoEnv(t *testing.T) {
	t.Setenv("VIRTUAL_ENV", "")
	if got := ResolveVenvPython(); got != "" {
		t.Errorf("ResolveVenvPython() = %q, want empty", got)
	}
}

func TestResolveVenvPython_MissingBinary(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VIRTUAL_ENV", dir)
	if got := ResolveVenvPython(); got != "" {
		t.Errorf("ResolveVenvPython() = %q, want empty (no python in venv)", got)
	}
}

func TestResolveVenvPython_FindsPython3(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	py3 := filepath.Join(binDir, "python3")
	if err := os.WriteFile(py3, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VIRTUAL_ENV", dir)
	if got := ResolveVenvPython(); got != py3 {
		t.Errorf("ResolveVenvPython() = %q, want %q", got, py3)
	}
}

func TestResolveVenvPython_FallsBackToPython(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	py := filepath.Join(binDir, "python")
	if err := os.WriteFile(py, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VIRTUAL_ENV", dir)
	if got := ResolveVenvPython(); got != py {
		t.Errorf("ResolveVenvPython() = %q, want %q", got, py)
	}
}

func TestResolveVenvPython_PrefersPython3OverPython(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	py3 := filepath.Join(binDir, "python3")
	py := filepath.Join(binDir, "python")
	for _, p := range []string{py3, py} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("VIRTUAL_ENV", dir)
	if got := ResolveVenvPython(); got != py3 {
		t.Errorf("ResolveVenvPython() = %q, want python3 path %q", got, py3)
	}
}

// TestDebugpyBackend_SpawnUsesConfiguredPython verifies that debugpyBackend.Python,
// when set, is the binary invoked by Spawn (rather than falling back to "python3"
// on PATH). We pass a stub binary that exits immediately; Spawn should attempt to
// run it, and since it is not a real debugpy, we expect Spawn to fail *after* the
// stub runs. Failure mode diagnoses whether the configured binary was actually
// used.
func TestDebugpyBackend_SpawnUsesConfiguredPython(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "my-python")
	// Stub exits cleanly without printing "Listening" so waitForReady fails —
	// confirming the configured binary was executed (not some other python).
	script := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	b := &debugpyBackend{Python: stub}
	_, _, err := b.Spawn(":0")
	if err == nil {
		t.Fatal("Spawn with stub python should fail (no Listening output)")
	}
	// The error should be about process exiting without reporting listen address,
	// proving the configured python was the one that ran.
	if want := "process exited without reporting listen address"; !strings.Contains(err.Error(), want) {
		t.Errorf("Spawn error = %q, want to contain %q", err.Error(), want)
	}
}
