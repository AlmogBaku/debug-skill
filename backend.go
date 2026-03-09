package dap

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Backend abstracts the debugger-specific logic for spawning a DAP server
// and building launch/attach argument maps.
type Backend interface {
	Spawn(port string) (cmd *exec.Cmd, addr string, err error)
	TransportMode() string
	AdapterID() string
	LaunchArgs(program string, stopOnEntry bool, args []string) (launchArgs map[string]any, cleanup func(), err error)
	AttachArgs(pid int) (map[string]any, error)
	RemoteAttachArgs(host string, port int) (map[string]any, error)
	// StopOnEntryBreakpoint returns a function name to use as a breakpoint
	// for stop-on-entry behavior. If empty, native stopOnEntry is used.
	StopOnEntryBreakpoint() string
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
	if !strings.HasPrefix(port, ":") {
		port = ":" + port
	}
	actualPort := strings.TrimPrefix(port, ":")

	// Use debugpy.adapter in debugServer mode (standalone DAP server).
	// This is how VS Code launches debugpy - it listens for DAP connections on TCP.
	cmd := exec.Command("python3", "-m", "debugpy.adapter", "--host", "127.0.0.1", "--port", actualPort, "--log-stderr")
	cmd.Stdout = nil

	// Capture stderr to detect readiness ("Listening for incoming Client connections")
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, "", fmt.Errorf("creating stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("starting debugpy adapter: %w", err)
	}

	// Wait for "Listening" message on stderr
	scanner := bufio.NewScanner(stderrPipe)
	ready := make(chan struct{})
	go func() {
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "Listening") {
				close(ready)
				// Keep draining to avoid blocking the adapter
				for scanner.Scan() {
				}
				return
			}
		}
		// If scanner ends without finding "Listening", close ready anyway
		select {
		case <-ready:
		default:
			close(ready)
		}
	}()

	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, "", fmt.Errorf("debugpy adapter did not start within 10s")
	}

	return cmd, "127.0.0.1:" + actualPort, nil
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

func (b *debugpyBackend) AttachArgs(pid int) (map[string]any, error) {
	return map[string]any{
		"request":   "attach",
		"processId": pid,
	}, nil
}

