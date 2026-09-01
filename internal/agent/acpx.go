package agent

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	acpxScannerMaxTokenSize    = 256 * 1024 * 1024
	acpxSchemaMaxBytes         = 1024 * 1024
	acpxSchemaEnvVar           = "NO_MISTAKES_JSON_SCHEMA_FILE"
	acpxSchemaDigestEnvVar     = "NO_MISTAKES_JSON_SCHEMA_SHA256"
	acpxStructuredOutputEnvVar = "NO_MISTAKES_PI_STRUCTURED_OUTPUT"
	acpxSingleAttemptEnvVar    = "NO_MISTAKES_ACPX_ATTEMPTS"
	acpxSingleAttemptEnvValue  = "1"
)

type acpxAgent struct {
	bin        string
	target     string
	rawCommand string
	// model is the harness-neutral model pin resolved by internal/agentcfg.
	// no-mistakes never speaks ACP itself, so acpx's own --model is the only
	// mechanism that reaches the target agent; empty leaves the target on its
	// configured default, exactly as before the common layer existed.
	model string
	subprocessContext
}

func (a *acpxAgent) Name() string { return "acp:" + a.target }

func (a *acpxAgent) ReportsAgentAttempts() bool { return true }

func (a *acpxAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	maxRetries, err := acpxMaxRetries(a.rawCommand)
	if err != nil {
		return nil, err
	}
	return runWithRetry(ctx, a.Name(), opts, maxRetries, classifyTransient, nil, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *acpxAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	prompt := opts.Prompt
	if len(opts.JSONSchema) > 0 {
		prompt = buildACPStructuredPrompt(prompt, opts.JSONSchema)
	}
	schemaPath, cleanupSchema, err := createACPXSchemaTransport(opts.JSONSchema)
	if err != nil {
		return nil, fmt.Errorf("acpx schema transport: %w", err)
	}
	defer cleanupSchema()

	args := a.buildArgs(opts)
	cmd := exec.CommandContext(ctx, a.bin, args...)
	cmd.Dir = opts.CWD
	// Always override ambient transport values. Only this invocation's exact
	// schema may activate the opt-in Pi extension in the ACP child.
	schemaDigest := ""
	if len(opts.JSONSchema) > 0 {
		sum := sha256.Sum256(opts.JSONSchema)
		schemaDigest = hex.EncodeToString(sum[:])
	}
	structuredOutputEnabled := acpxStructuredOutputOptedIn(a.rawCommand)
	structuredOutputOptIn := ""
	if structuredOutputEnabled {
		structuredOutputOptIn = "1"
	}
	cmd.Env = append(a.gitSafeEnv(opts.CWD, opts.Env),
		acpxSchemaEnvVar+"="+schemaPath,
		acpxSchemaDigestEnvVar+"="+schemaDigest,
		acpxStructuredOutputEnvVar+"="+structuredOutputOptIn,
		acpxSingleAttemptEnvVar+"=",
	)
	shellenv.ConfigureShellCommand(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("acpx stdin pipe: %w", err)
	}
	started, err := startNativeAgentCommand(cmd, nativeAgentActivityObserver(opts, a.Name()))
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("acpx start: %w", err)
	}
	defer started.closePipes()
	pid := started.pid()
	emitAgentStarted(opts, a.Name(), pid)

	stdinErrCh := writeNativeAgentStdin(stdin, prompt)

	var stderrBuf []byte
	var stderrWG sync.WaitGroup
	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		stderrBuf, _ = io.ReadAll(started.stderr)
	}()

	var usage TokenUsage
	_, singleAttempt := acpxRawCommandEnvValue(a.rawCommand, acpxSingleAttemptEnvVar)
	var protocolEvidence acpxProtocolEvidence
	warnProse := func() {
		if !protocolEvidence.assistantProse {
			return
		}
		emitAgentWarning(opts, a.Name(), "warning: ACP exact-output turn emitted assistant prose; ignored in favor of the native structured_output arguments")
		protocolEvidence.assistantProse = false
	}
	text, stdoutErr, err := parseAcpxJSONEventsWithEvidence(
		ctx,
		started.stdout,
		opts.OnChunk,
		&usage,
		&protocolEvidence,
		len(opts.JSONSchema) > 0 && structuredOutputEnabled,
		singleAttempt,
	)
	if err != nil {
		err = started.waitAfterParseError(err)
		stderrWG.Wait()
		err = errors.Join(err, acpxStdinError(<-stdinErrCh))
		retErr := fmt.Errorf("acpx parse events: %w", err)
		warnProse()
		emitAgentExited(opts, a.Name(), pid, retErr)
		return nil, retErr
	}
	waitErr := started.wait()
	stderrWG.Wait()
	stdinErr := acpxStdinError(<-stdinErrCh)
	if waitErr != nil {
		retErr := fmt.Errorf("acpx exited: %w: %s", errors.Join(waitErr, stdinErr), acpxProcessErrorOutput(stderrBuf, stdoutErr))
		warnProse()
		emitAgentExited(opts, a.Name(), pid, retErr)
		return nil, retErr
	}
	if stdinErr != nil {
		if out := acpxProcessErrorOutput(stderrBuf, stdoutErr); out != "" {
			stdinErr = fmt.Errorf("%w: %s", stdinErr, out)
		}
		warnProse()
		emitAgentExited(opts, a.Name(), pid, stdinErr)
		return nil, stdinErr
	}
	warnProse()
	if usage.OutputTokens == 0 {
		usage.OutputTokens = estimateAcpxTokens(len(text))
	}
	res, err := finalizeTextResult(a.Name(), text, opts.JSONSchema, usage)
	emitAgentExited(opts, a.Name(), pid, err)
	return res, err
}

