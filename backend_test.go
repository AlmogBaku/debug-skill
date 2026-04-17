package dap

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// venvLayout returns the (binSubdir, preferredName, fallbackName) for the current
// OS's virtualenv layout. On Windows that's Scripts\python.exe etc.
func venvLayout() (string, string, string) {
	if runtime.GOOS == "windows" {
		return "Scripts", "python.exe", "python3.exe"
	}
	return "bin", "python3", "python"
}

func TestResolveVenvPython_FindsPreferred(t *testing.T) {
	dir := t.TempDir()
	sub, preferred, _ := venvLayout()
	binDir := filepath.Join(dir, sub)
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(binDir, preferred)
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VIRTUAL_ENV", dir)
	got := ResolveVenvPython()
	wantAbs, _ := filepath.Abs(target)
	if got != wantAbs {
		t.Errorf("ResolveVenvPython() = %q, want %q", got, wantAbs)
	}
}

func TestResolveVenvPython_FallsBackToSecondary(t *testing.T) {
	dir := t.TempDir()
	sub, _, fallback := venvLayout()
	binDir := filepath.Join(dir, sub)
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(binDir, fallback)
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VIRTUAL_ENV", dir)
	got := ResolveVenvPython()
	wantAbs, _ := filepath.Abs(target)
	if got != wantAbs {
		t.Errorf("ResolveVenvPython() = %q, want %q", got, wantAbs)
	}
}

func TestResolveVenvPython_PrefersPreferredOverFallback(t *testing.T) {
	dir := t.TempDir()
	sub, preferred, fallback := venvLayout()
	binDir := filepath.Join(dir, sub)
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	preferredPath := filepath.Join(binDir, preferred)
	fallbackPath := filepath.Join(binDir, fallback)
	for _, p := range []string{preferredPath, fallbackPath} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("VIRTUAL_ENV", dir)
	got := ResolveVenvPython()
	wantAbs, _ := filepath.Abs(preferredPath)
	if got != wantAbs {
		t.Errorf("ResolveVenvPython() = %q, want preferred %q", got, wantAbs)
	}
}

func TestResolvePythonFlag_AbsolutePath(t *testing.T) {
	dir := t.TempDir()
	py := filepath.Join(dir, "python3")
	if err := os.WriteFile(py, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := ResolvePythonFlag(py)
	if err != nil {
		t.Fatalf("ResolvePythonFlag(%q): %v", py, err)
	}
	if got != py {
		t.Errorf("got %q, want %q", got, py)
	}
}

func TestResolvePythonFlag_RelativePath(t *testing.T) {
	dir := t.TempDir()
	py := filepath.Join(dir, "python3")
	if err := os.WriteFile(py, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// chdir to tmp, pass relative path
	wd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	got, err := ResolvePythonFlag("./python3")
	if err != nil {
		t.Fatalf("ResolvePythonFlag: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("expected absolute path, got %q", got)
	}
}

func TestResolvePythonFlag_NonexistentPath(t *testing.T) {
	_, err := ResolvePythonFlag("/definitely/not/a/real/python-xyz")
	if err == nil {
		t.Fatal("expected error for nonexistent path")
	}
}

func TestResolvePythonFlag_BareNameViaLookPath(t *testing.T) {
	// Use a name we know is on PATH — "sh" works on all Unix + a lot of Windows dev envs.
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	got, err := ResolvePythonFlag("sh")
	if err != nil {
		t.Fatalf("ResolvePythonFlag(sh): %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("expected absolute path from LookPath, got %q", got)
	}
}

func TestResolvePythonFlag_BareNameNotFound(t *testing.T) {
	_, err := ResolvePythonFlag("definitely-not-a-real-binary-xyz-123")
	if err == nil {
		t.Fatal("expected error for unknown bare name")
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

	b := &debugpyBackend{python: stub}
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
