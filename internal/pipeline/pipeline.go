// Package pipeline runs the per-test diagnosis as an explicit state machine of
// stages, each handing off to the next through a written Markdown file on disk:
//
//	DOWNLOAD    — materialize the failing test's log to .testdiag/logs/<test>.log
//	LOGPARSE    — one LLM pass over that log → an investigation brief
//	             (.testdiag/handoff/<test>.logparse.md): the first real error, the
//	             source/logic to find, and the flakiness conditions to check
//	DEEPINSPECT — a fresh LLM that gets ONLY the brief (not the raw log) plus the
//	             workspace source tools, and produces the root-cause report
//
// Different LLMs can be assigned to LOGPARSE and DEEPINSPECT (see config), so a
// cheap model can summarize the noisy log while a stronger one does the deep
// source tracing. The split, and withholding the raw log from DEEPINSPECT, is
// what keeps each model focused. New stages can be added to the slice in New.
package pipeline

import (
	"context"
	"fmt"

	"github.com/gilbertr/testdiag/internal/config"
	"github.com/gilbertr/testdiag/internal/diagnose"
	"github.com/gilbertr/testdiag/internal/jenkins"
	"github.com/gilbertr/testdiag/internal/workspace"
)

// State names the stages of the diagnosis state machine.
type State string

const (
	StateDownload    State = "DOWNLOAD"
	StateLogParse    State = "LOGPARSE"
	StateDeepInspect State = "DEEPINSPECT"
	StateDone        State = "DONE"
)

// Context is the per-test state threaded across stages. Each stage reads the
// fields earlier stages set and fills its own; the handoff to the next stage is
// always a file on disk (the *Path fields), with the contents also kept inline
// where the next stage consumes them directly.
type Context struct {
	Test         jenkins.FailedTest
	LogPath      string          // workspace-relative raw log (DOWNLOAD output)
	LogParsePath string          // workspace-relative investigation brief (LOGPARSE output)
	Brief        string          // brief contents, handed to DEEPINSPECT
	Result       diagnose.Result // root-cause result (DEEPINSPECT output)
}

// Stage is one step of the state machine. Stages mutate the shared Context.
type Stage interface {
	Name() State
	Run(ctx context.Context, sc *Context) error
}

// Pipeline runs the ordered stages for each test against a fixed workspace.
type Pipeline struct {
	stages []Stage
}

// New builds the DOWNLOAD → LOGPARSE → DEEPINSPECT pipeline. logparseLLM and
// deepinspectLLM are the LLMs assigned to those stages; background is the
// TEST_AGENT.md content (may be ""). verbose enables per-stage progress output.
func New(cfg *config.Config, ws *workspace.Workspace, logparseLLM, deepinspectLLM config.LLMSpec, background string, verbose bool) *Pipeline {
	return &Pipeline{
		stages: []Stage{
			&downloadStage{ws: ws},
			newLogParseStage(ws, logparseLLM),
			newDeepInspectStage(diagnose.New(cfg, ws, deepinspectLLM, background), verbose),
		},
	}
}

// States returns the ordered stage names, for logging the configured pipeline.
func (p *Pipeline) States() []State {
	out := make([]State, 0, len(p.stages))
	for _, s := range p.stages {
		out = append(out, s.Name())
	}
	return out
}

// Run drives one test through every stage in order, stopping at the first error
// (or a cancelled context), and returns the DEEPINSPECT result for reporting.
func (p *Pipeline) Run(ctx context.Context, test jenkins.FailedTest) (diagnose.Result, error) {
	sc := &Context{Test: test}
	for _, st := range p.stages {
		if err := ctx.Err(); err != nil {
			return diagnose.Result{}, err
		}
		if err := st.Run(ctx, sc); err != nil {
			return diagnose.Result{}, fmt.Errorf("%s stage: %w", st.Name(), err)
		}
	}
	return sc.Result, nil
}