func acpxStructuredOutputOptedIn(rawCommand string) bool {
	assignments, err := acpxLeadingEnvAssignments(rawCommand)
	return err == nil && assignments[acpxStructuredOutputEnvVar] == "1"
}

func acpxMaxRetries(rawCommand string) (int, error) {
	assignments, err := acpxLeadingEnvAssignments(rawCommand)
	if err != nil {
		return 0, err
	}
	value, present := assignments[acpxSingleAttemptEnvVar]
	if !present {
		return claudeMaxRetries, nil
	}
	if !acpxStructuredOutputOptedIn(rawCommand) {
		return 0, fmt.Errorf("%s is valid only on a trusted ACP Pi structured-output target", acpxSingleAttemptEnvVar)
	}
	if value != acpxSingleAttemptEnvValue {
		return 0, fmt.Errorf("invalid %s=%q: the only supported value is %s", acpxSingleAttemptEnvVar, value, acpxSingleAttemptEnvValue)
	}
	return 0, nil
}

func acpxLeadingEnvAssignments(rawCommand string) (map[string]string, error) {
	tokens := strings.Fields(strings.TrimSpace(rawCommand))
	assignments := make(map[string]string)
	if len(tokens) == 0 {
		return assignments, nil
	}
	if tokens[0] != "env" {
		for _, token := range tokens {
			if name, found := acpxPotentialControlledEnvName(token); found {
				return nil, fmt.Errorf("%s must be an unquoted leading env assignment", name)
			}
		}
		return assignments, nil
	}
	for i := 1; i < len(tokens); i++ {
		name, value, ok := acpxAssignment(tokens[i])
		if controlled, found := acpxPotentialControlledEnvName(tokens[i]); found && (!ok || name != controlled || strings.ContainsAny(tokens[i], `'"\\`)) {
			return nil, fmt.Errorf("%s must be an unquoted leading env assignment", controlled)
		}
		if !ok {
			if controlled, found := acpxPotentialControlledEnvName(tokens[i]); found {
				return nil, fmt.Errorf("%s must be an unquoted leading env assignment", controlled)
			}
			for _, token := range tokens[i:] {
				if trailingName, found := acpxPotentialControlledEnvName(token); found {
					return nil, fmt.Errorf("%s must be a leading env assignment", trailingName)
				}
			}
			return assignments, nil
		}
		if acpxControlledEnvName(name) {
			if _, duplicate := assignments[name]; duplicate {
				return nil, fmt.Errorf("duplicate %s assignment", name)
			}
			if name == acpxStructuredOutputEnvVar && i != 1 {
				return nil, fmt.Errorf("%s must be the first env assignment", name)
			}
		}
		assignments[name] = value
	}
	return assignments, nil
}

