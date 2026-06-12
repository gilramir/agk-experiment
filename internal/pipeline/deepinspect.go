package pipeline

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/gilbertr/testdiag/internal/diagnose"
)

// deepInspectStage runs the deep source-tracing agent. It hands the LOGPARSE
// brief (sc.Brief) to the diagnose engine, which builds a fresh per-test agent
// with the workspace source tools — but the raw log is withheld (not inlined and
// hard-disabled via tools.SetLogToolsEnabled inside Diagnose). sc.LogPath is
// carried only into the report metadata, not given to the agent.
type deepInspectStage struct {
	d       *diagnose.Diagnoser
	verbose bool
}

func newDeepInspectStage(d *diagnose.Diagnoser, verbose bool) *deepInspectStage {
	return &deepInspectStage{d: d, verbose: verbose}
}

func (s *deepInspectStage) Name() State { return StateDeepInspect }

func (s *deepInspectStage) Run(ctx context.Context, sc *Context) error {
	if s.verbose {
		brief := strings.TrimSpace(sc.Brief)
		if brief == "" {
			brief = "(empty)"
		}
		fmt.Fprintf(os.Stdout, "--- LOGPARSE handoff for %s ---\n%s\n--- end of handoff ---\n\n",
			sc.Test.FullName(), brief)
	}
	res, err := s.d.Diagnose(ctx, sc.Test, sc.LogPath, sc.Brief)
	if err != nil {
		return err
	}
	sc.Result = res
	return nil
}
