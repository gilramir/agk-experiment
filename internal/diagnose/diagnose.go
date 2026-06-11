// Package diagnose runs a single failing test through an AgenticGoKit agent
// that uses the provider's native tool-calling loop to read workspace files and
// determine the root cause.
package diagnose

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	vnext "github.com/agenticgokit/agenticgokit/v1beta"

	"github.com/gilbertr/testdiag/internal/config"
	"github.com/gilbertr/testdiag/internal/jenkins"
	"github.com/gilbertr/testdiag/internal/mapping"
	"github.com/gilbertr/testdiag/internal/workspace"
)

// logDir is where fetched failure logs are written, relative to the workspace
// root, so the jailed file tools can read them.
const logDir = ".testdiag/logs"

// maxToolIterations caps the native tool-calling loop per test.
const maxToolIterations = 20

// Result is the outcome of diagnosing one test.
type Result struct {
	Test        jenkins.FailedTest
	Mapping     mapping.Result
	LogPath     string   // workspace-relative path to the saved log
	RootCause   string   // the agent's Markdown analysis
	ToolsCalled []string // tools the agent invoked (for the report footer)
	Duration    time.Duration
}

// Diagnoser diagnoses tests against a fixed workspace and LLM config.
type Diagnoser struct {
	cfg        *config.Config
	ws         *workspace.Workspace
	background string // contents of TEST_AGENT.md
}

// New creates a Diagnoser. background is the TEST_AGENT.md content (may be "").
func New(cfg *config.Config, ws *workspace.Workspace, background string) *Diagnoser {
	return &Diagnoser{cfg: cfg, ws: ws, background: background}
}

// Diagnose maps the test to its source, persists its log, builds a fresh agent
// (per-test independence), and runs the native tool-calling loop to completion.
func (d *Diagnoser) Diagnose(ctx context.Context, test jenkins.FailedTest) (Result, error) {
	start := time.Now()

	m, err := mapping.MapTestToSource(d.ws.Root(), test)
	if err != nil {
		return Result{}, fmt.Errorf("mapping %s: %w", test.FullName(), err)
	}

	logRel, err := d.saveLog(test)
	if err != nil {
		return Result{}, fmt.Errorf("saving log for %s: %w", test.FullName(), err)
	}

	agent, err := d.buildAgent(test)
	if err != nil {
		return Result{}, fmt.Errorf("building agent for %s: %w", test.FullName(), err)
	}

	excerpt := makeExcerpt(combinedLog(test))
	prompt := buildUserPrompt(test, m, logRel, excerpt, d.background)

	res, err := agent.Run(ctx, prompt)
	if err != nil {
		return Result{}, fmt.Errorf("agent run for %s: %w", test.FullName(), err)
	}

	return Result{
		Test:        test,
		Mapping:     m,
		LogPath:     logRel,
		RootCause:   res.Content,
		ToolsCalled: res.ToolsCalled,
		Duration:    time.Since(start),
	}, nil
}

// buildAgent constructs a fresh agent for one test. Memory is disabled so each
// diagnosis is fully independent; reasoning is enabled so the agent loops:
// call LLM -> execute tools -> feed results back -> repeat.
func (d *Diagnoser) buildAgent(test jenkins.FailedTest) (vnext.Agent, error) {
	name := "diagnose-" + sanitize(test.FullName())
	return vnext.NewBuilder(name).
		WithConfig(&vnext.Config{
			Name:         name,
			SystemPrompt: systemPrompt,
			LLM: vnext.LLMConfig{
				Provider:    d.cfg.LLM.Provider,
				Model:       d.cfg.LLM.Model,
				BaseURL:     d.cfg.LLM.BaseURL,
				APIKey:      d.cfg.LLM.APIKey,
				Temperature: d.cfg.LLM.Temperature,
				MaxTokens:   d.cfg.LLM.MaxTokens,
			},
			Tools: &vnext.ToolsConfig{
				Enabled: true,
				Reasoning: &vnext.ReasoningConfig{
					Enabled:           true,
					MaxIterations:     maxToolIterations,
					ContinueOnToolUse: true,
				},
			},
			Memory:  &vnext.MemoryConfig{Enabled: false},
			Timeout: 10 * time.Minute,
		}).
		WithPreset(vnext.ChatAgent). // makes the registered internal tools available
		Build()
}

// saveLog writes the test's combined output under <root>/.testdiag/logs and
// returns the workspace-relative path (so the jailed tools can open it).
func (d *Diagnoser) saveLog(test jenkins.FailedTest) (string, error) {
	dir := filepath.Join(d.ws.Root(), logDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	rel := filepath.Join(logDir, sanitize(test.FullName())+".log")
	abs := filepath.Join(d.ws.Root(), rel)
	if err := os.WriteFile(abs, []byte(combinedLog(test)), 0o644); err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

// combinedLog assembles the full failure output the way a developer would see
// it: error summary, stack trace, then captured stdout/stderr.
func combinedLog(test jenkins.FailedTest) string {
	var b strings.Builder
	section := func(title, body string) {
		if strings.TrimSpace(body) == "" {
			return
		}
		fmt.Fprintf(&b, "===== %s =====\n%s\n\n", title, strings.TrimRight(body, "\n"))
	}
	section("ERROR DETAILS", test.ErrorDetails)
	section("STACK TRACE", test.ErrorStackTrace)
	section("STDOUT", test.Stdout)
	section("STDERR", test.Stderr)
	if b.Len() == 0 {
		return "(no failure output was provided by Jenkins for this test)\n"
	}
	return b.String()
}

// sanitize makes a test name safe to use as a single filename segment.
func sanitize(s string) string {
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