func acpxAssignment(token string) (name, value string, ok bool) {
	if !isEnvAssignment(token) {
		return "", "", false
	}
	name, value, _ = strings.Cut(token, "=")
	return name, value, true
}

func acpxControlledEnvName(name string) bool {
	return name == acpxStructuredOutputEnvVar || name == acpxSingleAttemptEnvVar
}

func acpxPotentialControlledEnvName(token string) (string, bool) {
	// acpx eventually evaluates the trusted command as shell syntax. Refuse any
	// quoted or escaped spelling that strings.Fields cannot interpret exactly,
	// rather than letting the child activate a control the parent did not see.
	normalized := strings.NewReplacer("'", "", `"`, "", "\\", "").Replace(token)
	name, _, ok := strings.Cut(normalized, "=")
	return name, ok && acpxControlledEnvName(name)
}

// acpxRawCommandEnvValue recognizes only a leading env assignment. The raw
// command is trusted global configuration, but deliberately is not parsed as a
// shell language here: quoting or expansion cannot silently activate a gate.
func acpxRawCommandEnvValue(rawCommand, name string) (string, bool) {
	assignments, err := acpxLeadingEnvAssignments(rawCommand)
	if err != nil {
		return "", false
	}
	value, ok := assignments[name]
	return value, ok
}

func (a *acpxAgent) Close() error { return nil }

func createACPXSchemaTransport(schema json.RawMessage) (string, func(), error) {
	if len(schema) == 0 {
		return "", func() {}, nil
	}
	if len(schema) > acpxSchemaMaxBytes {
		return "", func() {}, fmt.Errorf("schema exceeds %d-byte limit", acpxSchemaMaxBytes)
	}

	var root map[string]any
	if err := json.Unmarshal(schema, &root); err != nil {
		return "", func() {}, fmt.Errorf("schema must be a JSON object: %w", err)
	}
	if schemaType, ok := root["type"].(string); !ok || schemaType != "object" {
		return "", func() {}, fmt.Errorf(`schema root type must be "object"`)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft7)
	const schemaURL = "urn:no-mistakes:acpx-schema"
	if err := compiler.AddResource(schemaURL, root); err != nil {
		return "", func() {}, fmt.Errorf("invalid JSON Schema: %w", err)
	}
	if _, err := compiler.Compile(schemaURL); err != nil {
		return "", func() {}, fmt.Errorf("invalid JSON Schema: %w", err)
	}

	tempDir, err := filepath.Abs(os.TempDir())
	if err != nil {
		return "", func() {}, fmt.Errorf("resolve temporary directory: %w", err)
	}
	if !filepath.IsAbs(tempDir) {
		return "", func() {}, fmt.Errorf("temporary directory is not absolute")
	}
	f, err := os.CreateTemp(tempDir, "no-mistakes-acpx-schema-*.json")
	if err != nil {
		return "", func() {}, err
	}
	path := f.Name()
	cleanup := func() { _ = os.Remove(path) }
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("set owner-only mode: %w", err)
	}
	written, err := f.Write(schema)
	if err != nil || written != len(schema) {
		_ = f.Close()
		cleanup()
		if err != nil {
			return "", func() {}, fmt.Errorf("write: %w", err)
		}
		return "", func() {}, io.ErrShortWrite
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("close: %w", err)
	}
	return path, cleanup, nil
}

