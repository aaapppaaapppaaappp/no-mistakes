package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

func TestReviewFixerGlobalConfigRoutesRealProcessesAndKeepsUnslothProfilePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-level launcher fixture uses POSIX shell; argv construction is covered portably in internal/agent")
	}

	fixtureDir := t.TempDir()
	codexBin := filepath.Join(fixtureDir, "codex-fixture")
	unslothBin := filepath.Join(fixtureDir, "unsloth-fixture")
	writeExecutableFixture(t, codexBin, `#!/bin/sh
: > "$CAPTURE_FILE"
for arg do
  printf '%s\0' "$arg" >> "$CAPTURE_FILE"
done
printf '%s' "$CODEX_HOME" > "$CAPTURE_FILE.codex-home"
printf '%s\n' '{"type":"thread.started","thread_id":"local-fixer-thread"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`)
	writeExecutableFixture(t, unslothBin, `#!/bin/sh
: > "$CAPTURE_LAUNCHER_FILE"
for arg do
  printf '%s\0' "$arg" >> "$CAPTURE_LAUNCHER_FILE"
done
if [ "$1" != "start" ] || [ "$2" != "codex" ] || [ "$3" != "--persist" ]; then
  echo "unexpected unsloth launcher arguments" >&2
  exit 2
fi
shift 3
export CODEX_HOME="$UNSLOTH_PERSIST_HOME"
exec "$FAKE_CODEX_CHILD" "$@"
`)

	global, err := config.LoadGlobalFromBytes([]byte(fmt.Sprintf(`
agent: codex
agent_path_override:
  codex: %q
agent_args_override:
  codex: [-m, reviewer-model, -c, 'model_provider="reviewer-provider"']
review_fixer_agent: codex
review_fixer_command: [%q, start, codex, --persist]
`, codexBin, unslothBin)))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Merge(global, &config.RepoConfig{})
	t.Setenv("CODEX_HOME", t.TempDir())
	pipelineAgent, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), exec.LookPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pipelineAgent.Close() })

	workDir := t.TempDir()
	persistentHome := filepath.Join(fixtureDir, "unsloth-persistent-codex-home")
	run := func(tag, purpose string, session *agent.SessionRef) (*agent.Result, []string, []string, string) {
		t.Helper()
		capture := filepath.Join(fixtureDir, tag+".codex-args")
		launcherCapture := filepath.Join(fixtureDir, tag+".launcher-args")
		result, runErr := pipelineAgent.Run(context.Background(), agent.RunOpts{
			Prompt:  "USER_PROMPT_" + tag,
			Purpose: purpose,
			Session: session,
			CWD:     workDir,
			Env: []string{
				"CAPTURE_FILE=" + capture,
				"CAPTURE_LAUNCHER_FILE=" + launcherCapture,
				"CODEX_HOME=" + filepath.Join(fixtureDir, "default-reviewer-codex-home"),
				"UNSLOTH_PERSIST_HOME=" + persistentHome,
				"FAKE_CODEX_CHILD=" + codexBin,
			},
		})
		if runErr != nil {
			t.Fatalf("run %s: %v", tag, runErr)
		}
		codexArgs := readNULArgs(t, capture)
		launcherArgs := readOptionalNULArgs(t, launcherCapture)
		codexHomeBytes, readErr := os.ReadFile(capture + ".codex-home")
		if readErr != nil {
			t.Fatalf("read %s CODEX_HOME: %v", tag, readErr)
		}
		return result, codexArgs, launcherArgs, string(codexHomeBytes)
	}

	_, reviewArgs, reviewLauncher, reviewHome := run("review", "review", nil)
	assertArgsContainInOrder(t, reviewArgs, "exec", "-m", "reviewer-model", "-c", `model_provider="reviewer-provider"`)
	if len(reviewLauncher) != 0 {
		t.Fatalf("independent review unexpectedly used review fixer launcher: %v", reviewLauncher)
	}
	if want := filepath.Join(fixtureDir, "default-reviewer-codex-home"); reviewHome != want {
		t.Fatalf("review CODEX_HOME = %q, want %q", reviewHome, want)
	}

	cold, coldFixArgs, coldLauncher, coldHome := run("review-fix-cold", "review-fix", nil)
	assertArgsEqualPrefix(t, coldLauncher, "start", "codex", "--persist", "exec")
	assertArgsEqualPrefix(t, coldFixArgs, "exec")
	assertArgsOmit(t, coldFixArgs, "reviewer-model", `model_provider="reviewer-provider"`)
	if cold.SessionID != "local-fixer-thread" {
		t.Fatalf("cold fixer session id = %q, want local-fixer-thread", cold.SessionID)
	}
	if coldHome != persistentHome {
		t.Fatalf("cold fixer CODEX_HOME = %q, want persistent Unsloth home %q", coldHome, persistentHome)
	}

	resumeSession := &agent.SessionRef{ID: cold.SessionID, Agent: cold.Provider}
	resumed, resumeFixArgs, resumeLauncher, resumeHome := run("review-fix-resume", "review-fix", resumeSession)
	assertArgsEqualPrefix(t, resumeLauncher, "start", "codex", "--persist", "exec", "resume", "local-fixer-thread")
	assertArgsEqualPrefix(t, resumeFixArgs, "exec", "resume", "local-fixer-thread")
	assertArgsOmit(t, resumeFixArgs, "reviewer-model", `model_provider="reviewer-provider"`)
	if !resumed.Resumed {
		t.Fatal("second review-fix invocation did not resume the fixer session")
	}
	if resumeHome != persistentHome {
		t.Fatalf("resumed fixer CODEX_HOME = %q, want persistent Unsloth home %q", resumeHome, persistentHome)
	}

	_, evidenceArgs, evidenceLauncher, evidenceHome := run("test-evidence", "test-evidence", nil)
	assertArgsContainInOrder(t, evidenceArgs, "exec", "-m", "reviewer-model", "-c", `model_provider="reviewer-provider"`)
	if len(evidenceLauncher) != 0 {
		t.Fatalf("non-review purpose unexpectedly used review fixer launcher: %v", evidenceLauncher)
	}
	if evidenceHome != reviewHome {
		t.Fatalf("non-review purpose CODEX_HOME = %q, want default reviewer home %q", evidenceHome, reviewHome)
	}

	t.Logf("review -> codex %s; CODEX_HOME=%s", summarizeCapturedArgs(reviewArgs), reviewHome)
	t.Logf("review-fix (cold) -> unsloth %s -> codex %s; CODEX_HOME=%s; session=%s", summarizeCapturedArgs(coldLauncher), summarizeCapturedArgs(coldFixArgs), coldHome, cold.SessionID)
	t.Logf("review-fix (resume) -> unsloth %s -> codex %s; CODEX_HOME=%s; resumed=%t", summarizeCapturedArgs(resumeLauncher), summarizeCapturedArgs(resumeFixArgs), resumeHome, resumed.Resumed)
	t.Logf("test-evidence -> codex %s; CODEX_HOME=%s", summarizeCapturedArgs(evidenceArgs), evidenceHome)
}

func writeExecutableFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write executable fixture: %v", err)
	}
}

func readNULArgs(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read captured args %s: %v", path, err)
	}
	return splitNULArgs(data)
}

func readOptionalNULArgs(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read optional captured args %s: %v", path, err)
	}
	return splitNULArgs(data)
}

func splitNULArgs(data []byte) []string {
	trimmed := strings.TrimSuffix(string(data), "\x00")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\x00")
}

func assertArgsEqualPrefix(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) < len(want) {
		t.Fatalf("args %v shorter than wanted prefix %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg[%d] = %q, want %q in %v", i, got[i], want[i], got)
		}
	}
}

func assertArgsContainInOrder(t *testing.T, got []string, want ...string) {
	t.Helper()
	next := 0
	for _, arg := range got {
		if next < len(want) && arg == want[next] {
			next++
		}
	}
	if next != len(want) {
		t.Fatalf("args %v do not contain %v in order", got, want)
	}
}

func assertArgsOmit(t *testing.T, got []string, forbidden ...string) {
	t.Helper()
	for _, arg := range got {
		for _, value := range forbidden {
			if arg == value {
				t.Fatalf("args unexpectedly contain %q: %v", value, got)
			}
		}
	}
}

func summarizeCapturedArgs(args []string) string {
	compact := make([]string, len(args))
	for i, arg := range args {
		if strings.Contains(arg, "USER_PROMPT_") {
			compact[i] = "<steered-prompt>"
		} else {
			compact[i] = arg
		}
	}
	return strings.Join(compact, " ")
}
