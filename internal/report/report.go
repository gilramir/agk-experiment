// Package report writes a Markdown root-cause report for a diagnosed test.
package report

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gilbertr/testdiag/internal/diagnose"
)

// Write writes a single diagnosis as a Markdown file under outDir and returns
// the path written.
func Write(outDir string, r diagnose.Result) (string, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	name := sanitizeFilename(r.Test.FullName()) + ".md"
	path := filepath.Join(outDir, name)
	if err := os.WriteFile(path, []byte(render(r)), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func render(r diagnose.Result) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# Test failure root cause: %s\n\n", r.Test.FullName())

	b.WriteString("| | |\n|---|---|\n")
	fmt.Fprintf(&b, "| Test | `%s` |\n", r.Test.FullName())
	if r.Test.Status != "" {
		fmt.Fprintf(&b, "| Status | %s |\n", r.Test.Status)
	}
	if r.Mapping.SourceFile != "" {
		fmt.Fprintf(&b, "| Source file | `%s` |\n", r.Mapping.SourceFile)
	}
	if r.LogPath != "" {
		fmt.Fprintf(&b, "| Saved log | `%s` |\n", r.LogPath)
	}
	if r.Test.ReportURL != "" {
		fmt.Fprintf(&b, "| Jenkins report | %s |\n", r.Test.ReportURL)
	}
	fmt.Fprintf(&b, "| Analyzed | %s |\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(&b, "| Diagnosis time | %s |\n", r.Duration.Round(time.Millisecond))
	b.WriteString("\n---\n\n")

	body := strings.TrimSpace(r.RootCause)
	if body == "" {
		body = "_The agent produced no analysis._"
	}
	b.WriteString(body)
	b.WriteString("\n")

	if len(r.ToolsCalled) > 0 {
		fmt.Fprintf(&b, "\n---\n\n_Tools used: %s_\n", strings.Join(r.ToolsCalled, ", "))
	}
	return b.String()
}

func sanitizeFilename(s string) string {
	repl := func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}
	out := strings.Map(repl, s)
	if len(out) > 180 {
		out = out[:180]
	}
	if out == "" {
		return "test"
	}
	return out
}
