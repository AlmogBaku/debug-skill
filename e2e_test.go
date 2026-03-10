package dap

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// e2eEnv holds a built binary, running daemon, and helper to run CLI commands.
type e2eEnv struct {
	t          *testing.T
	binary     string
	socketPath string
	daemon     *exec.Cmd
}

func newE2EEnv(t *testing.T) *e2eEnv {
	t.Helper()

	binary := filepath.Join(t.TempDir(), "dap")
	build := exec.Command("go", "build", "-o", binary, "./cmd/dap")
	build.Dir = projectRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %s\n%s", err, out)
	}

	// Use /tmp for socket to avoid Unix socket path length limit (~104 bytes on macOS).
	// t.TempDir() paths with long test names can exceed this limit.
	sockDir, err := os.MkdirTemp("/tmp", "dap-test-*")
	if err != nil {
		t.Fatalf("creating socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	socketPath := filepath.Join(sockDir, "test.sock")

	daemon := exec.Command(binary, "__daemon", "--socket", socketPath)
	daemon.Stdout = os.Stderr
	daemon.Stderr = os.Stderr
	if err := daemon.Start(); err != nil {
		t.Fatalf("starting daemon: %v", err)
	}

	// Wait for daemon socket
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	env := &e2eEnv{t: t, binary: binary, socketPath: socketPath, daemon: daemon}
	t.Cleanup(func() {
		_ = exec.Command(binary, "stop", "--socket", socketPath).Run()
		_ = daemon.Process.Kill()
		_ = daemon.Wait()
		_ = os.Remove(socketPath)
	})
	return env
}

func (e *e2eEnv) run(args ...string) (string, error) {
	cmd := exec.Command(e.binary, append(args, "--socket", e.socketPath)...)
	cmd.Dir = projectRoot(e.t)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// --- Python tests ---

// TestE2E_DebugPython runs a full debug session: debug → step → eval → continue → stop.
func TestE2E_DebugPython(t *testing.T) {
	if err := exec.Command("python3", "-c", "import debugpy").Run(); err != nil {
		t.Skip("debugpy not installed")
	}

	env := newE2EEnv(t)
	scriptPath := filepath.Join(projectRoot(t), "testdata", "python", "simple.py")

	// 1. Debug with breakpoint at line 3
	out, err := env.run("debug", scriptPath, "--break", scriptPath+":3")
	if err != nil {
		t.Fatalf("debug failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Stopped: breakpoint") {
		t.Errorf("expected breakpoint stop, got:\n%s", out)
	}
	if !strings.Contains(out, "x (int) = 1") {
		t.Errorf("expected x=1 in locals, got:\n%s", out)
	}
	if !strings.Contains(out, "y (int) = 2") {
		t.Errorf("expected y=2 in locals, got:\n%s", out)
	}

	// 2. Step over
	out, err = env.run("step")
	if err != nil {
		t.Fatalf("step failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Stopped: step") {
		t.Errorf("expected step stop, got:\n%s", out)
	}
	if !strings.Contains(out, "z (int) = 3") {
		t.Errorf("expected z=3 after step, got:\n%s", out)
	}

	// 3. Eval expression
	out, err = env.run("eval", "x + y + z")
	if err != nil {
		t.Fatalf("eval failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "6") {
		t.Errorf("expected eval result 6, got:\n%s", out)
	}

	// 4. Continue to end
	out, err = env.run("continue")
	if err != nil {
		t.Fatalf("continue failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "terminated") || !strings.Contains(out, "Program terminated") {
		t.Errorf("expected terminated, got:\n%s", out)
	}
	if !strings.Contains(out, "Result: 3") {
		t.Errorf("expected program output, got:\n%s", out)
	}

	// 5. Stop
	out, err = env.run("stop")
	if err != nil {
		t.Fatalf("stop failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "OK") {
		t.Errorf("expected OK, got:\n%s", out)
	}
}

// TestE2E_JSONOutput verifies --json flag produces valid JSON.
func TestE2E_JSONOutput(t *testing.T) {
	if err := exec.Command("python3", "-c", "import debugpy").Run(); err != nil {
		t.Skip("debugpy not installed")
	}

	env := newE2EEnv(t)
	scriptPath := filepath.Join(projectRoot(t), "testdata", "python", "simple.py")

	cmd := exec.Command(env.binary, "debug", scriptPath, "--break", scriptPath+":2", "--json", "--socket", env.socketPath)
	cmd.Dir = projectRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("debug --json failed: %v\n%s", err, out)
	}

	var result ContextResult
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if result.Reason != "breakpoint" {
		t.Errorf("reason = %q, want %q", result.Reason, "breakpoint")
	}
	if result.Location == nil || result.Location.Line != 2 {
		t.Errorf("expected line 2, got: %+v", result.Location)
	}
}

// TestE2E_DebugPython_Scheduler exercises cross-file breakpoints across a
// multifile Python app: main.py → runner.py → resolver.py.
func TestE2E_DebugPython_Scheduler(t *testing.T) {
	if err := exec.Command("python3", "-c", "import debugpy").Run(); err != nil {
		t.Skip("debugpy not installed")
	}

	env := newE2EEnv(t)
	root := projectRoot(t)
	mainPath := filepath.Join(root, "testdata", "python", "scheduler", "main.py")
	resolverPath := filepath.Join(root, "testdata", "python", "scheduler", "resolver.py")

	// 1. Debug main.py with a cross-file breakpoint inside resolver.py's visit()
	out, err := env.run("debug", mainPath, "--break", resolverPath+":17")
	if err != nil {
		t.Fatalf("debug failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Stopped: breakpoint") {
		t.Errorf("expected breakpoint stop, got:\n%s", out)
	}
	if !strings.Contains(out, "resolver.py") {
		t.Errorf("expected resolver.py in location, got:\n%s", out)
	}

	// 2. Eval the current task name
	out, err = env.run("eval", "task.name")
	if err != nil {
		t.Fatalf("eval failed: %v\n%s", err, out)
	}
	if out == "" {
		t.Errorf("expected task.name result, got empty")
	}

	// 3. Continue until program terminates — visit() is called once per task,
	// so the breakpoint fires multiple times before the program exits.
	var finalOut string
	for range 10 {
		out, err = env.run("continue")
		if err != nil {
			t.Fatalf("continue failed: %v\n%s", err, out)
		}
		if strings.Contains(out, "Program terminated") {
			finalOut = out
			break
		}
	}
	if finalOut == "" {
		t.Fatalf("program did not terminate after 10 continues")
	}
	if !strings.Contains(finalOut, "BUG!") {
		t.Errorf("expected BUG! in program output (intentional bug), got:\n%s", finalOut)
	}

	// 4. Stop
	out, err = env.run("stop")
	if err != nil {
		t.Fatalf("stop failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "OK") {
		t.Errorf("expected OK, got:\n%s", out)
	}
}

// TestE2E_ContinueWithBreakpoint tests adding breakpoints mid-session via continue --break.
func TestE2E_ContinueWithBreakpoint(t *testing.T) {
	if err := exec.Command("python3", "-c", "import debugpy").Run(); err != nil {
		t.Skip("debugpy not installed")
	}

	env := newE2EEnv(t)
	scriptPath := filepath.Join(projectRoot(t), "testdata", "python", "simple.py")

	// 1. Debug with breakpoint at line 2 (y = 2)
	out, err := env.run("debug", scriptPath, "--break", scriptPath+":2")
	if err != nil {
		t.Fatalf("debug failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Stopped: breakpoint") {
		t.Errorf("expected breakpoint stop at line 2, got:\n%s", out)
	}

	// 2. Continue with a new breakpoint at line 4 (print)
	out, err = env.run("continue", "--break", scriptPath+":4")
	if err != nil {
		t.Fatalf("continue --break failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Stopped: breakpoint") {
		t.Errorf("expected breakpoint stop at line 4, got:\n%s", out)
	}
	if !strings.Contains(out, ":4") {
		t.Errorf("expected stop at line 4, got:\n%s", out)
	}

	// 3. Continue to end — line 2 breakpoint is still set but program already
	// passed it, so no more stops ahead.
	out, err = env.run("continue")
	if err != nil {
		t.Fatalf("continue failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "terminated") || !strings.Contains(out, "Program terminated") {
		t.Errorf("expected terminated, got:\n%s", out)
	}

	// 4. Stop
	out, err = env.run("stop")
	if err != nil {
		t.Fatalf("stop failed: %v\n%s", err, out)
	}
}

// TestE2E_ContinueRemoveBreakpoint tests removing breakpoints mid-session via continue --remove-break.
func TestE2E_ContinueRemoveBreakpoint(t *testing.T) {
	if err := exec.Command("python3", "-c", "import debugpy").Run(); err != nil {
		t.Skip("debugpy not installed")
	}

	env := newE2EEnv(t)
	scriptPath := filepath.Join(projectRoot(t), "testdata", "python", "simple.py")

	// 1. Debug with breakpoints at lines 2 and 4
	out, err := env.run("debug", scriptPath, "--break", scriptPath+":2", "--break", scriptPath+":4")
	if err != nil {
		t.Fatalf("debug failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Stopped: breakpoint") {
		t.Errorf("expected breakpoint stop, got:\n%s", out)
	}

	// 2. Continue but remove the breakpoint at line 4 — should run to end
	out, err = env.run("continue", "--remove-break", scriptPath+":4")
	if err != nil {
		t.Fatalf("continue --remove-break failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "terminated") || !strings.Contains(out, "Program terminated") {
		t.Errorf("expected terminated (bp at line 4 was removed), got:\n%s", out)
	}

	// 3. Stop
	out, err = env.run("stop")
	if err != nil {
		t.Fatalf("stop failed: %v\n%s", err, out)
	}
}

// TestE2E_StepWithBreakpoint tests adding breakpoints mid-session via step --break.
func TestE2E_StepWithBreakpoint(t *testing.T) {
	if err := exec.Command("python3", "-c", "import debugpy").Run(); err != nil {
		t.Skip("debugpy not installed")
	}

	env := newE2EEnv(t)
	scriptPath := filepath.Join(projectRoot(t), "testdata", "python", "simple.py")

	// 1. Debug with stop-on-entry
	out, err := env.run("debug", scriptPath, "--stop-on-entry")
	if err != nil {
		t.Fatalf("debug failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Stopped:") {
		t.Errorf("expected stopped, got:\n%s", out)
	}

	// 2. Step with --break at line 4 — breakpoint should be set
	out, err = env.run("step", "--break", scriptPath+":4")
	if err != nil {
		t.Fatalf("step --break failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Stopped: step") {
		t.Errorf("expected step stop, got:\n%s", out)
	}

	// 3. Continue — should hit the breakpoint at line 4
	out, err = env.run("continue")
	if err != nil {
		t.Fatalf("continue failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Stopped: breakpoint") {
		t.Errorf("expected breakpoint stop at line 4, got:\n%s", out)
	}
	if !strings.Contains(out, ":4") {
		t.Errorf("expected stop at line 4, got:\n%s", out)
	}

	// 4. Stop
	out, err = env.run("stop")
	if err != nil {
		t.Fatalf("stop failed: %v\n%s", err, out)
	}
}

// TestE2E_BreakCommand tests dap break list/add/remove/clear.
func TestE2E_BreakCommand(t *testing.T) {
	if err := exec.Command("python3", "-c", "import debugpy").Run(); err != nil {
		t.Skip("debugpy not installed")
	}

	env := newE2EEnv(t)
	scriptPath := filepath.Join(projectRoot(t), "testdata", "python", "simple.py")

	// 1. Debug with breakpoint at line 2
	out, err := env.run("debug", scriptPath, "--break", scriptPath+":2")
	if err != nil {
		t.Fatalf("debug failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Stopped: breakpoint") {
		t.Errorf("expected breakpoint stop, got:\n%s", out)
	}

	// 2. break list — should show line 2
	out, err = env.run("break", "list")
	if err != nil {
		t.Fatalf("break list failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, ":2") {
		t.Errorf("expected :2 in break list, got:\n%s", out)
	}

	// 3. break add line 4
	out, err = env.run("break", "add", scriptPath+":4")
	if err != nil {
		t.Fatalf("break add failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "OK") {
		t.Errorf("expected OK, got:\n%s", out)
	}

	// 4. break list — should show both
	out, err = env.run("break", "list")
	if err != nil {
		t.Fatalf("break list failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, ":2") || !strings.Contains(out, ":4") {
		t.Errorf("expected both :2 and :4 in break list, got:\n%s", out)
	}

	// 5. break remove line 2
	out, err = env.run("break", "remove", scriptPath+":2")
	if err != nil {
		t.Fatalf("break remove failed: %v\n%s", err, out)
	}

	// 6. break list — should only show line 4
	out, err = env.run("break", "list")
	if err != nil {
		t.Fatalf("break list failed: %v\n%s", err, out)
	}
	if strings.Contains(out, ":2") {
		t.Errorf("line 2 should be removed, got:\n%s", out)
	}
	if !strings.Contains(out, ":4") {
		t.Errorf("expected :4 in break list, got:\n%s", out)
	}

	// 7. Continue — should hit line 4
	out, err = env.run("continue")
	if err != nil {
		t.Fatalf("continue failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Stopped: breakpoint") {
		t.Errorf("expected breakpoint stop at line 4, got:\n%s", out)
	}

	// 8. break clear
	out, err = env.run("break", "clear")
	if err != nil {
		t.Fatalf("break clear failed: %v\n%s", err, out)
	}

	// 9. break list — should be empty
	out, err = env.run("break", "list")
	if err != nil {
		t.Fatalf("break list failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "(none)") {
		t.Errorf("expected (none) after clear, got:\n%s", out)
	}

	// 10. Continue — should run to end (no breakpoints)
	out, err = env.run("continue")
	if err != nil {
		t.Fatalf("continue failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "terminated") || !strings.Contains(out, "Program terminated") {
		t.Errorf("expected terminated, got:\n%s", out)
	}

	// 11. Stop
	out, err = env.run("stop")
	if err != nil {
		t.Fatalf("stop failed: %v\n%s", err, out)
	}
}

// TestE2E_ConditionalBreakpoint tests conditional breakpoints with Python/debugpy.
func TestE2E_ConditionalBreakpoint(t *testing.T) {
	if err := exec.Command("python3", "-c", "import debugpy").Run(); err != nil {
		t.Skip("debugpy not installed")
	}

	env := newE2EEnv(t)
	scriptPath := filepath.Join(projectRoot(t), "testdata", "python", "loop.py")

	// 1. Debug with conditional breakpoint: stop only when i == 3
	out, err := env.run("debug", scriptPath, "--break", scriptPath+":3:i == 3")
	if err != nil {
		t.Fatalf("debug failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Stopped: breakpoint") {
		t.Errorf("expected breakpoint stop, got:\n%s", out)
	}
	if !strings.Contains(out, "i (int) = 3") {
		t.Errorf("expected i=3 in locals (conditional breakpoint), got:\n%s", out)
	}

	// 2. break list — should show condition
	out, err = env.run("break", "list")
	if err != nil {
		t.Fatalf("break list failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "i == 3") {
		t.Errorf("expected condition in break list, got:\n%s", out)
	}

	// 3. Continue — condition won't match again (i goes 4 then loop ends), should terminate
	out, err = env.run("continue")
	if err != nil {
		t.Fatalf("continue failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Program terminated") {
		t.Errorf("expected terminated, got:\n%s", out)
	}
	if !strings.Contains(out, "Total: 10") {
		t.Errorf("expected program output 'Total: 10', got:\n%s", out)
	}

	// 4. Stop
	out, err = env.run("stop")
	if err != nil {
		t.Fatalf("stop failed: %v\n%s", err, out)
	}
}

// TestE2E_Pause tests the pause command on a long-running Python script.
func TestE2E_Pause(t *testing.T) {
	if err := exec.Command("python3", "-c", "import debugpy").Run(); err != nil {
		t.Skip("debugpy not installed")
	}

	env := newE2EEnv(t)
	scriptPath := filepath.Join(projectRoot(t), "testdata", "python", "long_loop.py")

	// 1. Debug with breakpoint at line 3 (i = 0, before loop)
	out, err := env.run("debug", scriptPath, "--break", scriptPath+":3")
	if err != nil {
		t.Fatalf("debug failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Stopped: breakpoint") {
		t.Errorf("expected breakpoint stop, got:\n%s", out)
	}

	// 2. Continue (loop will run) — then pause from another goroutine
	doneCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		o, e := env.run("continue")
		doneCh <- o
		errCh <- e
	}()

	// Give the program time to enter the loop
	time.Sleep(500 * time.Millisecond)

	// 3. Pause from another connection
	pauseOut, pauseErr := env.run("pause")
	if pauseErr != nil {
		// The continue goroutine might have received the stop already
		continueOut := <-doneCh
		<-errCh
		if !strings.Contains(continueOut, "Stopped: pause") {
			t.Fatalf("pause failed: %v\n%s\ncontinue out: %s", pauseErr, pauseOut, continueOut)
		}
	} else {
		// Pause succeeded directly — continue goroutine should have gotten the stop
		<-doneCh
		<-errCh
	}

	// 4. Stop
	out, err = env.run("stop")
	if err != nil {
		t.Fatalf("stop failed: %v\n%s", err, out)
	}
}

// TestE2E_ContinueTo tests the continue --to flag (temp breakpoint).
func TestE2E_ContinueTo(t *testing.T) {
	if err := exec.Command("python3", "-c", "import debugpy").Run(); err != nil {
		t.Skip("debugpy not installed")
	}

	env := newE2EEnv(t)
	scriptPath := filepath.Join(projectRoot(t), "testdata", "python", "simple.py")

	// 1. Debug with --stop-on-entry
	out, err := env.run("debug", scriptPath, "--stop-on-entry")
	if err != nil {
		t.Fatalf("debug failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Stopped:") {
		t.Errorf("expected stopped, got:\n%s", out)
	}

	// 2. continue --to simple.py:4 — should stop at line 4
	out, err = env.run("continue", "--to", scriptPath+":4")
	if err != nil {
		t.Fatalf("continue --to failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, ":4") {
		t.Errorf("expected stop at line 4, got:\n%s", out)
	}

	// 3. break list — temp BP should be gone
	out, err = env.run("break", "list")
	if err != nil {
		t.Fatalf("break list failed: %v\n%s", err, out)
	}
	if strings.Contains(out, ":4") {
		t.Errorf("temp breakpoint at :4 should be removed, got:\n%s", out)
	}

	// 4. continue — should terminate normally
	out, err = env.run("continue")
	if err != nil {
		t.Fatalf("continue failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Program terminated") {
		t.Errorf("expected terminated, got:\n%s", out)
	}

	// 5. Stop
	out, err = env.run("stop")
	if err != nil {
		t.Fatalf("stop failed: %v\n%s", err, out)
	}
}

// TestE2E_ExceptionInfo tests exception info in auto-context.
func TestE2E_ExceptionInfo(t *testing.T) {
	if err := exec.Command("python3", "-c", "import debugpy").Run(); err != nil {
		t.Skip("debugpy not installed")
	}

	env := newE2EEnv(t)
	scriptPath := filepath.Join(projectRoot(t), "testdata", "python", "exception.py")

	// Debug with a breakpoint (to avoid stop-on-entry) and userUnhandled exception filter.
	// The breakpoint at line 4 is where convert("abc") is called — it will stop there first.
	out, err := env.run("debug", scriptPath, "--break", scriptPath+":4", "--break-on-exception", "userUnhandled")
	if err != nil {
		t.Fatalf("debug failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Stopped: breakpoint") {
		t.Errorf("expected breakpoint stop, got:\n%s", out)
	}

	// Continue — should hit the ValueError exception
	out, err = env.run("continue")
	if err != nil {
		t.Fatalf("continue failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Stopped: exception") {
		t.Errorf("expected exception stop, got:\n%s", out)
	}
	if !strings.Contains(out, "Exception:") {
		t.Errorf("expected Exception: in output, got:\n%s", out)
	}
	if !strings.Contains(out, "ValueError") {
		t.Errorf("expected ValueError, got:\n%s", out)
	}

	// Stop
	out, err = env.run("stop")
	if err != nil {
		t.Fatalf("stop failed: %v\n%s", err, out)
	}
}

// --- Go tests ---

// TestE2E_DebugGo runs a full Go debug session via dlv: debug → step → eval → continue → stop.
func TestE2E_DebugGo(t *testing.T) {
	if _, err := exec.LookPath("dlv"); err != nil {
		t.Skip("dlv not installed")
	}

	env := newE2EEnv(t)
	scriptPath := filepath.Join(projectRoot(t), "testdata", "go", "hello.go")

	// 1. Debug with breakpoint at line 8 (z := x + y)
	out, err := env.run("debug", scriptPath, "--break", scriptPath+":8")
	if err != nil {
		t.Fatalf("debug failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Stopped: breakpoint") {
		t.Errorf("expected breakpoint stop, got:\n%s", out)
	}
	if !strings.Contains(out, "x (int) = 1") {
		t.Errorf("expected x=1, got:\n%s", out)
	}
	if !strings.Contains(out, "y (int) = 2") {
		t.Errorf("expected y=2, got:\n%s", out)
	}

	// 2. Step over
	out, err = env.run("step")
	if err != nil {
		t.Fatalf("step failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Stopped: step") {
		t.Errorf("expected step stop, got:\n%s", out)
	}
	if !strings.Contains(out, "z (int) = 3") {
		t.Errorf("expected z=3, got:\n%s", out)
	}

	// 3. Eval expression
	out, err = env.run("eval", "x + y")
	if err != nil {
		t.Fatalf("eval failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "3") {
		t.Errorf("expected eval result 3, got:\n%s", out)
	}

	// 4. Continue to end
	out, err = env.run("continue")
	if err != nil {
		t.Fatalf("continue failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "terminated") || !strings.Contains(out, "Program terminated") {
		t.Errorf("expected terminated, got:\n%s", out)
	}

	// 5. Stop
	out, err = env.run("stop")
	if err != nil {
		t.Fatalf("stop failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "OK") {
		t.Errorf("expected OK, got:\n%s", out)
	}
}

// TestE2E_DebugGo_StopOnEntry tests --stop-on-entry with Go.
func TestE2E_DebugGo_StopOnEntry(t *testing.T) {
	if _, err := exec.LookPath("dlv"); err != nil {
		t.Skip("dlv not installed")
	}

	env := newE2EEnv(t)
	scriptPath := filepath.Join(projectRoot(t), "testdata", "go", "hello.go")

	out, err := env.run("debug", scriptPath, "--stop-on-entry")
	if err != nil {
		t.Fatalf("debug --stop-on-entry failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Stopped:") {
		t.Errorf("expected stopped, got:\n%s", out)
	}
	// Should be stopped at the beginning of main
	if !strings.Contains(out, "main.main") {
		t.Errorf("expected main.main in stack, got:\n%s", out)
	}
}

// TestE2E_DebugGo_JSON verifies --json with Go backend.
func TestE2E_DebugGo_JSON(t *testing.T) {
	if _, err := exec.LookPath("dlv"); err != nil {
		t.Skip("dlv not installed")
	}

	env := newE2EEnv(t)
	scriptPath := filepath.Join(projectRoot(t), "testdata", "go", "hello.go")

	cmd := exec.Command(env.binary, "debug", scriptPath, "--break", scriptPath+":8", "--json", "--socket", env.socketPath)
	cmd.Dir = projectRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("debug --json failed: %v\n%s", err, out)
	}

	var result ContextResult
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if result.Reason != "breakpoint" {
		t.Errorf("reason = %q, want %q", result.Reason, "breakpoint")
	}
	if result.Location == nil || result.Location.Line != 8 {
		t.Errorf("expected line 8, got: %+v", result.Location)
	}
}

// --- Rust tests ---

// TestE2E_DebugRust runs a full Rust debug session via lldb-dap: debug → step → eval → continue → stop.
func TestE2E_DebugRust(t *testing.T) {
	if _, err := exec.LookPath("rustc"); err != nil {
		t.Skip("rustc not installed")
	}
	if findLLDBDap() == "" {
		t.Skip("lldb-dap not found")
	}

	env := newE2EEnv(t)
	scriptPath := filepath.Join(projectRoot(t), "testdata", "rust", "hello.rs")

	// 1. Debug with breakpoint at line 4 (let z = x + y)
	out, err := env.run("debug", scriptPath, "--break", scriptPath+":4")
	if err != nil {
		t.Fatalf("debug failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Stopped: breakpoint") {
		t.Errorf("expected breakpoint stop, got:\n%s", out)
	}
	if !strings.Contains(out, "x") {
		t.Errorf("expected x in locals, got:\n%s", out)
	}
	if !strings.Contains(out, "y") {
		t.Errorf("expected y in locals, got:\n%s", out)
	}

	// 2. Step over
	out, err = env.run("step")
	if err != nil {
		t.Fatalf("step failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Stopped: step") {
		t.Errorf("expected step stop, got:\n%s", out)
	}

	// 3. Continue to end
	out, err = env.run("continue")
	if err != nil {
		t.Fatalf("continue failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "terminated") || !strings.Contains(out, "Program terminated") {
		t.Errorf("expected terminated, got:\n%s", out)
	}

	// 4. Stop
	out, err = env.run("stop")
	if err != nil {
		t.Fatalf("stop failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "OK") {
		t.Errorf("expected OK, got:\n%s", out)
	}
}

// --- Node.js tests ---

// TestE2E_DebugNode runs a full Node.js debug session via js-debug: debug → step → continue → stop.
func TestE2E_DebugNode(t *testing.T) {
	if FindJSDebugServer() == "" {
		t.Skip("js-debug not found")
	}

	env := newE2EEnv(t)
	scriptPath := filepath.Join(projectRoot(t), "testdata", "node", "simple.js")

	// 1. Debug with breakpoint at line 3 (const z = x + y)
	out, err := env.run("debug", scriptPath, "--break", scriptPath+":3")
	if err != nil {
		t.Fatalf("debug failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Stopped: breakpoint") {
		t.Errorf("expected breakpoint stop, got:\n%s", out)
	}
	if !strings.Contains(out, "x") {
		t.Errorf("expected x in locals, got:\n%s", out)
	}

	// 2. Step over
	out, err = env.run("step")
	if err != nil {
		t.Fatalf("step failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Stopped: step") {
		t.Errorf("expected step stop, got:\n%s", out)
	}

	// 3. Continue to end
	out, err = env.run("continue")
	if err != nil {
		t.Fatalf("continue failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "terminated") || !strings.Contains(out, "Program terminated") {
		t.Errorf("expected terminated, got:\n%s", out)
	}

	// 4. Stop
	out, err = env.run("stop")
	if err != nil {
		t.Fatalf("stop failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "OK") {
		t.Errorf("expected OK, got:\n%s", out)
	}
}

// --- Remote attach tests ---

// TestE2E_RemoteAttach_Python tests attaching to a remote debugpy instance.
func TestE2E_RemoteAttach_Python(t *testing.T) {
	if err := exec.Command("python3", "-c", "import debugpy").Run(); err != nil {
		t.Skip("debugpy not installed")
	}

	env := newE2EEnv(t)
	scriptPath := filepath.Join(projectRoot(t), "testdata", "python", "simple.py")

	// Start a debugpy server that waits for a client to attach
	debugPort := findFreePortForTest(t)
	debugpy := exec.Command("python3", "-m", "debugpy", "--listen", fmt.Sprintf("127.0.0.1:%d", debugPort), "--wait-for-client", scriptPath)
	debugpy.Dir = projectRoot(t)
	debugpy.Stdout = os.Stderr
	debugpy.Stderr = os.Stderr
	if err := debugpy.Start(); err != nil {
		t.Fatalf("starting debugpy: %v", err)
	}
	t.Cleanup(func() {
		_ = debugpy.Process.Kill()
		_ = debugpy.Wait()
	})

	// Wait for debugpy to start listening.
	// NOTE: We cannot use waitForPort (connect+close) because debugpy treats
	// any TCP connection as a DAP client. Connecting and closing poisons the session.
	if err := waitForListening(debugPort, 10*time.Second); err != nil {
		t.Fatalf("debugpy did not start: %v", err)
	}

	// Attach with breakpoint
	out, err := env.run("debug", "--attach", fmt.Sprintf("127.0.0.1:%d", debugPort), "--backend", "debugpy", "--break", scriptPath+":3")
	if err != nil {
		t.Fatalf("attach failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Stopped: breakpoint") {
		t.Errorf("expected breakpoint stop, got:\n%s", out)
	}
	if !strings.Contains(out, "x (int) = 1") {
		t.Errorf("expected x=1, got:\n%s", out)
	}

	// Continue to end
	out, err = env.run("continue")
	if err != nil {
		t.Fatalf("continue failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "terminated") {
		t.Errorf("expected terminated, got:\n%s", out)
	}
}

// --- Multi-session tests ---

// TestE2E_MultiSession verifies two independent sessions can run in parallel.
func TestE2E_MultiSession(t *testing.T) {
	if err := exec.Command("python3", "-c", "import debugpy").Run(); err != nil {
		t.Skip("debugpy not installed")
	}
	if _, err := exec.LookPath("dlv"); err != nil {
		t.Skip("dlv not installed")
	}

	// Build binary once
	binary := filepath.Join(t.TempDir(), "dap")
	build := exec.Command("go", "build", "-o", binary, "./cmd/dap")
	build.Dir = projectRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %s\n%s", err, out)
	}

	tmpDir := t.TempDir()
	socketA := filepath.Join(tmpDir, "a.sock")
	socketB := filepath.Join(tmpDir, "b.sock")

	// Start two daemons
	daemonA := exec.Command(binary, "__daemon", "--socket", socketA)
	daemonA.Stdout = os.Stderr
	daemonA.Stderr = os.Stderr
	if err := daemonA.Start(); err != nil {
		t.Fatalf("starting daemon A: %v", err)
	}

	daemonB := exec.Command(binary, "__daemon", "--socket", socketB)
	daemonB.Stdout = os.Stderr
	daemonB.Stderr = os.Stderr
	if err := daemonB.Start(); err != nil {
		t.Fatalf("starting daemon B: %v", err)
	}

	// Wait for both sockets
	for _, sock := range []string{socketA, socketB} {
		for i := 0; i < 50; i++ {
			if _, err := os.Stat(sock); err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	t.Cleanup(func() {
		_ = exec.Command(binary, "stop", "--socket", socketA).Run()
		_ = exec.Command(binary, "stop", "--socket", socketB).Run()
		_ = daemonA.Process.Kill()
		_ = daemonA.Wait()
		_ = daemonB.Process.Kill()
		_ = daemonB.Wait()
	})

	runA := func(args ...string) (string, error) {
		cmd := exec.Command(binary, append(args, "--socket", socketA)...)
		cmd.Dir = projectRoot(t)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	runB := func(args ...string) (string, error) {
		cmd := exec.Command(binary, append(args, "--socket", socketB)...)
		cmd.Dir = projectRoot(t)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	pyScript := filepath.Join(projectRoot(t), "testdata", "python", "simple.py")
	goScript := filepath.Join(projectRoot(t), "testdata", "go", "hello.go")

	// Session A: Python
	outA, err := runA("debug", pyScript, "--break", pyScript+":3")
	if err != nil {
		t.Fatalf("session A debug failed: %v\n%s", err, outA)
	}
	if !strings.Contains(outA, "Stopped: breakpoint") {
		t.Errorf("session A: expected breakpoint stop, got:\n%s", outA)
	}

	// Session B: Go
	outB, err := runB("debug", goScript, "--break", goScript+":8")
	if err != nil {
		t.Fatalf("session B debug failed: %v\n%s", err, outB)
	}
	if !strings.Contains(outB, "Stopped: breakpoint") {
		t.Errorf("session B: expected breakpoint stop, got:\n%s", outB)
	}

	// Both sessions should be independent: step A, verify B unaffected
	outA, err = runA("step")
	if err != nil {
		t.Fatalf("session A step failed: %v\n%s", err, outA)
	}
	if !strings.Contains(outA, "Stopped: step") {
		t.Errorf("session A: expected step stop, got:\n%s", outA)
	}

	// B should still be at breakpoint context
	outB, err = runB("context")
	if err != nil {
		t.Fatalf("session B context failed: %v\n%s", err, outB)
	}
	if !strings.Contains(outB, "x (int) = 1") {
		t.Errorf("session B: expected x=1 at breakpoint, got:\n%s", outB)
	}

	// Stop A, verify B still works
	outA, err = runA("stop")
	if err != nil {
		t.Fatalf("session A stop failed: %v\n%s", err, outA)
	}

	outB, err = runB("step")
	if err != nil {
		t.Fatalf("session B step after A stopped: %v\n%s", err, outB)
	}
	if !strings.Contains(outB, "Stopped: step") {
		t.Errorf("session B: expected step after A stopped, got:\n%s", outB)
	}

	// Clean up B
	_, _ = runB("stop")
}

// TestE2E_IdleTimeout verifies daemon exits after idle timeout.
func TestE2E_IdleTimeout(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "dap")
	build := exec.Command("go", "build", "-o", binary, "./cmd/dap")
	build.Dir = projectRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %s\n%s", err, out)
	}

	socketPath := filepath.Join(t.TempDir(), "idle.sock")

	// Start daemon with short idle timeout via env var
	daemon := exec.Command(binary, "__daemon", "--socket", socketPath)
	daemon.Env = append(os.Environ(), "DAP_IDLE_TIMEOUT=1s")
	daemon.Stdout = os.Stderr
	daemon.Stderr = os.Stderr
	if err := daemon.Start(); err != nil {
		t.Fatalf("starting daemon: %v", err)
	}

	// Wait for socket
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Verify daemon is running
	conn, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		t.Fatalf("daemon not reachable: %v", err)
	}
	_ = conn.Close()

	// Wait for idle timeout to fire (1s + buffer)
	time.Sleep(3 * time.Second)

	// Daemon should have exited — socket should be gone or unreachable
	_, err = net.DialTimeout("unix", socketPath, 500*time.Millisecond)
	if err == nil {
		t.Errorf("daemon still running after idle timeout")
	}

	_ = daemon.Wait() // reap
}

// TestE2E_BreakpointVerification tests that unverified breakpoints produce warnings.
func TestE2E_BreakpointVerification(t *testing.T) {
	if err := exec.Command("python3", "-c", "import debugpy").Run(); err != nil {
		t.Skip("debugpy not installed")
	}

	env := newE2EEnv(t)
	scriptPath := filepath.Join(projectRoot(t), "testdata", "python", "simple.py")

	// Debug with breakpoint at line 999 (doesn't exist) + valid breakpoint at line 2
	out, err := env.run("debug", scriptPath, "--break", scriptPath+":999", "--break", scriptPath+":2")
	if err != nil {
		t.Fatalf("debug failed: %v\n%s", err, out)
	}

	// Should still stop at the valid breakpoint
	if !strings.Contains(out, "Stopped: breakpoint") {
		t.Errorf("expected breakpoint stop, got:\n%s", out)
	}

	// Should contain a warning about the adjusted breakpoint (line 999 → last valid line)
	if !strings.Contains(out, "Warnings:") {
		t.Errorf("expected Warnings section for line 999, got:\n%s", out)
	}
	if !strings.Contains(out, "999") {
		t.Errorf("expected warning mentioning line 999, got:\n%s", out)
	}
	if !strings.Contains(out, "adjusted") {
		t.Errorf("expected 'adjusted' in warning, got:\n%s", out)
	}

	// Stop
	out, err = env.run("stop")
	if err != nil {
		t.Fatalf("stop failed: %v\n%s", err, out)
	}
}

// --- Helpers ---

func projectRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find project root")
		}
		dir = parent
	}
}

// waitForListening checks if a port is in LISTEN state without connecting to it.
// This avoids poisoning single-client servers like debugpy.
func waitForListening(port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for time.Now().Before(deadline) {
		// Use lsof to check if the port is in LISTEN state
		out, err := exec.Command("lsof", "-i", fmt.Sprintf("TCP:%d", port), "-sTCP:LISTEN", "-t").Output()
		if err == nil && len(strings.TrimSpace(string(out))) > 0 {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s to be listening", addr)
}

func findFreePortForTest(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding free port: %v", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}
