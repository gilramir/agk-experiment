// Package diagnose implements the DEEPINSPECT stage: it runs a single failing
// test through an AgenticGoKit agent that uses the provider's native
// tool-calling loop to read workspace SOURCE files and determine the root cause.
//
// Unlike earlier versions, the agent is NOT given the raw Jenkins log. It works
// only from the investigation brief produced by the LOGPARSE stage (a Markdown
// handoff naming the source/logic to find and the flakiness conditions to
// check), and the raw-log tools are hard-disabled for the duration of the run
// (see tools.SetLogToolsEnabled). This keeps the deep-tracing model focused on
// code instead of re-reading noisy log output.
package diagnose

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	vnext "github.com/agenticgokit/agenticgokit/v1beta"

	"github.com/gilbertr/testdiag/internal/config"
	"github.com/gilbertr/testdiag/internal/jenkins"
	"github.com/gilbertr/testdiag/internal/mapping"
	"github.com/gilbertr/testdiag/internal/tools"
	"github.com/gilbertr/testdiag/internal/workspace"
)

// notebookDir is where each test's investigation notebook lives, relative to the
// workspace root, so the notebook tool (jailed like the rest) can read/write it.
const notebookDir = ".testdiag/notes"

// defaultMaxToolIterations caps the native tool-calling loop within a single
// attempt when config leaves it unset. It is generous because a flaky failure
// often requires tracing across the Python client / C++ server boundary, which
// takes many reads. Override via diagnosis.max_tool_iterations.
const defaultMaxToolIterations = 50

// maxToolIterations is the per-attempt tool-call cap from config, falling back to
// the default if unset.
func (d *Diagnoser) maxToolIterations() int {
	if d.cfg.Diagnosis.MaxToolIterations > 0 {
		return d.cfg.Diagnosis.MaxToolIterations
	}
	return defaultMaxToolIterations
}

// Result is the outcome of diagnosing one test.
type Result struct {
	Test        jenkins.FailedTest
	Mapping     mapping.Result
	LogPath     string   // workspace-relative path to the saved log (from DOWNLOAD)
	RootCause   string   // the agent's Markdown analysis
	ToolsCalled []string // tools the agent invoked (for the report footer)
	Duration    time.Duration
}

// Diagnoser runs the DEEPINSPECT stage against a fixed workspace using the LLM
// assigned to that stage.
type Diagnoser struct {
	cfg        *config.Config
	ws         *workspace.Workspace
	llm        config.LLMSpec
	background string // contents of TEST_AGENT.md
}

// New creates a Diagnoser. llm is the LLM assigned to the DEEPINSPECT stage;
// background is the TEST_AGENT.md content (may be "").
func New(cfg *config.Config, ws *workspace.Workspace, llm config.LLMSpec, background string) *Diagnoser {
	return &Diagnoser{cfg: cfg, ws: ws, llm: llm, background: background}
}

// Diagnose runs the DEEPINSPECT agent for one test. brief is the LOGPARSE
// handoff (Markdown); logRel is the workspace-relative path of the already-saved
// raw log, carried only into the report metadata — it is NOT given to the agent,
// which is hard-blocked from the raw log. A fresh agent is built per test
// (per-test independence), then the native tool-calling loop runs to completion.
func (d *Diagnoser) Diagnose(ctx context.Context, test jenkins.FailedTest, logRel, brief string) (Result, error) {
	start := time.Now()

	m, err := mapping.MapTestToSource(d.ws.Root(), test)
	if err != nil {
		return Result{}, fmt.Errorf("mapping %s: %w", test.FullName(), err)
	}

	// Give the agent a fresh per-test notebook to record what it's looking for
	// and why, and point the notebook tool at it for the duration of this test.
	if _, err := d.prepareNotebook(test); err != nil {
		return Result{}, fmt.Errorf("preparing notebook for %s: %w", test.FullName(), err)
	}

	// Hard-block the raw failure log: DEEPINSPECT works only from the brief. The
	// log tools are also withheld from the advertised tool set (see main), so this
	// only fires if a model emits a log call unprompted. Re-enable on the way out
	// so other stages/tests are unaffected.
	tools.SetLogToolsEnabled(false)
	defer tools.SetLogToolsEnabled(true)

	agent, err := d.buildAgent(test)
	if err != nil {
		return Result{}, fmt.Errorf("building agent for %s: %w", test.FullName(), err)
	}

	basePrompt := buildUserPrompt(test, m, brief, d.background)

	// Critique/revise loop: run the agent, and if the draft looks shallow (didn't
	// open source, or never names a flakiness mechanism) re-run it with the gaps
	// fed back, up to MaxAttempts. Each run is independent (memory disabled), so
	// the retry prompt carries the prior draft and the full task forward.
	attempts := d.cfg.Diagnosis.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}
	prompt := basePrompt
	var (
		res         *vnext.Result
		toolsCalled []string
	)
	for attempt := 1; attempt <= attempts; attempt++ {
		// Scope loop detection to this attempt: each Run is an independent
		// tool-calling loop, so a repeated call only signals a stuck loop within
		// the same run, not across attempts (which use different prompts).
		tools.ResetLoopGuard()
		r, err := agent.Run(ctx, prompt)
		if err != nil {
			return Result{}, fmt.Errorf("agent run for %s: %w", test.FullName(), err)
		}
		res = r
		toolsCalled = append(toolsCalled, r.ToolsCalled...)

		issues := critique(r)
		if len(issues) == 0 || attempt == attempts {
			break
		}
		fmt.Fprintf(os.Stderr, "  ↻ %s: attempt %d was shallow, re-diagnosing (%s)\n",
			test.FullName(), attempt, strings.Join(issues, "; "))
		prompt = buildRetryPrompt(basePrompt, r.Content, issues)
	}

	return Result{
		Test:        test,
		Mapping:     m,
		LogPath:     logRel,
		RootCause:   res.Content,
		ToolsCalled: toolsCalled,
		Duration:    time.Since(start),
	}, nil
}