func (a *acpxAgent) buildArgs(opts RunOpts) []string {
	args := make([]string, 0, 12)
	if a.rawCommand != "" {
		args = append(args, "--agent", a.rawCommand)
	}
	if opts.CWD != "" {
		args = append(args, "--cwd", opts.CWD)
	}
	args = append(args,
		"--format", "json",
		"--json-strict",
		"--approve-all",
		"--non-interactive-permissions", "deny",
		"--suppress-reads",
	)
	// --model must stay among acpx's own options, ahead of the bare target and
	// the exec subcommand, or acpx reads it as an argument to the target.
	if a.model != "" {
		args = append(args, "--model", a.model)
	}
	if _, present := acpxRawCommandEnvValue(a.rawCommand, acpxSingleAttemptEnvVar); present {
		// acpx currently defaults to zero prompt retries. Make that part of the
		// observable process contract instead of relying on a version default.
		args = append(args, "--prompt-retries", "0")
	}
	if a.rawCommand == "" {
		args = append(args, a.target)
	}
	args = append(args, "exec", "--file", "-")
	return args
}

func acpxStdinError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("acpx stdin: %w", err)
}

func acpxProcessErrorOutput(stderr []byte, stdoutErr string) string {
	parts := make([]string, 0, 2)
	if stderrText := strings.TrimSpace(string(stderr)); stderrText != "" {
		parts = append(parts, stderrText)
	}
	if stdoutErr != "" {
		parts = append(parts, stdoutErr)
	}
	return strings.Join(parts, "\n")
}

func buildACPStructuredPrompt(prompt string, schema json.RawMessage) string {
	return prompt + "\n\n## no-mistakes final output contract\n\n" +
		"When the task is complete, your final assistant message must be a single JSON object that matches this JSON Schema. " +
		"Return only the JSON object. Do not wrap it in Markdown fences. Do not include prose before or after the JSON.\n\n" +
		string(schema)
}

type acpxJSONMessage struct {
	Method string         `json:"method"`
	Error  *acpxJSONError `json:"error"`
	Result struct {
		Usage acpxUsageFields `json:"usage"`
	} `json:"result"`
	Params struct {
		Update acpxSessionUpdate `json:"update"`
	} `json:"params"`
}

type acpxJSONError struct {
	Message string `json:"message"`
}

type acpxSessionUpdate struct {
	SessionUpdate string          `json:"sessionUpdate"`
	ToolCallID    string          `json:"toolCallId"`
	Title         string          `json:"title"`
	Name          string          `json:"name"`
	Status        string          `json:"status"`
	Content       json.RawMessage `json:"content"`
	Text          string          `json:"text"`
	Used          int             `json:"used"`
	usedReported  bool
	acpxUsageFields
	Meta struct {
		Usage acpxUsageFields `json:"usage"`
	} `json:"_meta"`
}

type acpxUsageFields struct {
	InputTokens                   int `json:"input_tokens"`
	OutputTokens                  int `json:"output_tokens"`
	CacheReadInputTokens          int `json:"cache_read_input_tokens"`
	CacheReadTokens               int `json:"cache_read_tokens"`
	CacheCreationInputTokens      int `json:"cache_creation_input_tokens"`
	CacheWriteInputTokens         int `json:"cache_write_input_tokens"`
	CacheWriteTokens              int `json:"cache_write_tokens"`
	CachedInputTokens             int `json:"cached_input_tokens"`
	InputTokensCamel              int `json:"inputTokens"`
	OutputTokensCamel             int `json:"outputTokens"`
	CacheReadInputTokensCamel     int `json:"cacheReadInputTokens"`
	CacheCreationInputTokensCamel int `json:"cacheCreationInputTokens"`
	CachedInputTokensCamel        int `json:"cachedInputTokens"`
	CacheReadTokensCamel          int `json:"cacheReadTokens"`
	CachedReadTokensCamel         int `json:"cachedReadTokens"`
	CacheCreationTokensCamel      int `json:"cacheCreationTokens"`
	CacheWriteTokensCamel         int `json:"cacheWriteTokens"`
	CachedWriteTokensCamel        int `json:"cachedWriteTokens"`
	reported                      bool
	cacheCreationReported         bool
}

// ACPStructuredOutputProtocolError means an opted-in exact-output turn did
// not end in one exclusive, completed structured_output call. Callers may use
// errors.As to distinguish a provider/protocol violation from process failure.
type ACPStructuredOutputProtocolError struct {
	Reason string
}

