package pipeline

import (
	"context"
	"fmt"
	"os"

	"github.com/gilbertr/testdiag/internal/diagnose"
)

// deepInspectAllStage runs one DEEPINSPECT+FEEDBACK pass per hypothesis from
// the HYPOTHESIZE stage. A hypothesis that fails (agent error or feedback
// exhausted) is recorded as a failed outcome but does NOT stop the pipeline —
// the COMBINE stage will work with whatever results are available.
type deepInspectAllStage struct {
	d            *diagnose.Diagnoser
	feedback     *feedbackChecker // nil when DEEPINSPECT feedback is disabled
	maxFeedbacks int
	verbose      bool
}

func newDeepInspectAllStage(d *diagnose.Diagnoser, fb *feedbackChecker, maxFeedbacks int, verbose bool) *deepInspectAllStage {
	return &deepInspectAllStage{d: d, feedback: fb, maxFeedbacks: maxFeedbacks, verbose: verbose}
}

func (s *deepInspectAllStage) Name() State { return StateDeepInspect }

func (s *deepInspectAllStage) Run(ctx context.Context, sc *Context) error {
	sc.DeepInspects = make([]DeepInspectOutcome, 0, len(sc.Hypotheses))
	for i, h := range sc.Hypotheses {
		if ctx.Err() != nil {
			sc.DeepInspects = append(sc.DeepInspects, DeepInspectOutcome{
				Hypothesis: h, Failed: true, FailReason: "context cancelled",
			})
			continue
		}
		// Pass the PLANINSPECTION output for this hypothesis, if available and successful.
		var planContent string
		if i < len(sc.Plans) && !sc.Plans[i].Failed {
			planContent = sc.Plans[i].Content
		}
		sc.DeepInspects = append(sc.DeepInspects, s.runOne(ctx, sc, h, planContent))
	}
	return nil
}

// runOne runs the DEEPINSPECT+FEEDBACK loop for one hypothesis. It never
// returns an error; failures are captured in the returned outcome.
func (s *deepInspectAllStage) runOne(ctx context.Context, sc *Context, h Hypothesis, planContent string) DeepInspectOutcome {
	out := DeepInspectOutcome{Hypothesis: h}

	if s.verbose {
		fmt.Fprintf(os.Stdout, "--- handoff to DEEPINSPECT h%d/%d for %s ---\n%s\n--- end ---\n\n",
			h.Index, len(sc.Hypotheses), sc.Test.FullName(), h.Text())
	}

	var (
		prevResult string
		critique   string
	)
	for feedbacks := 0; ; {
		res, err := s.d.Diagnose(ctx, diagnose.DiagnoseInput{
			Test:            sc.Test,
			Brief:           sc.Brief,
			Hypothesis:      h.Text(),
			HypothesisIndex: h.Index,
			Plan:            planContent,
			PrevResult:      prevResult,
			Critique:        critique,
		})
		if err != nil {
			out.Failed = true
			out.FailReason = err.Error()
			if s.verbose {
				fmt.Fprintf(os.Stdout, "  DEEPINSPECT h%d error: %v\n", h.Index, err)
			}
			return out
		}
		out.Content = res.Content
		out.ToolsCalled = res.ToolsCalled

		if s.feedback == nil {
			out.FeedbackApproved = true
			return out
		}

		ok, newCritique, err := s.feedback.Check(ctx, sc.Test, res.Content)
		if err != nil {
			// A feedback error on a hypothesis is non-fatal: mark as failed.
			out.Failed = true
			out.FailReason = fmt.Sprintf("feedback error: %v", err)
			if s.verbose {
				fmt.Fprintf(os.Stdout, "  DEEPINSPECT h%d FEEDBACK error: %v\n", h.Index, err)
			}
			return out
		}
		if s.verbose {
			if ok {
				fmt.Fprintf(os.Stdout, "  DEEPINSPECT h%d FEEDBACK: APPROVED\n", h.Index)
			} else {
				fmt.Fprintf(os.Stdout, "  DEEPINSPECT h%d FEEDBACK: NEEDS REVISION: %s\n", h.Index, newCritique)
			}
		}
		if ok {
			out.FeedbackApproved = true
			return out
		}
		feedbacks++
		if feedbacks >= s.maxFeedbacks {
			out.Failed = true
			out.FailReason = fmt.Sprintf("did not meet goals after %d feedback(s): %s", feedbacks, newCritique)
			return out
		}
		prevResult = res.Content
		critique = newCritique
	}
}