// minToolCalls is the fewest tool calls below which we assume the agent barely
// looked at the system before concluding.
const minToolCalls = 3

// critique returns the reasons a diagnosis looks shallow, or nil if it passes.
// It is a cheap, conservative gate for the revise loop: it only flags answers
// that clearly didn't do the work, so a genuinely thorough first attempt is
// accepted without a second (costly) run.
func critique(res *vnext.Result) []string {
	var issues []string

	// Did the agent actually open any source files? ToolCalls carries the
	// arguments; fall back to ToolsCalled (names only) if it's empty.
	total := len(res.ToolCalls)
	sourceReads := 0
	for _, c := range res.ToolCalls {
		if p := toolArgPath(c.Arguments); p != "" {
			sourceReads++
		}
	}
	if total == 0 {
		total = len(res.ToolsCalled)
	}
	if total < minToolCalls {
		issues = append(issues, fmt.Sprintf("only %d tool call(s) were made — the system was barely explored", total))
	}
	if len(res.ToolCalls) > 0 && sourceReads == 0 {
		issues = append(issues, "no source files were opened — read the actual client and server code named in the brief")
	}

	content := strings.ToLower(res.Content)
	if !mentionsMechanism(content) {
		issues = append(issues, "the report names no nondeterminism mechanism (race / timing / ordering / resource / environment) — flaky failures need one")
	}
	if !hasFileCitation(res.Content) {
		issues = append(issues, "the report cites no concrete source file as evidence")
	}
	return issues
}

// mechanismTerms are words that signal the report engaged with WHY a test is
// flaky rather than just what it does.
var mechanismTerms = []string{
	"race", "concurren", "thread", "lock", "mutex", "atomic", "deadlock",
	"timing", "timeout", "deadline", "sleep", "wait", "poll", "async",
	"order", "schedul", "nondetermin", "intermitt", "retry", "backoff",
	"resource", "port", "leak", "limit", "environment", "leftover", "seed",
	"replication", "quorum", "partition", "startup", "ready",
}

func mentionsMechanism(lowerContent string) bool {
	for _, t := range mechanismTerms {
		if strings.Contains(lowerContent, t) {
			return true
		}
	}
	return false
}

// sourceFileRe matches a workspace-relative source path with a recognizable
// extension (Python/C++ and common glue), used to confirm the report cites
// actual code rather than just prose.
var sourceFileRe = regexp.MustCompile(`[\w./-]+\.(py|pyx|cc|cpp|cxx|c|h|hh|hpp|hxx|proto|go|java|rs)\b`)

func hasFileCitation(content string) bool {
	return sourceFileRe.MatchString(content)
}

// toolArgPath extracts a single file path from a tool call's arguments, handling
// both the "path" argument (read_lines/grep/read_file/list_directory) and the
// "paths" list (count_lines).
func toolArgPath(args map[string]interface{}) string {
	if args == nil {
		return ""
	}
	if p, ok := args["path"].(string); ok {
		return p
	}
	switch ps := args["paths"].(type) {
	case string:
		return ps
	case []interface{}:
		if len(ps) > 0 {
			if s, ok := ps[0].(string); ok {
				return s
			}
		}
	}
	return ""
}

// buildAgent constructs a fresh agent for one test. Memory is disabled so each
// diagnosis is fully independent; reasoning is enabled so the agent loops:
// call LLM -> execute tools -> feed results back -> repeat.
//
// We deliberately do NOT apply a builder preset: the registered internal tools
// attach via Tools.Enabled alone (createTools -> DiscoverInternalTools), and the
// ChatAgent preset would clobber our SystemPrompt/Temperature/MaxTokens and
// re-enable memory after WithConfig.
func (d *Diagnoser) buildAgent(test jenkins.FailedTest) (vnext.Agent, error) {
	name := "diagnose-" + sanitize(test.FullName())
	return vnext.NewBuilder(name).
		WithConfig(&vnext.Config{
			Name:         name,
			SystemPrompt: systemPrompt,
			LLM: vnext.LLMConfig{
				Provider:    d.llm.Provider,
				Model:       d.llm.Model,
				BaseURL:     d.llm.BaseURL,
				APIKey:      d.llm.APIKey,
				Temperature: d.llm.Temperature,
				MaxTokens:   d.llm.MaxTokens,
			},
			Tools: &vnext.ToolsConfig{
				Enabled: true,
				Reasoning: &vnext.ReasoningConfig{
					Enabled:           true,
					MaxIterations:     d.maxToolIterations(),
					ContinueOnToolUse: true,
				},
			},
			Memory:  &vnext.MemoryConfig{Enabled: false},
			Timeout: 10 * time.Minute,
		}).
		Build()
}

// prepareNotebook starts a fresh investigation notebook for the test (replacing
// any stale one from a previous run) and points the notebook tool at it. The
// agent appends to and re-reads this file through the tool to keep its bearings.
func (d *Diagnoser) prepareNotebook(test jenkins.FailedTest) (string, error) {
	dir := filepath.Join(d.ws.Root(), notebookDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	rel := filepath.Join(notebookDir, sanitize(test.FullName())+".md")
	abs := filepath.Join(d.ws.Root(), rel)
	header := fmt.Sprintf("# Investigation notebook: %s\n\n", test.FullName())
	if err := os.WriteFile(abs, []byte(header), 0o644); err != nil {
		return "", err
	}
	relSlash := filepath.ToSlash(rel)
	tools.SetNotebookPath(relSlash)
	return relSlash, nil
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
