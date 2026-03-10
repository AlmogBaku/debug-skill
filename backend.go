package dap

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// waitForReady scans pipe lines for readyString, extracts an address via parseAddr,
// and kills cmd on timeout (10s) or early exit. Caller must have already started cmd.
func waitForReady(cmd *exec.Cmd, pipe io.ReadCloser, readyString string, parseAddr func(line string) string) (string, error) {
	scanner := bufio.NewScanner(pipe)
	addrCh := make(chan string, 1)
	go func() {
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, readyString) {
				addrCh <- parseAddr(line)
				for scanner.Scan() {
				}
				return
			}
		}
		close(addrCh)
	}()

	select {
	case addr, ok := <-addrCh:
		if !ok || addr == "" {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return "", fmt.Errorf("process exited without reporting listen address")
		}
		return addr, nil
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return "", fmt.Errorf("process did not start within 10s")
	}
}

// normalizePort ensures port has a ":" prefix and returns both forms.
func normalizePort(port string) (withColon, bare string) {
	if !strings.HasPrefix(port, ":") {
		port = ":" + port
	}
	return port, strings.TrimPrefix(port, ":")
}

// Backend abstracts the debugger-specific logic for spawning a DAP server
// and building launch/attach argument maps.
type Backend interface {
	Spawn(port string) (cmd *exec.Cmd, addr string, err error)
	TransportMode() string
	AdapterID() string
	LaunchArgs(program string, stopOnEntry bool, args []string) (launchArgs map[string]any, cleanup func(), err error)
	RemoteAttachArgs(host string, port int) (map[string]any, error)
	// StopOnEntryBreakpoint returns a function name to use as a breakpoint
	// for stop-on-entry behavior. If empty, native stopOnEntry is used.
	StopOnEntryBreakpoint() string
	// PIDAttachArgs returns attach arguments for attaching to a local process by PID.
	PIDAttachArgs(pid int) (map[string]any, error)
}

// DetectBackend returns the appropriate backend based on file extension.
func DetectBackend(script string) Backend {
	switch strings.ToLower(filepath.Ext(script)) {
	case ".py":
		return &debugpyBackend{}
	case ".go":
		return &delveBackend{}
	case ".js", ".ts", ".mjs", ".cjs":
		return &jsDebugBackend{}
	case ".rs", ".c", ".cpp", ".cc":
		return &lldbBackend{}
	default:
		return &debugpyBackend{} // default to debugpy
	}
}

// GetBackendByName returns a backend by name.
func GetBackendByName(name string) (Backend, error) {
	switch name {
	case "debugpy":
		return &debugpyBackend{}, nil
	case "dlv", "delve":
		return &delveBackend{}, nil
	case "js-debug":
		return &jsDebugBackend{}, nil
	case "lldb", "lldb-dap":
		return &lldbBackend{}, nil
	default:
		return nil, fmt.Errorf("unknown backend %q — valid options: debugpy, dlv, js-debug, lldb-dap", name)
	}
}

// --- debugpy backend (Python) ---

type debugpyBackend struct{}

func (b *debugpyBackend) Spawn(port string) (*exec.Cmd, string, error) {
	_, actualPort := normalizePort(port)

	cmd := exec.Command("python3", "-m", "debugpy.adapter", "--host", "127.0.0.1", "--port", actualPort, "--log-stderr")
	cmd.Stdout = nil

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, "", fmt.Errorf("creating stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("starting debugpy: %w", err)
	}

	addr, err := waitForReady(cmd, stderrPipe, "Listening", func(string) string {
		return "127.0.0.1:" + actualPort
	})
	if err != nil {
		return nil, "", fmt.Errorf("starting debugpy: %w", err)
	}
	return cmd, addr, nil
}

func (b *debugpyBackend) TransportMode() string         { return "tcp" }
func (b *debugpyBackend) AdapterID() string             { return "debugpy" }
func (b *debugpyBackend) StopOnEntryBreakpoint() string { return "" }

func (b *debugpyBackend) LaunchArgs(program string, stopOnEntry bool, args []string) (map[string]any, func(), error) {
	absProgram, err := filepath.Abs(program)
	if err != nil {
		return nil, nil, fmt.Errorf("resolving path: %w", err)
	}
	cwd, _ := os.Getwd()
	m := map[string]any{
		"request":     "launch",
		"program":     absProgram,
		"stopOnEntry": stopOnEntry,
		"console":     "internalConsole",
		"cwd":         cwd,
		"justMyCode":  false,
	}
	if len(args) > 0 {
		m["args"] = args
	}
	return m, nil, nil
}