func (e *ACPStructuredOutputProtocolError) Error() string {
	return "ACP structured-output protocol violation: " + e.Reason
}

func acpxProtocolError(reason string) error {
	return &ACPStructuredOutputProtocolError{Reason: reason}
}

type acpxProtocolEvidence struct {
	assistantProse bool
}

func parseAcpxJSONEvents(ctx context.Context, r io.Reader, onChunk func(string), usage *TokenUsage, acceptStructuredOutput ...bool) (string, string, error) {
	return parseAcpxJSONEventsWithEvidence(ctx, r, onChunk, usage, nil, acceptStructuredOutput...)
}

func parseAcpxJSONEventsWithEvidence(ctx context.Context, r io.Reader, onChunk func(string), usage *TokenUsage, evidence *acpxProtocolEvidence, acceptStructuredOutput ...bool) (string, string, error) {
	structuredEnabled := len(acceptStructuredOutput) > 0 && acceptStructuredOutput[0]
	strictProtocol := structuredEnabled
	if len(acceptStructuredOutput) > 1 {
		strictProtocol = acceptStructuredOutput[1]
	}
	if structuredEnabled && !strictProtocol {
		return parseAcpxLegacyStructuredJSONEvents(ctx, r, onChunk, usage)
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), acpxScannerMaxTokenSize)
	var output strings.Builder
	var stdoutErr string
	var structuredOutput string
	var structuredCallID string
	var structuredCalls int
	var protocolReason string
	var wrongToolSeen bool
	activeTools := make(map[string]string)

	failProtocol := func(reason string) {
		if structuredEnabled && protocolReason == "" {
			protocolReason = reason
		}
	}

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return "", stdoutErr, ctx.Err()
		default:
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var msg acpxJSONMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			if structuredEnabled {
				return "", stdoutErr, acpxProtocolError("malformed acpx JSON event")
			}
			continue
		}
		markAcpxUsagePresence(line, &msg)
		if msg.Error != nil && msg.Error.Message != "" {
			if stdoutErr == "" {
				stdoutErr = msg.Error.Message
			}
			failProtocol("ACP JSON-RPC error: " + msg.Error.Message)
		}
		*usage = acpxMaxUsage(*usage, acpxUsageFieldsToTokenUsage(msg.Result.Usage))
		if msg.Method != "session/update" {
			continue
		}

		update := msg.Params.Update
		switch update.SessionUpdate {
		case "usage_update":
			*usage = acpxMaxUsage(*usage, acpxUpdateUsage(update))
		case "agent_message_chunk":
			text := acpxUpdateText(update)
			if text == "" {
				continue
			}
			// pi-acp emits this fixed adapter banner as an ACP assistant chunk
			// before the model turn. It is not provider/model prose.
			if structuredEnabled && structuredCalls == 0 && text == "pi v0.84.4\n---\n" {
				continue
			}
			if structuredEnabled {
				// Native structured_output arguments are the sole authority. Keep
				// prose out of both result construction and streaming output; retain
				// only a bounded presence bit for the lifecycle warning.
				if evidence != nil {
					evidence.assistantProse = true
				}
				continue
			}
			output.WriteString(text)
			if onChunk != nil {
				onChunk(text)
			}
		case "tool_call":
			if !structuredEnabled {
				continue
			}
			if structuredCalls > 0 {
				failProtocol("tool activity followed the authoritative structured_output completion")
			}
			if update.ToolCallID == "" {
				failProtocol("tool call is missing an id")
				continue
			}
			name := acpxStructuredToolName(update)
			if name == "" {
				failProtocol("tool call is missing a name")
			} else if name != "structured_output" {
				wrongToolSeen = true
			}
			if _, duplicateID := activeTools[update.ToolCallID]; duplicateID {
				failProtocol("duplicate tool call id was emitted")
			}
			if len(activeTools) != 0 {
				failProtocol("coissued tool calls were emitted")
			}
			activeTools[update.ToolCallID] = name
		case "tool_call_update":
			if !structuredEnabled {
				continue
			}
			if update.ToolCallID == "" {
				failProtocol("tool update is missing an id")
				continue
			}
			name, known := activeTools[update.ToolCallID]
			if !known {
				failProtocol("tool update has no preceding tool call")
				continue
			}
			if updateName := acpxStructuredToolName(update); updateName != "" && updateName != name {
				failProtocol("tool name changed during the call")
			}
			switch update.Status {
			case "failed":
				// The strict Pi extension may repair a schema/field
				// formatting failure in the same session. A failed final-tool
				// attempt has no authoritative result and no side effects.
				delete(activeTools, update.ToolCallID)
			case "completed":
				delete(activeTools, update.ToolCallID)
				if name != "structured_output" {
					failProtocol("competing tool completed")
					continue
				}
				structuredCalls++
				if structuredCalls != 1 || structuredOutput != "" {
					failProtocol("structured_output completed more than once")
					continue
				}
				text, ok := acpxCompletedToolText(update)
				if !ok {
					failProtocol("completed structured_output has no exact text result")
					continue
				}
				structuredOutput = text
				structuredCallID = update.ToolCallID
			case "", "in_progress", "pending":
				// Non-terminal progress is expected.
			default:
				failProtocol("tool call has invalid terminal status " + update.Status)
			}
		case "agent_thought_chunk":
			if structuredEnabled && (len(activeTools) > 0 || structuredCalls > 0) && acpxUpdateText(update) != "" {
				failProtocol("thought activity occurred during or after a final tool call")
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", stdoutErr, err
	}
	if !structuredEnabled {
		return output.String(), stdoutErr, nil
	}
	if protocolReason != "" {
		return "", stdoutErr, acpxProtocolError(protocolReason)
	}
	if len(activeTools) != 0 {
		return "", stdoutErr, acpxProtocolError("tool call remained incomplete at end of stream")
	}
	if wrongToolSeen {
		return "", stdoutErr, acpxProtocolError("competing tool call was emitted")
	}
	if structuredCalls == 0 {
		return "", stdoutErr, acpxProtocolError("no structured_output call completed")
	}
	if structuredCalls != 1 {
		return "", stdoutErr, acpxProtocolError("multiple structured_output calls were emitted")
	}
	if structuredOutput == "" || structuredCallID == "" {
		return "", stdoutErr, acpxProtocolError("structured_output call did not complete with a result")
	}
	return structuredOutput, stdoutErr, nil
}