func (b *debugpyBackend) RemoteAttachArgs(host string, port int) (map[string]any, error) {
	return map[string]any{
		"request":    "attach",
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

	if !strings.HasPrefix(port, ":") {
		port = ":" + port
	}

	cmd := exec.Command("dlv", "dap", "--listen", port)
	cmd.Stderr = nil

	// Capture stdout to detect "DAP server listening at:"
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "", fmt.Errorf("creating stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("starting dlv: %w", err)
	}

	// Parse listen address from stdout
	scanner := bufio.NewScanner(stdoutPipe)
	addrCh := make(chan string, 1)
	go func() {
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "DAP server listening at:") {
				// Format: "DAP server listening at: [::]:PORT" or "DAP server listening at: 127.0.0.1:PORT"
				idx := strings.Index(line, "DAP server listening at:")
				addr := strings.TrimSpace(line[idx+len("DAP server listening at:"):])
				// Normalize [::]:PORT to 127.0.0.1:PORT
				if strings.HasPrefix(addr, "[::]") {
					addr = "127.0.0.1" + addr[4:]
				}
				addrCh <- addr
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
			return nil, "", fmt.Errorf("dlv exited without reporting listen address")
		}
		return cmd, addr, nil
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, "", fmt.Errorf("dlv did not start within 10s")
	}
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

func (b *delveBackend) AttachArgs(pid int) (map[string]any, error) {
	return map[string]any{
		"request":   "attach",
		"mode":      "local",
		"processId": pid,
	}, nil
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

// findLLDBDap searches for the lldb-dap binary, preferring newer versions.
// The Xcode Command Line Tools ship lldb-dap v17 which lacks the --connection
// flag required by dap. Homebrew llvm (v18+) is checked first on macOS.
// Returns the path or "" if not found.
func findLLDBDap() string {
	// Prefer Homebrew LLVM on macOS — Xcode CLT ships v17 which is too old.
	for _, p := range []string{
		"/opt/homebrew/opt/llvm/bin/lldb-dap",
		"/usr/local/opt/llvm/bin/lldb-dap",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	// Fall back to whatever is in PATH (may be old Xcode CLT version)
	if p, err := exec.LookPath("lldb-dap"); err == nil {
		return p
	}
	if _, err := os.Stat("/usr/bin/lldb-dap"); err == nil {
		return "/usr/bin/lldb-dap"
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

	if !strings.HasPrefix(port, ":") {
		port = ":" + port
	}
	actualPort := strings.TrimPrefix(port, ":")

	cmd := exec.Command(binary, "--connection", fmt.Sprintf("listen://127.0.0.1:%s", actualPort))

	// Capture stdout to detect "Listening for: connection://..."
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "", fmt.Errorf("creating stdout pipe: %w", err)
	}
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("starting lldb-dap: %w", err)
	}

	// Parse listen address from stdout
	scanner := bufio.NewScanner(stdoutPipe)
	addrCh := make(chan string, 1)
	go func() {
		for scanner.Scan() {
			line := scanner.Text()
			// Format: "Listening for: connection://[127.0.0.1]:PORT"
			if strings.Contains(line, "Listening") {
				// Extract host:port from the connection URL
				if idx := strings.Index(line, "connection://"); idx >= 0 {
					raw := line[idx+len("connection://"):]
					// Handle bracketed addresses like [127.0.0.1]:PORT
					raw = strings.ReplaceAll(raw, "[", "")
					raw = strings.ReplaceAll(raw, "]", "")
					addrCh <- raw
				} else {
					addrCh <- "127.0.0.1:" + actualPort
				}
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
			return nil, "", fmt.Errorf("lldb-dap exited without reporting listen address")
		}
		return cmd, addr, nil
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, "", fmt.Errorf("lldb-dap did not start within 10s")
	}
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

func (b *lldbBackend) AttachArgs(pid int) (map[string]any, error) {
	return map[string]any{
		"pid": pid,
	}, nil
}

func (b *lldbBackend) RemoteAttachArgs(host string, port int) (map[string]any, error) {
	return nil, fmt.Errorf("lldb-dap does not support remote attach")
}

// --- js-debug backend (Node.js/TypeScript) ---

type jsDebugBackend struct{}

func (b *jsDebugBackend) Spawn(port string) (*exec.Cmd, string, error) {
	serverPath := FindJSDebugServer()
	if serverPath == "" {
		return nil, "", fmt.Errorf("js-debug not found.\n" +
			"Install standalone (no VS Code needed):\n" +
			"  VERSION=$(curl -fsSL https://api.github.com/repos/microsoft/vscode-js-debug/releases/latest | grep '\"tag_name\"' | sed 's/.*\"v\\([^\"]*\\)\".*/\\1/')\n" +
			"  mkdir -p ~/.dap-cli/js-debug && curl -fsSL \"https://github.com/microsoft/vscode-js-debug/releases/download/v${VERSION}/js-debug-dap-v${VERSION}.tar.gz\" | tar -xz -C ~/.dap-cli/js-debug\n" +
			"Or set DAP_JS_DEBUG_PATH to an existing dapDebugServer.js, or install VS Code.")
	}

	if !strings.HasPrefix(port, ":") {
		port = ":" + port
	}
	actualPort := strings.TrimPrefix(port, ":")

	cmd := exec.Command("node", serverPath, actualPort)

	// Capture stdout to detect "Listening at HOST:PORT"
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "", fmt.Errorf("creating stdout pipe: %w", err)
	}
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("starting js-debug: %w", err)
	}

	scanner := bufio.NewScanner(stdoutPipe)
	addrCh := make(chan string, 1)
	go func() {
		for scanner.Scan() {
			line := scanner.Text()
			// js-debug prints: "Debug server listening at HOST:PORT"
			// e.g. "Debug server listening at ::1:12345"
			if strings.Contains(line, "Debug server listening at") {
				parts := strings.Fields(line)
				addr := "127.0.0.1:" + actualPort
				if len(parts) > 0 {
					raw := parts[len(parts)-1]
					// Extract port from the last colon (handles IPv6 like "::1:PORT")
					if idx := strings.LastIndex(raw, ":"); idx >= 0 {
						port := raw[idx+1:]
						host := raw[:idx]
						// Normalize: use the original host but bracket IPv6
						if strings.Contains(host, ":") {
							addr = "[" + host + "]:" + port
						} else if host == "" {
							addr = "127.0.0.1:" + port
						} else {
							addr = host + ":" + port
						}
					}
				}
				addrCh <- addr
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
			return nil, "", fmt.Errorf("js-debug exited without reporting listen address")
		}
		return cmd, addr, nil
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, "", fmt.Errorf("js-debug did not start within 10s")
	}
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

func (b *jsDebugBackend) AttachArgs(pid int) (map[string]any, error) {
	return map[string]any{
		"type":      "pwa-node",
		"request":   "attach",
		"processId": pid,
	}, nil
}

func (b *jsDebugBackend) RemoteAttachArgs(host string, port int) (map[string]any, error) {
	return map[string]any{
		"type":    "pwa-node",
		"request": "attach",
		"address": host,
		"port":    port,
	}, nil
}
