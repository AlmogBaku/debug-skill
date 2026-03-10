package dap

import (
	"encoding/json"
	"fmt"
	"strings"
)

// FormatText formats a ContextResult as human-readable text.
func FormatText(r *ContextResult) string {
	if r == nil {
		return ""
	}

	var b strings.Builder

	// Break list result (special case)
	if r.IsBreakList {
		if len(r.Breakpoints) > 0 {
			b.WriteString("Breakpoints:\n")
			for _, bp := range r.Breakpoints {
				fmt.Fprintf(&b, "  %s\n", bp.String())
			}
		} else {
			b.WriteString("Breakpoints: (none)\n")
		}
		if len(r.ExceptionFilters) > 0 {
			b.WriteString("\nException filters:\n")
			for _, f := range r.ExceptionFilters {
				fmt.Fprintf(&b, "  %s\n", f)
			}
		} else {
			b.WriteString("\nException filters: (none)\n")
		}
		return b.String()
	}

	// Eval result (special case)
	if r.EvalResult != nil {
		if r.EvalResult.Type != "" {
			fmt.Fprintf(&b, "%s (type: %s)\n", r.EvalResult.Value, r.EvalResult.Type)
		} else {
			fmt.Fprintf(&b, "%s\n", r.EvalResult.Value)
		}
		return b.String()
	}

	// Status line
	if r.Reason != "" {
		fmt.Fprintf(&b, "Stopped: %s\n", r.Reason)
	}

	// Location
	if r.Location != nil {
		if r.Location.Function != "" {
			fmt.Fprintf(&b, "Function: %s\n", r.Location.Function)
		}
		if r.Location.File != "" {
			fmt.Fprintf(&b, "File: %s:%d\n", r.Location.File, r.Location.Line)
		}
	}

	// Source
	if len(r.Source) > 0 {
		b.WriteString("\nSource:\n")
		for _, line := range r.Source {
			marker := " "
			if line.Current {
				marker = ">"
			}
			fmt.Fprintf(&b, "  %3d%s| %s\n", line.Line, marker, line.Text)
		}
	}

	// Locals
	if len(r.Locals) > 0 {
		b.WriteString("\nLocals:\n")
		for _, v := range r.Locals {
			if v.Type != "" {
				fmt.Fprintf(&b, "  %s (%s) = %s\n", v.Name, v.Type, v.Value)
			} else {
				fmt.Fprintf(&b, "  %s = %s\n", v.Name, v.Value)
			}
		}
	}

	// Stack
	if len(r.Stack) > 0 {
		b.WriteString("\nStack:\n")
		for _, f := range r.Stack {
			if f.File != "" {
				fmt.Fprintf(&b, "  #%d %s at %s:%d\n", f.Frame, f.Function, f.File, f.Line)
			} else {
				fmt.Fprintf(&b, "  #%d %s\n", f.Frame, f.Function)
			}
		}
	}

	// Output
	if r.Output != "" {
		b.WriteString("\nOutput:\n")
		for _, line := range strings.Split(strings.TrimRight(r.Output, "\n"), "\n") {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}

	// Warnings
	if len(r.Warnings) > 0 {
		b.WriteString("\nWarnings:\n")
		for _, w := range r.Warnings {
			fmt.Fprintf(&b, "  ⚠ %s\n", w)
		}
	}

	return b.String()
}

// FormatJSON formats a ContextResult as JSON.
func FormatJSON(r *ContextResult) string {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"error": %q}`, err.Error())
	}
	return string(data)
}

// FormatResponse formats a Response for CLI output.
func FormatResponse(resp *Response, jsonOutput bool) string {
	switch resp.Status {
	case "error":
		return fmt.Sprintf("Error: %s\n", resp.Error)
	case "terminated":
		var b strings.Builder
		b.WriteString("Program terminated\n")
		if resp.Data != nil {
			if resp.Data.ExitCode != nil {
				fmt.Fprintf(&b, "Exit code: %d\n", *resp.Data.ExitCode)
			}
			if resp.Data.Output != "" {
				b.WriteString("Output:\n")
				for _, line := range strings.Split(strings.TrimRight(resp.Data.Output, "\n"), "\n") {
					fmt.Fprintf(&b, "  %s\n", line)
				}
			}
			if len(resp.Data.Warnings) > 0 {
				b.WriteString("\nWarnings:\n")
				for _, w := range resp.Data.Warnings {
					fmt.Fprintf(&b, "  ⚠ %s\n", w)
				}
			}
		}
		return b.String()
	case "ok":
		if resp.Data != nil {
			if jsonOutput {
				return FormatJSON(resp.Data)
			}
			return FormatText(resp.Data)
		}
		return "OK\n"
	case "stopped":
		if resp.Data == nil {
			return "Stopped\n"
		}
		if jsonOutput {
			return FormatJSON(resp.Data)
		}
		return FormatText(resp.Data)
	default:
		if jsonOutput && resp.Data != nil {
			return FormatJSON(resp.Data)
		}
		if resp.Data != nil {
			return FormatText(resp.Data)
		}
		return fmt.Sprintf("Status: %s\n", resp.Status)
	}
}