// parseAcpxLegacyStructuredJSONEvents preserves the pre-existing ACP extractor
// for targets that did not explicitly select the one-attempt strict protocol.
// Those targets keep their established Pi parse/schema retry behavior.
func parseAcpxLegacyStructuredJSONEvents(ctx context.Context, r io.Reader, onChunk func(string), usage *TokenUsage) (string, string, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), acpxScannerMaxTokenSize)
	var output strings.Builder
	var stdoutErr string
	toolNames := make(map[string]string)
	activeTools := make(map[string]string)
	structuredEligible := make(map[string]bool)
	var structuredOutput string
	var structuredCallID string

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return "", stdoutErr, ctx.Err()
		default:
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg acpxJSONMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		markAcpxUsagePresence(line, &msg)
		if msg.Error != nil && msg.Error.Message != "" && stdoutErr == "" {
			stdoutErr = msg.Error.Message
		}
		*usage = acpxMaxUsage(*usage, acpxUsageFieldsToTokenUsage(msg.Result.Usage))
		if msg.Method != "session/update" {
			continue
		}
		update := msg.Params.Update
		switch update.SessionUpdate {
		case "usage_update":
			*usage = acpxMaxUsage(*usage, acpxUpdateUsage(update))
		case "agent_message_chunk":
			text := acpxUpdateText(update)
			if text == "" {
				continue
			}
			acpxInvalidateActiveStructuredOutput(activeTools, structuredEligible)
			structuredOutput = ""
			structuredCallID = ""
			output.WriteString(text)
			if onChunk != nil {
				onChunk(text)
			}
		case "tool_call":
			if update.ToolCallID != "" {
				if structuredOutput != "" {
					structuredOutput = ""
					structuredCallID = ""
				}
				name := acpxStructuredToolName(update)
				acpxInvalidateActiveStructuredOutput(activeTools, structuredEligible)
				toolNames[update.ToolCallID] = name
				structuredEligible[update.ToolCallID] = name == "structured_output" && len(activeTools) == 0
				activeTools[update.ToolCallID] = name
			}
		case "tool_call_update":
			if update.ToolCallID == "" {
				continue
			}
			name, known := toolNames[update.ToolCallID]
			if !known {
				name = acpxStructuredToolName(update)
				acpxInvalidateActiveStructuredOutput(activeTools, structuredEligible)
				toolNames[update.ToolCallID] = name
				structuredEligible[update.ToolCallID] = name == "structured_output" && len(activeTools) == 0
				activeTools[update.ToolCallID] = name
			}
			if structuredOutput != "" && update.ToolCallID != structuredCallID {
				structuredOutput = ""
				structuredCallID = ""
			}
			if update.Status == "failed" {
				delete(activeTools, update.ToolCallID)
				continue
			}
			if update.Status != "completed" {
				continue
			}
			delete(activeTools, update.ToolCallID)
			if name != "structured_output" || !structuredEligible[update.ToolCallID] || len(activeTools) != 0 {
				continue
			}
			text, ok := acpxCompletedToolText(update)
			if !ok {
				continue
			}
			structuredOutput = text
			structuredCallID = update.ToolCallID
		case "agent_thought_chunk":
			if acpxUpdateText(update) == "" {
				continue
			}
			acpxInvalidateActiveStructuredOutput(activeTools, structuredEligible)
			if structuredOutput != "" {
				structuredOutput = ""
				structuredCallID = ""
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", stdoutErr, err
	}
	if structuredOutput != "" {
		return structuredOutput, stdoutErr, nil
	}
	return output.String(), stdoutErr, nil
}

