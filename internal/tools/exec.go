package tools

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	vnext "github.com/agenticgokit/agenticgokit/v1beta"

	"github.com/gilbertr/testdiag/internal/workspace"
)

// Caps for the script-running tool. Scripts are arbitrary code, so output is
// hard-capped like every other tool, and the run is killed if it overruns.
const (
	maxScriptBytes = 64 << 10        // largest script body we'll accept
	maxOutputBytes = 64 << 10        // cap on each of stdout/stderr fed back
	scriptTimeout  = 2 * time.Minute // wall-clock limit on one script
)

// interpreters maps a caller-supplied language to the argv that runs a script
// fed on stdin. bash uses `-s` and python3 `-` to read the program from stdin,
// so nothing is ever written to disk.
var interpreters = map[string][]string{
	"shell":   {"bash", "-s"},
	"bash":    {"bash", "-s"},
	"sh":      {"sh", "-s"},
	"python":  {"python3", "-"},
	"python3": {"python3", "-"},
}

// Confirmer asks the operator to approve running a script, returning true to
// proceed. The script body and language are passed so the UI can show exactly
// what will run. It is called serialized (see confirmMu) so concurrent workers'
// prompts never interleave on the shared terminal.
type Confirmer func(language, script string) bool

var (
	confirmMu sync.Mutex
	confirmFn Confirmer = interactiveConfirm
)

// SetConfirmer replaces the approval policy used by run_script. main wires this
// to its interactive prompt; tests use it to auto-approve or auto-deny. Passing
// nil restores the default interactive prompt.
func SetConfirmer(c Confirmer) {
	confirmMu.Lock()
	defer confirmMu.Unlock()
	if c == nil {
		c = interactiveConfirm
	}
	confirmFn = c
}

// stdinReader is a single shared buffered reader over stdin. A fresh
// bufio.Reader per prompt could swallow bytes already buffered from the
// terminal, so we keep one for the life of the process.
var stdinReader = bufio.NewReader(os.Stdin)

// interactiveConfirm shows the script on stderr and reads a 1 (yes) / 2 (no)
// answer from stdin. Anything other than "1" is treated as a decline, so an
// EOF or a stray keystroke fails safe.
func interactiveConfirm(language, script string) bool {
	fmt.Fprintf(os.Stderr, "\n┌─ The agent wants to run a %s script ─────────────────────────\n", language)
	for _, line := range strings.Split(strings.TrimRight(script, "\n"), "\n") {
		fmt.Fprintf(os.Stderr, "│ %s\n", line)
	}
	fmt.Fprintf(os.Stderr, "└──────────────────────────────────────────────────────────────\n")
	fmt.Fprintf(os.Stderr, "Run it? [1 = Yes, 2 = No]: ")

	line, err := stdinReader.ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(os.Stderr, "(no input — declined)")
		return false
	}
	return strings.TrimSpace(line) == "1"
}

// ---------------------------------------------------------------------------
// run_script
// ---------------------------------------------------------------------------

type runScriptTool struct{ ws *workspace.Workspace }

func (t *runScriptTool) Name() string { return "run_script" }
func (t *runScriptTool) Description() string {
	return "Write and execute a shell or Python script in the workspace root and return its exit code, stdout and stderr. " +
		"DANGEROUS: the script runs real commands on the operator's machine, so the operator is shown the exact script and " +
		"must approve it before it runs — a script may be declined, in which case nothing runs. Use it only when reading " +
		"files is not enough (e.g. to reproduce a failure, inspect the environment, or run a quick experiment); keep scripts " +
		"short, read-only where possible, and self-contained. The working directory is the project checkout."
}
func (t *runScriptTool) JSONSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"language": map[string]interface{}{
				"type":        "string",
				"enum":        []string{"shell", "bash", "sh", "python", "python3"},
				"description": "Interpreter to run the script with: a shell (bash/sh) or Python 3.",
			},
			"script": map[string]interface{}{
				"type":        "string",
				"description": "The full script source to execute. Runs with the workspace root as the working directory.",
			},
		},
		"required": []string{"language", "script"},
	}
}

func (t *runScriptTool) Execute(ctx context.Context, args map[string]interface{}) (*vnext.ToolResult, error) {
	language, hasLang := strArg(args, "language")
	if !hasLang {
		return fail("run_script: 'language' is required")
	}
	argv, known := interpreters[strings.ToLower(language)]
	if !known {
		return fail("run_script: unsupported language %q (use one of: shell, bash, sh, python, python3)", language)
	}
	script, hasScript := strArg(args, "script")
	if !hasScript {
		return fail("run_script: 'script' is required")
	}
	if len(script) > maxScriptBytes {
		return fail("run_script: script is %d bytes, exceeding the %d-byte limit; make it smaller", len(script), maxScriptBytes)
	}

	// Ask the operator. Serialize so concurrent workers prompt one at a time and
	// their output never interleaves on the shared terminal.
	confirmMu.Lock()
	approved := confirmFn(strings.ToLower(language), script)
	confirmMu.Unlock()
	if !approved {
		return ok(map[string]interface{}{
			"approved": false,
			"message":  "The operator declined to run this script. Do not retry the same script; either reason from the files you can read or propose a different, safer script.",
		}), nil
	}

	runCtx, cancel := context.WithTimeout(ctx, scriptTimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = t.ws.Root()
	cmd.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return ok(map[string]interface{}{
			"approved":  true,
			"timed_out": true,
			"timeout_s": int(scriptTimeout / time.Second),
			"stdout":    capString(stdout.String()),
			"stderr":    capString(stderr.String()),
			"message":   fmt.Sprintf("Script killed after exceeding the %s time limit.", scriptTimeout),
		}), nil
	}

	exitCode, err := exitCodeOf(runErr)
	if err != nil {
		// Couldn't even start the interpreter (e.g. python3 not installed).
		return fail("run_script: could not run %s: %v", argv[0], err)
	}

	outStr, outTrunc := capStringTrunc(stdout.String())
	errStr, errTrunc := capStringTrunc(stderr.String())
	return ok(map[string]interface{}{
		"approved":         true,
		"exit_code":        exitCode,
		"stdout":           outStr,
		"stdout_truncated": outTrunc,
		"stderr":           errStr,
		"stderr_truncated": errTrunc,
	}), nil
}

// exitCodeOf extracts the process exit code from a *exec.Cmd Run error. A nil
// error means exit 0; a non-zero exit is an *exec.ExitError carrying the code;
// anything else (interpreter not found, etc.) is a real start error.
func exitCodeOf(runErr error) (int, error) {
	if runErr == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		return ee.ExitCode(), nil
	}
	return 0, runErr
}

// capString truncates s to the output cap (no truncation flag).
func capString(s string) string {
	out, _ := capStringTrunc(s)
	return out
}

// capStringTrunc truncates s to maxOutputBytes, reporting whether it was cut.
func capStringTrunc(s string) (string, bool) {
	if len(s) <= maxOutputBytes {
		return s, false
	}
	return s[:maxOutputBytes], true
}
