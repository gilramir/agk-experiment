package diagnose

import (
	"fmt"
	"strings"

	"github.com/gilbertr/testdiag/internal/jenkins"
	"github.com/gilbertr/testdiag/internal/mapping"
)

// systemPrompt instructs the agent how to behave. It is deliberately explicit
// about NOT trying to read everything, because both the failure log and the
// source files can be very large — the whole point of the line/grep/wc tools is
// to let the model page through them instead of dumping them into context.
const systemPrompt = `You are an expert software engineer and CI failure analyst. Your task is to determine the ROOT CAUSE of ONE failing automated test, then report it.

You have read-only tools to explore the source workspace the test ran against:
- list_directory(path): list a directory's entries.
- count_lines(paths): line counts (like wc -l) for one or more files — use this to size a file BEFORE reading it.
- read_lines(path, start, end): read a single line or an inclusive range.
- grep(path, pattern, ignore_case): find matching lines (with line numbers) in a file.
- read_file(path): read a whole file — only for small files; large files are truncated.

The complete failure log has been saved to a file in the workspace; you are given its path. Treat it like any other large file: grep it for the first error and read_lines around the interesting parts rather than expecting it all inline.

Method:
1. Find the FIRST genuine error / assertion / exception, not downstream noise it caused.
2. From the stack trace, locate the relevant source files. Run count_lines first; for large files grep for the symbol, then read_lines around it. Read only what you need.
3. Form a hypothesis about the underlying defect, then verify it against the code you read.
4. Separate the ROOT CAUSE (the underlying defect or condition) from the SYMPTOM (the assertion that happened to fire).

Rules:
- Cite evidence: reference the actual file paths and line numbers you read.
- Do not invent code you have not read. If the cause is genuinely ambiguous, give the most likely cause and say what would confirm it.
- When finished, STOP calling tools and reply with your final analysis only, as Markdown with exactly these sections:
## Summary
## Evidence
## Root Cause
## Suggested Fix`

// excerptHead/Tail control how much of the log is inlined into the first
// message. The rest is reachable through the file tools on the saved log.
const (
	excerptHead = 150
	excerptTail = 100
)

// buildUserPrompt assembles the first user message for a single failing test.
func buildUserPrompt(test jenkins.FailedTest, m mapping.Result, logPath, logExcerpt, background string) string {
	var b strings.Builder

	b.WriteString("Diagnose the root cause of this failing test.\n\n")

	b.WriteString("## Failing test\n")
	fmt.Fprintf(&b, "- Name: %s\n", test.FullName())
	if test.Status != "" {
		fmt.Fprintf(&b, "- Status: %s\n", test.Status)
	}
	if test.ReportURL != "" {
		fmt.Fprintf(&b, "- Jenkins report: %s\n", test.ReportURL)
	}
	if m.SourceFile != "" {
		fmt.Fprintf(&b, "- Likely source file: %s\n", m.SourceFile)
	}
	if m.Notes != "" {
		fmt.Fprintf(&b, "- Mapping note: %s\n", m.Notes)
	}
	b.WriteString("\n")

	if strings.TrimSpace(test.ErrorDetails) != "" {
		b.WriteString("## Error details\n```\n")
		b.WriteString(strings.TrimSpace(test.ErrorDetails))
		b.WriteString("\n```\n\n")
	}

	fmt.Fprintf(&b, "## Full failure log\n")
	fmt.Fprintf(&b, "The complete log is saved at workspace path `%s` — use grep/read_lines/count_lines on it to navigate.\n", logPath)
	b.WriteString("An excerpt (head + tail) follows:\n\n```\n")
	b.WriteString(logExcerpt)
	b.WriteString("\n```\n\n")

	if strings.TrimSpace(background) != "" {
		b.WriteString("## Project background (from TEST_AGENT.md)\n")
		b.WriteString(strings.TrimSpace(background))
		b.WriteString("\n\n")
	}

	b.WriteString("Begin by locating the first real error, then trace it into the source. Produce the Markdown report when you are confident.")
	return b.String()
}

// makeExcerpt returns the head and tail of log joined with an elision marker,
// so very large logs don't blow up the first message.
func makeExcerpt(log string) string {
	lines := strings.Split(log, "\n")
	if len(lines) <= excerptHead+excerptTail {
		return log
	}
	head := lines[:excerptHead]
	tail := lines[len(lines)-excerptTail:]
	omitted := len(lines) - excerptHead - excerptTail
	var b strings.Builder
	b.WriteString(strings.Join(head, "\n"))
	fmt.Fprintf(&b, "\n... [%d lines omitted — read the saved log file for the full output] ...\n", omitted)
	b.WriteString(strings.Join(tail, "\n"))
	return b.String()
}
