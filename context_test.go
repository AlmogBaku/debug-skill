package dap

import "testing"

func TestReadSourceLines(t *testing.T) {
	lines := readSourceLines("testdata/python/simple.py", 2, 2)
	if len(lines) == 0 {
		t.Fatal("no source lines returned")
	}

	// Should have lines 1-4 (2 before line 2 = line 1, 2 after = line 4)
	// But line 2 - 2 = 0, clamped to 1
	found := false
	for _, l := range lines {
		if l.Line == 2 && l.Current {
			found = true
		}
	}
	if !found {
		t.Error("current line not marked")
	}

	// Line 1 should be "x = 1"
	if lines[0].Text != "x = 1" {
		t.Errorf("first line = %q, want %q", lines[0].Text, "x = 1")
	}
}

func TestReadSourceLines_Missing(t *testing.T) {
	lines := readSourceLines("/nonexistent/file.py", 1, 2)
	if lines != nil {
		t.Error("expected nil for missing file")
	}
}

func TestTruncateString(t *testing.T) {
	short := "hello"
	if truncateString(short, 10) != "hello" {
		t.Error("short string should not be truncated")
	}

	long := "abcdefghij"
	if truncateString(long, 5) != "abcde..." {
		t.Errorf("truncated = %q, want %q", truncateString(long, 5), "abcde...")
	}

	// Multi-byte: emoji runes should not be split
	emoji := "hello🌍🌎🌏world"
	got := truncateString(emoji, 8) // "hello🌍🌎🌏" (8 runes)
	want := "hello🌍🌎🌏..."
	if got != want {
		t.Errorf("multi-byte truncated = %q, want %q", got, want)
	}
}
