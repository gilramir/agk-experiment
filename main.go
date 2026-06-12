// Command testdiag diagnoses automated-test failures from a Jenkins build.
//
// Usage:
//
//	testdiag [flags] <jenkins-build-url>
//
// It fetches the build's test report (appending /api/json), finds every failed
// test, and for each one asks an LLM — equipped with workspace file-reading
// tools — to determine the root cause, writing one Markdown report per failure.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/gilramir/argparse/v2"

	// Side-effect import: registers the OpenAI-compatible LLM provider so
	// config "provider = openai" with a custom base_url works for local servers.
	_ "github.com/agenticgokit/agenticgokit/plugins/llm/openai"

	"github.com/gilbertr/testdiag/internal/config"
	"github.com/gilbertr/testdiag/internal/diagnose"
	"github.com/gilbertr/testdiag/internal/jenkins"
	"github.com/gilbertr/testdiag/internal/llmproxy"
	"github.com/gilbertr/testdiag/internal/report"
	"github.com/gilbertr/testdiag/internal/tools"
	"github.com/gilbertr/testdiag/internal/workspace"
)

// backgroundFile is read from the workspace root and injected into every
// diagnosis as project context.
const backgroundFile = "TEST_AGENT.md"

// options holds the parsed command-line arguments. Field names map to the
// argparse switches/positional (e.g. the "url" positional -> URL via its Dest).
type options struct {
	Output  string
	Debug   bool
	Verbose bool
	URL     string
	Filters []string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "testdiag: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	opts := &options{}
	ap := argparse.New(&argparse.Command{
		Description: "Diagnose Jenkins test failures with an LLM.",
		Values:      opts,
		Epilog: "Configuration is read from ~/.config/testdiag/config.toml and may be " +
			"overridden with TESTDIAG_* environment variables. The URL may be a build " +
			"or test-report URL; /api/json is appended automatically.",
	})
	ap.Add(&argparse.Argument{
		Switches: []string{"-o", "--output"},
		MetaVar:  "DIR",
		Help:     "Output directory for reports (overrides config)",
	})
	ap.Add(&argparse.Argument{
		Switches: []string{"-d", "--debug"},
		Help:     "Log the full conversation with the LLM to stderr",
	})
	ap.Add(&argparse.Argument{
		Switches: []string{"-v", "--verbose"},
		Help:     "Log tool progress (e.g. when a whole-repo search_repo/find_files starts and finishes) to stderr",
	})
	ap.Add(&argparse.Argument{
		Name: "url",
		Dest: "URL",
		Help: "Jenkins build (or test-report) URL",
	})
	ap.Add(&argparse.Argument{
		Name:        "filter",
		Dest:        "Filters",
		NumArgsGlob: "*",
		MetaVar:     "SUBSTRING",
		Help: "Only diagnose tests whose name contains any of these substrings " +
			"(default: all failed tests)",
	})
	// Parse handles -h/--help and reports parse errors, exiting as appropriate.
	ap.Parse()
	buildURL := opts.URL

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if opts.Output != "" {
		cfg.Output.Dir = opts.Output
	}
	if opts.Debug {
		cfg.LLM.Debug = true
	}

	ws, err := workspace.New(cfg.Workspace.Root)
	if err != nil {
		return err
	}

	background := readBackground(ws.Root())

	// Register the workspace file tools once, before any agent is built. Exclude
	// the report output directory from tree searches so the agent never reads its
	// own generated reports back in (the output dir often lives in the checkout).
	tools.SetVerbose(opts.Verbose)
	tools.ExcludeDir(filepath.Base(cfg.Output.Dir))
	toolNames := tools.Register(ws)

	// Front the LLM endpoint with the in-process proxy so models with differing
	// native tool-call syntaxes (GPT-OSS, Gemma, Mistral, Nemotron) all work, and
	// so the full conversation can be logged when debugging. This rewrites
	// cfg.LLM.BaseURL to the local proxy. Debug needs the proxy too, so start it
	// whenever either is requested.
	if cfg.LLM.NormalizeToolCalls || cfg.LLM.Debug || opts.Verbose {
		var proxyTools []llmproxy.Tool
		if cfg.LLM.NormalizeToolCalls && cfg.LLM.InjectTools {
			for _, s := range tools.Schemas() {
				proxyTools = append(proxyTools, llmproxy.Tool{
					Name:        s.Name,
					Description: s.Description,
					Parameters:  s.Parameters,
				})
			}
		}
		px, err := llmproxy.Start(cfg.LLM.BaseURL, llmproxy.Options{
			Tools:     proxyTools,
			Normalize: cfg.LLM.NormalizeToolCalls,
			Debug:     cfg.LLM.Debug,
			Verbose:   opts.Verbose,
		})
		if err != nil {
			return fmt.Errorf("starting LLM proxy: %w", err)
		}
		defer px.Close()
		fmt.Printf("LLM proxy active: %s -> %s (normalize=%t, inject_tools=%t, debug=%t)\n",
			px.BaseURL(), cfg.LLM.BaseURL,
			cfg.LLM.NormalizeToolCalls, cfg.LLM.InjectTools, cfg.LLM.Debug)
		cfg.LLM.BaseURL = px.BaseURL()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	jc := jenkins.NewClient(cfg.Jenkins.User, cfg.Jenkins.APIToken)
	failures, err := jc.FetchFailedTests(ctx, buildURL)
	if err != nil {
		return err
	}
	if len(failures) == 0 {
		fmt.Println("No failed tests found in the build report. Nothing to diagnose.")
		return nil
	}

	if len(opts.Filters) > 0 {
		failures = filterTests(failures, opts.Filters)
		if len(failures) == 0 {
			fmt.Printf("No failed tests match the given filter(s): %v\n", opts.Filters)
			return nil
		}
	}

	fmt.Printf("Found %d failed test(s). Workspace: %s\n", len(failures), ws.Root())
	fmt.Printf("LLM: %s @ %s (model %s). Tools: %v\n",
		cfg.LLM.Provider, cfg.LLM.BaseURL, cfg.LLM.Model, toolNames)
	fmt.Printf("Diagnosing one at a time; reports -> %s\n\n", cfg.Output.Dir)

	d := diagnose.New(cfg, ws, background)
	return process(ctx, d, failures, cfg.Output)
}

// process diagnoses failures one at a time, in order. Each test is independent,
// but running them sequentially keeps the output (and the run_script approval
// prompts) coherent for the operator rather than interleaving many at once.
func process(ctx context.Context, d *diagnose.Diagnoser, failures []jenkins.FailedTest, out config.Output) error {
	var failed, analyzed int

	for _, test := range failures {
		if ctx.Err() != nil {
			break
		}
		res, err := d.Diagnose(ctx, test)
		if err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "  ✗ %s: %v\n", test.FullName(), err)
		} else if path, werr := report.Write(out.Dir, res); werr != nil {
			failed++
			fmt.Fprintf(os.Stderr, "  ✗ %s: writing report: %v\n", test.FullName(), werr)
		} else {
			analyzed++
			fmt.Printf("  ✓ %s -> %s (%s)\n", test.FullName(), path, res.Duration.Round(1e6))
		}
	}

	fmt.Printf("\nDone. %d analyzed, %d failed.\n", analyzed, failed)
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("interrupted: %w", err)
	}
	if failed > 0 {
		return fmt.Errorf("%d test(s) could not be diagnosed", failed)
	}
	return nil
}

// filterTests keeps only tests whose full name contains at least one of the
// given substrings (OR semantics). With no substrings it returns all tests.
func filterTests(failures []jenkins.FailedTest, substrings []string) []jenkins.FailedTest {
	var kept []jenkins.FailedTest
	for _, t := range failures {
		name := t.FullName()
		for _, s := range substrings {
			if strings.Contains(name, s) {
				kept = append(kept, t)
				break
			}
		}
	}
	return kept
}

// readBackground loads TEST_AGENT.md from the workspace root if present.
func readBackground(root string) string {
	path := filepath.Join(root, backgroundFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "warning: could not read %s: %v\n", path, err)
		}
		return ""
	}
	return string(data)
}