func (b *debugpyBackend) RemoteAttachArgs(host string, port int) (map[string]any, error) {
	return map[string]any{
		"request":    "attach",
		"justMyCode": false,
	}, nil
}

func (b *debugpyBackend) PIDAttachArgs(pid int) (map[string]any, error) {
	return map[string]any{
		"request":    "attach",
		"processId":  pid,
		"justMyCode": false,
	}, nil
}

// --- delve backend (Go) ---

type delveBackend struct{}

func (b *delveBackend) Spawn(port string) (*exec.Cmd, string, error) {
	if runtime.GOOS == "darwin" {
		if err := checkMacOSDevMode(); err != nil {
			return nil, "", err
		}
	}

	port, _ = normalizePort(port)

	cmd := exec.Command("dlv", "dap", "--listen", port)
	cmd.Stderr = nil

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "", fmt.Errorf("creating stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("starting dlv: %w", err)
	}

	addr, err := waitForReady(cmd, stdoutPipe, "DAP server listening at:", func(line string) string {
		idx := strings.Index(line, "DAP server listening at:")
		a := strings.TrimSpace(line[idx+len("DAP server listening at:"):])
		if strings.HasPrefix(a, "[::]") {
			a = "127.0.0.1" + a[4:]
		}
		return a
	})
	if err != nil {
		return nil, "", fmt.Errorf("starting dlv: %w", err)
	}
	return cmd, addr, nil
}

func (b *delveBackend) TransportMode() string         { return "tcp" }
func (b *delveBackend) AdapterID() string             { return "go" }
func (b *delveBackend) StopOnEntryBreakpoint() string { return "main.main" }

func (b *delveBackend) LaunchArgs(program string, stopOnEntry bool, args []string) (map[string]any, func(), error) {
	absProgram, err := filepath.Abs(program)
	if err != nil {
		return nil, nil, fmt.Errorf("resolving path: %w", err)
	}

	info, err := os.Stat(absProgram)
	if err != nil {
		return nil, nil, fmt.Errorf("stat: %w", err)
	}

	isGoSource := info.IsDir() || filepath.Ext(absProgram) == ".go"

	var cleanupFn func()
	if isGoSource {
		// Pre-compile Go source with debug symbols, then use exec mode.
		// This avoids module resolution issues in dlv DAP mode.
		pkgDir := absProgram
		if !info.IsDir() {
			pkgDir = filepath.Dir(absProgram)
		}
		tmpBin, err := os.CreateTemp("", "dap-dlv-*")
		if err != nil {
			return nil, nil, fmt.Errorf("creating temp file: %w", err)
		}
		_ = tmpBin.Close()

		build := exec.Command("go", "build", "-gcflags=all=-N -l", "-o", tmpBin.Name(), ".")
		build.Dir = pkgDir
		if out, err := build.CombinedOutput(); err != nil {
			_ = os.Remove(tmpBin.Name())
			return nil, nil, fmt.Errorf("compiling Go program: %s\n%s", err, out)
		}
		absProgram = tmpBin.Name()
		cleanupFn = func() { _ = os.Remove(absProgram) }
	}

	m := map[string]any{
		"request":     "launch",
		"mode":        "exec",
		"program":     absProgram,
		"stopOnEntry": false, // dlv exec mode can't stop before runtime init; use function breakpoints instead
	}
	if len(args) > 0 {
		m["args"] = args
	}
	return m, cleanupFn, nil
}

func (b *delveBackend) RemoteAttachArgs(host string, port int) (map[string]any, error) {
	return map[string]any{
		"request":        "attach",
		"mode":           "remote",
		"host":           host,
		"port":           port,
		"substitutePath": []any{},
	}, nil
}

// PIDAttachArgs for dlv uses "launch" with "local" mode (not "attach"),
// because dlv's DAP API requires this for local process attachment.
func (b *delveBackend) PIDAttachArgs(pid int) (map[string]any, error) {
	return map[string]any{
		"request":   "launch",
		"mode":      "local",
		"processId": pid,
	}, nil
}

