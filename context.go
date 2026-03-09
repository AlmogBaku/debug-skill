package dap

import (
	"fmt"
	"os"
	"strings"

	godap "github.com/google/go-dap"
)

const (
	maxStackFrames     = 20
	sourceContext      = 2 // lines before/after current line
	maxStringLen       = 200
	maxCollectionItems = 5
)

// variableFilters maps adapter IDs to functions that decide if a variable should be skipped.
// Returns true if the variable should be filtered OUT.
var variableFilters = map[string]func(string) bool{
	"debugpy": func(name string) bool {
		// Skip debugpy's synthetic variable groups and dunder names
		switch name {
		case "special variables", "function variables", "class variables", "return values":
			return true
		}
		return len(name) > 4 && name[:2] == "__" && name[len(name)-2:] == "__"
	},
	"pwa-node": func(name string) bool {
		return name == "__proto__"
	},
	"lldb-dap": func(name string) bool {
		return strings.HasPrefix(name, "[raw]")
	},
	// "go": nil — show all variables
}

// getFullContext aggregates stack, source, locals, and output into a ContextResult.
// It also updates d.frameID with the target frame for subsequent eval calls.
func getFullContext(d *Daemon, threadID, frameID int) (*ContextResult, error) {
	result := &ContextResult{}

	// 1. Get stack trace
	if err := d.client.StackTraceRequest(threadID, 0, maxStackFrames); err != nil {
		return nil, fmt.Errorf("stack trace request: %w", err)
	}

	var frames []godap.StackFrame
	for {
		msg, err := d.readExpected()
		if err != nil {
			return nil, fmt.Errorf("reading stack trace: %w", err)
		}
		if resp, ok := msg.(*godap.StackTraceResponse); ok {
			if !resp.Success {
				return nil, fmt.Errorf("stack trace failed: %s", resp.Message)
			}
			frames = resp.Body.StackFrames
			break
		}
		if errResp, ok := msg.(*godap.ErrorResponse); ok {
			return nil, fmt.Errorf("stack trace error: %s", errorMessage(errResp))
		}
	}

	// 2. Build stack and location from frames; record DAP frame IDs for eval
	d.frameIDs = make([]int, len(frames))
	for i, f := range frames {
		d.frameIDs[i] = f.Id
		sf := StackFrame{
			Frame:    i,
			Function: f.Name,
		}
		if f.Source != nil {
			sf.File = f.Source.Path
			sf.Line = f.Line
		}
		result.Stack = append(result.Stack, sf)
	}

	if len(frames) > 0 {
		top := frames[0]
		result.Location = &Location{
			Function: top.Name,
			Line:     top.Line,
		}
		if top.Source != nil {
			result.Location.File = top.Source.Path
		}
	}

	// 3. Read source file around current line
	if result.Location != nil && result.Location.File != "" {
		result.Source = readSourceLines(result.Location.File, result.Location.Line, sourceContext)
	}

	// 4. Get scopes and variables for target frame
	targetFrameID := frameID
	if targetFrameID == 0 && len(frames) > 0 {
		targetFrameID = frames[0].Id
	}
	d.frameID = targetFrameID

	if err := d.client.ScopesRequest(targetFrameID); err != nil {
		return nil, fmt.Errorf("scopes request: %w", err)
	}

	var scopes []godap.Scope
	for {
		msg, err := d.readExpected()
		if err != nil {
			return nil, fmt.Errorf("reading scopes: %w", err)
		}
		if resp, ok := msg.(*godap.ScopesResponse); ok {
			if !resp.Success {
				break // non-fatal
			}
			scopes = resp.Body.Scopes
			break
		}
		if _, ok := msg.(*godap.ErrorResponse); ok {
			break // non-fatal
		}
	}

	// 5. Get variables for each scope (only "Locals" and "Arguments")
	var filterVar func(string) bool
	if d.backend != nil {
		filterVar = variableFilters[d.backend.AdapterID()]
	}

	for _, scope := range scopes {
		name := strings.ToLower(scope.Name)
		if scope.VariablesReference == 0 {
			continue
		}
		// Only fetch locals-like scopes, skip globals/builtins for brevity
		if !strings.Contains(name, "local") && !strings.Contains(name, "argument") &&
			name != "locals" && name != "arguments" {
			continue
		}

		if err := d.client.VariablesRequest(scope.VariablesReference); err != nil {
			continue
		}

		for {
			msg, err := d.readExpected()
			if err != nil {
				break
			}
			if _, ok := msg.(*godap.ErrorResponse); ok {
				break
			}
			if resp, ok := msg.(*godap.VariablesResponse); ok {
				if !resp.Success {
					break
				}
				for _, v := range resp.Body.Variables {
					if filterVar != nil && filterVar(v.Name) {
						continue
					}
					result.Locals = append(result.Locals, Variable{
						Name:  v.Name,
						Type:  v.Type,
						Value: truncateString(v.Value, maxStringLen),
					})
				}
				break
			}
		}
	}

	// 6. Include buffered output (capped at maxOutputLines) and clear
	d.mu.Lock()
	result.Output = d.outputString()
	d.mu.Unlock()

	return result, nil
}

// readSourceLines reads lines from a file around the target line.
func readSourceLines(file string, line, context int) []SourceLine {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	start := line - context - 1
	if start < 0 {
		start = 0
	}
	end := line + context
	if end > len(lines) {
		end = len(lines)
	}

	var result []SourceLine
	for i := start; i < end; i++ {
		result = append(result, SourceLine{
			Line:    i + 1,
			Text:    lines[i],
			Current: i+1 == line,
		})
	}
	return result
}

// truncateString truncates a string to maxLen characters.
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// errorMessage extracts a human-readable message from an ErrorResponse.
func errorMessage(r *godap.ErrorResponse) string {
	if r.Body.Error != nil && r.Body.Error.Format != "" {
		return r.Body.Error.Format
	}
	return r.Message
}
