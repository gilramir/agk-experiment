# testdiag

A CLI that diagnoses automated-test failures from a Jenkins build, using an LLM
(via [AgenticGoKit](https://github.com/agenticgokit/agenticgokit)) that can read
the project's source with file-inspection tools to find the **root cause** of
each failure.

> Status: reference implementation. It is wired end-to-end but has one
> deliberate placeholder you must fill in (see [Placeholders](#placeholders)).

## What it does

Given a Jenkins build URL:

1. Appends `/api/json` and fetches the build's test report (HTTP Basic auth with
   your Jenkins user + API token).
2. Parses the JSON to find every **failed** test case (with its stack trace and
   captured stdout/stderr).
3. For each failure, **one at a time, in order**, it runs a small state machine
   of stages, each handing off to the next through a Markdown file on disk:
   - **DOWNLOAD** — saves the test's full failure log under `.testdiag/logs/`.
   - **LOGPARSE** — one tool-less LLM pass over that log produces an
     **investigation brief** (`.testdiag/handoff/<test>.logparse.md`): the first
     real error, the source/logic to find, and the candidate flakiness
     conditions to check.
   - **DEEPINSPECT** — a fresh agent that gets **only the brief** (not the raw
     log) plus the workspace **source** tools, traces into the actual code, and
     produces the root-cause report. The raw-log tools are withheld from it.
4. Writes one Markdown root-cause report per test under `test-diagnosis/`.

You can assign a **different LLM to LOGPARSE and DEEPINSPECT** (see
[Setup](#setup)): a cheap model can read the noisy log and write the brief while
a stronger model does the deep source tracing. Splitting the work, and keeping
the raw log out of DEEPINSPECT, is what keeps each model focused.

Each test is diagnosed independently — its own agents, no shared memory. They are
run sequentially (rather than in a worker pool) so the output, and especially the
`run_script` approval prompts, stay coherent for the operator instead of
interleaving many tests at once.

## Tools given to the LLM

All are **jailed to the workspace root** — the model cannot read outside the
checkout (absolute paths are reinterpreted relative to the root, and symlinks are
resolved and re-checked so they can't escape). They are AgenticGoKit *internal
tools* exposing JSON Schemas, so the provider can call them natively. Every tool
has hard output caps (file size, line span, match/entry/file counts) to protect
the context window.

| Tool | Purpose |
|------|---------|
| `read_file` | Read an entire (small) file |
| `list_directory` | List a directory's entries |
| `count_lines` | `wc -l` for one or more files |
| `read_lines` | Read a single line or an inclusive range |
| `grep` | Find matching lines (with numbers) in a file |
| `search_repo` | Recursive grep across the tree |
| `find_files` | Locate files by glob / substring |
| `git_blame` | Blame a jailed path |
| `git_log` | History for a jailed path (pager off, byte-capped) |
| `read_log` | Read the saved failure log (with `tail`) — **withheld from DEEPINSPECT** |
| `grep_log` | Search the failure log (with context lines) — **withheld from DEEPINSPECT** |
| `run_script` | Write + run a shell/Python script — **only after operator approval** |
| `notebook` | Per-test Markdown scratchpad (`append` / `read`) the agent uses as working memory |

The two log tools are not advertised to DEEPINSPECT, and are hard-disabled while
it runs (`tools.SetLogToolsEnabled`), so it cannot re-read the raw log — it works
from the LOGPARSE brief. LOGPARSE itself uses no tools (the log is given inline).

The prompt steers the model to `count_lines`/`grep`/`read_lines` rather than
dumping whole files, so large logs and large sources stay within context.

`run_script` is the one tool that writes and executes. It runs nothing until the
operator approves the exact script at a `1 = Yes / 2 = No` prompt; a decline runs
nothing. The `notebook` path is fixed per test (`.testdiag/notes/<test>.md`) and
is **not** a model argument, so the agent can only write there. A loop guard
intercepts identical repeated tool calls and nudges the model to change approach.

## Setup

```sh
go mod tidy                      # download AgenticGoKit + deps
cp config.example.toml ~/.config/testdiag/config.toml
$EDITOR ~/.config/testdiag/config.toml
```

Configuration (file + `TESTDIAG_*` env overrides; env always wins, for CI
secrets) is documented in [`config.example.toml`](config.example.toml). At
minimum: define at least one LLM under `[llms.<name>]` (with `base_url` +
`model`), assign one to each stage under `[stages]`, and set your Jenkins
`user` + `api_token`.

```toml
[llms.fast]
base_url = "http://localhost:1234/v1"
model    = "your-fast-model"
[llms.deep]
base_url = "http://localhost:5678/v1"
model    = "your-strong-model"

[stages]
logparse    = "fast"   # reads the log, writes the brief
deepinspect = "deep"   # gets the brief + source tools, finds the root cause
```

(The two stages may point at the **same** LLM if you only have one.) Per-LLM
secrets can come from `TESTDIAG_LLM_<NAME>_API_KEY` etc.

Useful knobs:

- `[llms.<name>].context_window` — sizes how much of the log LOGPARSE inlines.
- `[proxy].normalize_tool_calls` / `[proxy].inject_tools` — front each endpoint
  with the in-process proxy that rewrites open-model tool-call syntaxes into the
  one form the agent parses, and advertises the workspace tools to the model. On
  by default; see below.
- `diagnosis.max_attempts` — DEEPINSPECT runs per test (>1 enables the
  critique/revise loop; 1 disables it).
- `diagnosis.max_tool_iterations` — tool calls allowed within one DEEPINSPECT
  attempt.

Put a `TEST_AGENT.md` at the root of the workspace you run against; its contents
are injected into every diagnosis as background context.

## Usage

```sh
# Run from inside the build's checkout (or set workspace.root in config):
testdiag https://jenkins.example.com/job/myapp/1234/

# Override the output directory:
testdiag --output ./reports https://jenkins.example.com/job/myapp/1234/testReport/

# Filter to a subset of failures: pass one or more substrings after the URL.
# Only failed tests whose name (class.method) contains any of them are
# diagnosed; with no substrings, every failed test is processed.
testdiag https://jenkins.example.com/job/myapp/1234/ 100 LoginTest

# -d/--debug logs the full LLM conversation; -v/--verbose logs tool progress.
testdiag -v https://jenkins.example.com/job/myapp/1234/
```

## Placeholders

These are intentionally left for you to complete:

- **Test → source-file mapping** — `internal/mapping/mapping.go`
  (`MapTestToSource`). This is project-specific (language, repo layout,
  package→path rules). It currently returns an empty path, which is safe: the
  agent will locate the file itself via `list_directory`/`grep`. Implement it to
  give the agent a precise starting point.

## How tool calls reach the model

AgenticGoKit v0.5.x's OpenAI adapter does **no** native tool calling: it never
sends a `tools` array and reads only `choices[].message.content`, leaving the
agent to parse tool calls out of text. testdiag bridges this with an in-process
reverse proxy (`internal/llmproxy`) that fronts your LLM endpoint: it injects the
workspace tools into each request and runs the response through `internal/toolproto`,
which normalizes the various native tool-call syntaxes open models emit
(GPT-OSS Harmony, Gemma ` ```tool_code `, Mistral `[TOOL_CALLS]`,
Nemotron `<TOOLCALL>`, Llama 3.x bare-JSON / `<|python_tag|>`, plus structured
`tool_calls`) into the one shape the agent recognizes. `main.go` starts the
proxies and repoints each stage's `base_url` at one when `[proxy].normalize_tool_calls`
(or `--debug` / `-v`) is set. It runs at most one proxy per distinct
(endpoint, advertised tool set): DEEPINSPECT's proxy advertises the **source**
tools, LOGPARSE's advertises **none**, so the log tools never reach DEEPINSPECT.

## Layout

```
main.go                     CLI + sequential orchestration + per-stage proxies
internal/config             named LLMs, stage assignments, env overrides
internal/jenkins            fetch /api/json, parse failed cases
internal/pipeline           the DOWNLOAD → LOGPARSE → DEEPINSPECT state machine
internal/mapping            test -> source file  (STUB)
internal/workspace          path jail for the file tools
internal/tools              the workspace tools (native-schema internal tools)
internal/diagnose           the DEEPINSPECT agent: build, prompt, tool loop
internal/report             Markdown report writer
internal/llmproxy           in-process proxy fronting an LLM endpoint
internal/toolproto          normalize open-model tool-call syntaxes
```

The `AgenticGoKit/` directory in this tree is a local clone for reference only;
it is git-ignored and not used by the build (the dependency is fetched normally
via `go.mod`).
</content>
</invoke>