// checkMacOSDevMode verifies that macOS developer mode is enabled, which is
// required for dlv to debug processes via ptrace.
func checkMacOSDevMode() error {
	out, err := exec.Command("DevToolsSecurity", "-status").CombinedOutput()
	if err != nil {
		// If the command doesn't exist or fails, skip the check
		return nil
	}
	if strings.Contains(string(out), "enabled") {
		return nil
	}
	return fmt.Errorf("macOS developer mode is disabled — dlv cannot debug programs without it.\n" +
		"Enable it by running: sudo DevToolsSecurity -enable\n" +
		"See: https://github.com/go-delve/delve/blob/master/Documentation/installation/README.md#macos-considerations")
}

// findLLDBDap searches for the lldb-dap binary.
// Returns the path or "" if not found.
func findLLDBDap() string {
	// Prefer Homebrew LLVM on macOS (Xcode CLT ships v17 which lacks --connection)
	for _, p := range []string{
		"/opt/homebrew/opt/llvm/bin/lldb-dap",
		"/usr/local/opt/llvm/bin/lldb-dap",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	// Fall back to PATH (catches Linux installs and custom locations)
	if p, err := exec.LookPath("lldb-dap"); err == nil {
		return p
	}
	return ""
}

// FindJSDebugServer searches for the js-debug DAP server script.
// Returns the path or "" if not found.
func FindJSDebugServer() string {
	// Check env var first
	if p := os.Getenv("DAP_JS_DEBUG_PATH"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	// Search VS Code and Cursor extension dirs
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	// Check ~/.dap-cli standalone install
	standalone := filepath.Join(home, ".dap-cli", "js-debug", "js-debug", "src", "dapDebugServer.js")
	if _, err := os.Stat(standalone); err == nil {
		return standalone
	}
	// Check VS Code and Cursor extension dirs
	for _, dir := range []string{
		filepath.Join(home, ".vscode", "extensions"),
		filepath.Join(home, ".cursor", "extensions"),
	} {
		matches, _ := filepath.Glob(filepath.Join(dir, "ms-vscode.js-debug-*/src/dapDebugServer.js"))
		if len(matches) > 0 {
			return matches[len(matches)-1]
		}
	}
	return ""
}

// --- lldb-dap backend (Rust/C/C++) ---

type lldbBackend struct{}

func (b *lldbBackend) Spawn(port string) (*exec.Cmd, string, error) {
	binary := findLLDBDap()
	if binary == "" {
		return nil, "", fmt.Errorf("lldb-dap not found. Install: brew install llvm (macOS) or apt install lldb (Linux)")
	}

	_, actualPort := normalizePort(port)

	cmd := exec.Command(binary, "--connection", fmt.Sprintf("listen://127.0.0.1:%s", actualPort))
	cmd.Stderr = nil

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "", fmt.Errorf("creating stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("starting lldb-dap: %w", err)
	}

	addr, err := waitForReady(cmd, stdoutPipe, "Listening", func(line string) string {
		if idx := strings.Index(line, "connection://"); idx >= 0 {
			raw := line[idx+len("connection://"):]
			raw = strings.ReplaceAll(raw, "[", "")
			raw = strings.ReplaceAll(raw, "]", "")
			return raw
		}
		return "127.0.0.1:" + actualPort
	})
	if err != nil {
		return nil, "", fmt.Errorf("starting lldb-dap: %w", err)
	}
	return cmd, addr, nil
}

func (b *lldbBackend) TransportMode() string         { return "tcp" }
func (b *lldbBackend) AdapterID() string             { return "lldb-dap" }
func (b *lldbBackend) StopOnEntryBreakpoint() string { return "" } // native stopOnEntry works

func (b *lldbBackend) LaunchArgs(program string, stopOnEntry bool, args []string) (map[string]any, func(), error) {
	absProgram, err := filepath.Abs(program)
	if err != nil {
		return nil, nil, fmt.Errorf("resolving path: %w", err)
	}

	var cleanupFn func()

	ext := strings.ToLower(filepath.Ext(absProgram))
	switch ext {
	case ".rs":
		// Compile Rust source with debug symbols
		tmpBin, err := os.CreateTemp("", "dap-rust-*")
		if err != nil {
			return nil, nil, fmt.Errorf("creating temp file: %w", err)
		}
		_ = tmpBin.Close()
		build := exec.Command("rustc", "-g", "-o", tmpBin.Name(), absProgram)
		if out, err := build.CombinedOutput(); err != nil {
			_ = os.Remove(tmpBin.Name())
			return nil, nil, fmt.Errorf("compiling Rust program: %s\n%s", err, out)
		}
		absProgram = tmpBin.Name()
		cleanupFn = func() { _ = os.Remove(absProgram) }
	case ".c", ".cpp", ".cc":
		// Compile C/C++ source with debug symbols
		tmpBin, err := os.CreateTemp("", "dap-cc-*")
		if err != nil {
			return nil, nil, fmt.Errorf("creating temp file: %w", err)
		}
		_ = tmpBin.Close()
		compiler := "cc"
		if ext == ".cpp" || ext == ".cc" {
			compiler = "c++"
		}
		build := exec.Command(compiler, "-g", "-o", tmpBin.Name(), absProgram)
		if out, err := build.CombinedOutput(); err != nil {
			_ = os.Remove(tmpBin.Name())
			return nil, nil, fmt.Errorf("compiling C/C++ program: %s\n%s", err, out)
		}
		absProgram = tmpBin.Name()
		cleanupFn = func() { _ = os.Remove(absProgram) }
	}

	m := map[string]any{
		"program":     absProgram,
		"stopOnEntry": stopOnEntry,
	}
	if len(args) > 0 {
		m["args"] = args
	}
	return m, cleanupFn, nil
}

func (b *lldbBackend) RemoteAttachArgs(host string, port int) (map[string]any, error) {
	return nil, fmt.Errorf("lldb-dap does not support remote attach")
}

func (b *lldbBackend) PIDAttachArgs(pid int) (map[string]any, error) {
	return map[string]any{
		"request": "attach",
		"pid":     pid,
	}, nil
}

// --- js-debug backend (Node.js/TypeScript) ---

type jsDebugBackend struct{}

func (b *jsDebugBackend) Spawn(port string) (*exec.Cmd, string, error) {
	serverPath := FindJSDebugServer()
	if serverPath == "" {
		return nil, "", fmt.Errorf("js-debug not found. Install VS Code, set DAP_JS_DEBUG_PATH, or download from github.com/microsoft/vscode-js-debug/releases")
	}

	_, actualPort := normalizePort(port)

	cmd := exec.Command("node", serverPath, actualPort)
	cmd.Stderr = nil

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "", fmt.Errorf("creating stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("starting js-debug: %w", err)
	}

	addr, err := waitForReady(cmd, stdoutPipe, "Debug server listening at", func(line string) string {
		parts := strings.Fields(line)
		if len(parts) == 0 {
			return "127.0.0.1:" + actualPort
		}
		raw := parts[len(parts)-1]
		if idx := strings.LastIndex(raw, ":"); idx >= 0 {
			p := raw[idx+1:]
			host := raw[:idx]
			if strings.Contains(host, ":") {
				return "[" + host + "]:" + p
			} else if host == "" {
				return "127.0.0.1:" + p
			}
			return host + ":" + p
		}
		return "127.0.0.1:" + actualPort
	})
	if err != nil {
		return nil, "", fmt.Errorf("starting js-debug: %w", err)
	}
	return cmd, addr, nil
}

func (b *jsDebugBackend) TransportMode() string         { return "tcp" }
func (b *jsDebugBackend) AdapterID() string             { return "pwa-node" }
func (b *jsDebugBackend) StopOnEntryBreakpoint() string { return "" }

func (b *jsDebugBackend) LaunchArgs(program string, stopOnEntry bool, args []string) (map[string]any, func(), error) {
	absProgram, err := filepath.Abs(program)
	if err != nil {
		return nil, nil, fmt.Errorf("resolving path: %w", err)
	}
	cwd, _ := os.Getwd()
	m := map[string]any{
		"type":        "pwa-node",
		"request":     "launch",
		"program":     absProgram,
		"stopOnEntry": stopOnEntry,
		"cwd":         cwd,
	}
	if len(args) > 0 {
		m["args"] = args
	}
	return m, nil, nil
}

func (b *jsDebugBackend) RemoteAttachArgs(host string, port int) (map[string]any, error) {
	return map[string]any{
		"type":    "pwa-node",
		"request": "attach",
		"address": host,
		"port":    port,
	}, nil
}

func (b *jsDebugBackend) PIDAttachArgs(pid int) (map[string]any, error) {
	return map[string]any{
		"type":      "pwa-node",
		"request":   "attach",
		"processId": pid,
	}, nil
}