func acpxInvalidateActiveStructuredOutput(activeTools map[string]string, structuredEligible map[string]bool) {
	for id, activeName := range activeTools {
		if activeName == "structured_output" {
			structuredEligible[id] = false
		}
	}
}

func acpxStructuredToolName(update acpxSessionUpdate) string {
	if update.Name != "" {
		return update.Name
	}
	return update.Title
}

func acpxCompletedToolText(update acpxSessionUpdate) (string, bool) {
	var content []struct {
		Type    string `json:"type"`
		Content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(update.Content, &content) == nil && len(content) == 1 &&
		content[0].Type == "content" && content[0].Content.Type == "text" && content[0].Content.Text != "" {
		return content[0].Content.Text, true
	}
	return "", false
}

func acpxUpdateUsage(update acpxSessionUpdate) TokenUsage {
	usage := acpxUsageFieldsToTokenUsage(update.acpxUsageFields)
	metaUsage := acpxUsageFieldsToTokenUsage(update.Meta.Usage)
	usage = acpxMaxUsage(usage, metaUsage)
	if update.Used > usage.InputTokens {
		usage.InputTokens = update.Used
	}
	usage.Reported = usage.Reported || update.usedReported || update.Used != 0
	return usage
}

func acpxUsageFieldsToTokenUsage(fields acpxUsageFields) TokenUsage {
	return TokenUsage{
		Reported: fields.reported || acpxUsageFieldsHaveValues(fields),
		CacheCreationReported: fields.cacheCreationReported || acpxFirstPositive(
			fields.CacheCreationInputTokens,
			fields.CacheWriteInputTokens,
			fields.CacheWriteTokens,
			fields.CacheCreationInputTokensCamel,
			fields.CacheCreationTokensCamel,
			fields.CacheWriteTokensCamel,
			fields.CachedWriteTokensCamel,
		) > 0,
		InputTokens: acpxFirstPositive(
			fields.InputTokens,
			fields.InputTokensCamel,
		),
		OutputTokens: acpxFirstPositive(
			fields.OutputTokens,
			fields.OutputTokensCamel,
		),
		CacheReadTokens: acpxFirstPositive(
			fields.CacheReadInputTokens,
			fields.CacheReadTokens,
			fields.CachedInputTokens,
			fields.CacheReadInputTokensCamel,
			fields.CachedInputTokensCamel,
			fields.CacheReadTokensCamel,
			fields.CachedReadTokensCamel,
		),
		CacheCreationTokens: acpxFirstPositive(
			fields.CacheCreationInputTokens,
			fields.CacheWriteInputTokens,
			fields.CacheWriteTokens,
			fields.CacheCreationInputTokensCamel,
			fields.CacheCreationTokensCamel,
			fields.CacheWriteTokensCamel,
			fields.CachedWriteTokensCamel,
		),
	}
}

func markAcpxUsagePresence(line []byte, msg *acpxJSONMessage) {
	var raw struct {
		Result struct {
			Usage json.RawMessage `json:"usage"`
		} `json:"result"`
		Params struct {
			Update json.RawMessage `json:"update"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &raw) != nil {
		return
	}
	markAcpxUsageFields(raw.Result.Usage, &msg.Result.Usage)
	if len(raw.Params.Update) == 0 {
		return
	}
	var update map[string]json.RawMessage
	if json.Unmarshal(raw.Params.Update, &update) != nil {
		return
	}
	markAcpxUsageFields(raw.Params.Update, &msg.Params.Update.acpxUsageFields)
	if _, ok := update["used"]; ok {
		msg.Params.Update.usedReported = true
	}
	if meta, ok := update["_meta"]; ok {
		var metaFields struct {
			Usage json.RawMessage `json:"usage"`
		}
		if json.Unmarshal(meta, &metaFields) == nil {
			markAcpxUsageFields(metaFields.Usage, &msg.Params.Update.Meta.Usage)
		}
	}
}

func markAcpxUsageFields(raw json.RawMessage, fields *acpxUsageFields) {
	if len(raw) == 0 || fields == nil {
		return
	}
	var values map[string]json.RawMessage
	if json.Unmarshal(raw, &values) != nil {
		return
	}
	usageKeys := []string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_read_tokens", "cached_input_tokens", "inputTokens", "outputTokens", "cacheReadInputTokens", "cachedInputTokens", "cacheReadTokens", "cachedReadTokens"}
	cacheKeys := []string{"cache_creation_input_tokens", "cache_write_input_tokens", "cache_write_tokens", "cacheCreationInputTokens", "cacheCreationTokens", "cacheWriteTokens", "cachedWriteTokens"}
	for _, key := range usageKeys {
		if _, ok := values[key]; ok {
			fields.reported = true
		}
	}
	for _, key := range cacheKeys {
		if _, ok := values[key]; ok {
			fields.reported = true
			fields.cacheCreationReported = true
		}
	}
}

func acpxUsageFieldsHaveValues(fields acpxUsageFields) bool {
	fields.reported = false
	fields.cacheCreationReported = false
	return fields != (acpxUsageFields{})
}

func acpxMaxUsage(a, b TokenUsage) TokenUsage {
	return TokenUsage{
		Reported:              a.Reported || b.Reported,
		CacheCreationReported: a.CacheCreationReported || b.CacheCreationReported,
		InputTokens:           max(a.InputTokens, b.InputTokens),
		OutputTokens:          max(a.OutputTokens, b.OutputTokens),
		CacheReadTokens:       max(a.CacheReadTokens, b.CacheReadTokens),
		CacheCreationTokens:   max(a.CacheCreationTokens, b.CacheCreationTokens),
	}
}

func acpxFirstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func acpxUpdateText(update acpxSessionUpdate) string {
	if update.Text != "" {
		return update.Text
	}
	if len(update.Content) == 0 {
		return ""
	}
	var content struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(update.Content, &content); err == nil && content.Text != "" {
		return content.Text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(update.Content, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, part := range parts {
		if part.Text != "" {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

func estimateAcpxTokens(charCount int) int {
	if charCount <= 0 {
		return 0
	}
	return (charCount + 3) / 4
}
