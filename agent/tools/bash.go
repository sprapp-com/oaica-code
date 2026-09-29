package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ollama/ollama/agent"
	"github.com/ollama/ollama/api"
)

const (
	bashTimeout        = 3 * time.Minute
	bashWaitDelay      = 1 * time.Second
	maxBashOutputBytes = 60_000
)

type Bash struct{}

func (b *Bash) Name() string {
	return shellToolName()
}

func (b *Bash) Description() string {
	return shellToolDescription()
}

func (b *Bash) Schema() api.ToolFunction {
	props := api.NewToolPropertiesMap()
	props.Set("command", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: shellCommandDescription(),
	})
	return api.ToolFunction{
		Name:        b.Name(),
		Description: b.Description(),
		Parameters: api.ToolFunctionParameters{
			Type:       "object",
			Properties: props,
			Required:   []string{"command"},
		},
	}
}

func (b *Bash) RequiresApproval(map[string]any) bool {
	return true
}

// ApprovalScope scopes shell approval to the exact, trimmed command string
// using a NUL separator: "<tool>\x00<command>". "Always allow this command"
// matches ONLY that precise string — any whitespace, quoting, or casing
// variant re-prompts. The NUL separator is safe because a shell command
// string cannot contain a literal NUL.
// ScopeUsesWorkingDir: a command's effect depends on the directory it runs in exactly as a relative path
// does, so "y" to `rm -rf src` in A must not approve it in B after a `cd B` (2026-09-29 audit, round 129,
// F129-L1-1).
func (b *Bash) ScopeUsesWorkingDir(map[string]any) bool { return true }

func (b *Bash) ApprovalScope(args map[string]any) string {
	name := b.Name()
	if command, ok := args["command"].(string); ok {
		command = strings.TrimSpace(command)
		if command != "" {
			return name + "\x00" + command
		}
	}
	return name
}

func (b *Bash) Execute(ctx context.Context, toolCtx agent.ToolContext, args map[string]any) (agent.ToolResult, error) {
	// TODO: use shared agent.RequiredStringArg for the "command" parameter (see agent package cleanup plan).
	command, ok := args["command"].(string)
	if !ok || strings.TrimSpace(command) == "" {
		return agent.ToolResult{}, fmt.Errorf("command parameter is required")
	}
	if err := rejectUnsafeShellCommand(command); err != nil {
		return agent.ToolResult{}, err
	}
	if err := refuseCredentialWords(toolCtx.WorkingDir, command); err != nil {
		return agent.ToolResult{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, bashTimeout)
	defer cancel()

	cwdFile, err := os.CreateTemp("", "ollama-agent-cwd-*")
	if err != nil {
		return agent.ToolResult{}, err
	}
	cwdPath := cwdFile.Name()
	_ = cwdFile.Close()
	defer os.Remove(cwdPath)

	cmd := newBashCommand(ctx, command, cwdPath)
	cmd.WaitDelay = bashWaitDelay
	cmd.Cancel = func() error {
		return killBashCommand(cmd)
	}
	if toolCtx.WorkingDir != "" {
		cmd.Dir = toolCtx.WorkingDir
	}

	var stdout, stderr boundedOutput
	stdout.Limit = maxBashOutputBytes
	stderr.Limit = maxBashOutputBytes
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = runBashCommand(cmd)
	finalWorkingDir := readFinalWorkingDir(cwdPath)

	var sb strings.Builder
	if stdout.Len() > 0 {
		sb.WriteString(stdout.String("stdout"))
	}
	if stderr.Len() > 0 {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString("stderr:\n")
		sb.WriteString(stderr.String("stderr"))
	}

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return agent.ToolResult{Content: bashContentWithError(sb.String(), "Error: command timed out after "+bashTimeout.String()), WorkingDir: finalWorkingDir}, nil
		}
		if ctx.Err() == context.Canceled {
			return agent.ToolResult{Content: bashContentWithError(sb.String(), "Error: command was canceled"), WorkingDir: finalWorkingDir}, nil
		}
		if errors.Is(err, exec.ErrWaitDelay) {
			_ = killBashCommand(cmd)
			return agent.ToolResult{Content: bashContentWithError(sb.String(), "Error: command output pipes did not close after "+bashWaitDelay.String()), WorkingDir: finalWorkingDir}, nil
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			return agent.ToolResult{Content: bashContentWithError(sb.String(), fmt.Sprintf("Exit code: %d", exitErr.ExitCode())), WorkingDir: finalWorkingDir}, nil
		}
		return agent.ToolResult{Content: sb.String(), WorkingDir: finalWorkingDir}, fmt.Errorf("executing command: %w", err)
	}

	if sb.Len() == 0 {
		return agent.ToolResult{Content: "(no output)", WorkingDir: finalWorkingDir}, nil
	}
	return agent.ToolResult{Content: sb.String(), WorkingDir: finalWorkingDir}, nil
}

func bashContentWithError(content, msg string) string {
	if content == "" {
		return msg
	}
	return content + "\n\n" + msg
}

// rejectUnsafeShellCommand applies a best-effort blocklist for obviously
// destructive or credential-exfiltrating commands. It is defense-in-depth
// ONLY: the interactive approval prompt is the real security control, and
// this check must not be relied upon as a sandbox. Sophisticated or novel
// dangerous commands (e.g. find / -delete, dd, fork bombs, custom binaries)
// are NOT caught here and will simply be routed through approval like any
// other command. Keep the approval prompt as the gate.
func rejectUnsafeShellCommand(command string) error {
	switch {
	case hasUnsafeRecursiveDelete(command):
		return fmt.Errorf("refusing to run unsafe command: recursive delete target is too broad")
	case readsCredentialPath(command):
		return fmt.Errorf("refusing to run unsafe command: credential file reads are not allowed")
	default:
		return nil
	}
}

func hasUnsafeRecursiveDelete(command string) bool {
	// Check each command segment independently. shellSafetyText flattens
	// separators (; & | newlines) to spaces, which would otherwise let the
	// rm target scan bleed across command boundaries — e.g.
	// "rm -rf build && echo ~/.ssh/config" flattened to one token stream
	// would treat the unrelated ~/.ssh/config (a ~/-prefixed "unsafe
	// target") as an rm argument. Splitting on separators first restores
	// command boundaries while still catching multi-target single commands
	// like "rm -rf build /etc".
	var piped []string // the literal words `echo`/`printf` in the segment just before hand to `xargs rm -rf`
	for _, segment := range shellSegments(command) {
		// `r\m` is rm to the shell; the backslash-to-slash rewrite turned it into `r/m`, while a Windows path needs
		// that rewrite: scan both spellings.
		for _, fields := range [][]string{shellSafetyFields(segment), shellSafetyFields(strings.ReplaceAll(segment, "\\", ""))} {
			for i, field := range fields {
				if isRMCommand(field) && rmCommandDeletesUnsafeTarget(fields[i+1:]) {
					return true
				}
				if field == "xargs" {
					// Flags and wrappers (`-0`, `-I{} `, `sudo`) sit between xargs and the rm it runs.
					for j := i + 1; j < len(fields); j++ {
						if isRMCommand(fields[j]) {
							if rmCommandDeletesUnsafeTarget(append(append([]string{}, fields[j+1:]...), piped...)) {
								return true
							}
							break
						}
					}
				}
				if isPowerShellDeleteCommand(field) && powerShellDeleteCommandDeletesUnsafeTarget(fields[i+1:]) {
					return true
				}
			}
		}
		piped = nil
		if f := shellSafetyFields(segment); len(f) > 1 && (f[0] == "echo" || f[0] == "printf") {
			for _, w := range f[1:] {
				if !strings.HasPrefix(w, "-") {
					piped = append(piped, w)
				}
			}
		}
	}
	return false
}

// shellSegments splits a command on shell control operators (;, &, |, &&,
// ||) and newlines, returning the individual command segments. It operates on
// the lowercased raw command before quote/separator normalization so that
// command boundaries are preserved for per-segment checks. Subshell parens are
// intentionally NOT treated as separators: splitting on them would fragment
// command substitutions like "rm -rf $(echo /)" into "rm -rf $" and "echo /",
// hiding the destructive "/" target from the per-segment scan. Empty segments
// are dropped.
func shellSegments(command string) []string {
	command = strings.ToLower(command)
	var segments []string
	for _, segment := range strings.FieldsFunc(command, func(r rune) bool {
		switch r {
		case ';', '&', '|', '\n', '\r':
			return true
		}
		return false
	}) {
		if segment = strings.TrimSpace(segment); segment != "" {
			segments = append(segments, segment)
		}
	}
	return segments
}

func rmCommandDeletesUnsafeTarget(fields []string) bool {
	var flags string
	for _, field := range fields {
		if field == "--" {
			continue
		}
		if strings.HasPrefix(field, "-") {
			flags += field
			continue
		}
		if strings.Contains(flags, "r") && strings.Contains(flags, "f") && isUnsafeDeleteTarget(field) {
			return true
		}
	}
	return false
}

func powerShellDeleteCommandDeletesUnsafeTarget(fields []string) bool {
	var recurse, force bool
	var targets []string
	for _, field := range fields {
		switch field {
		case "-r", "-recurse", "-recursive":
			recurse = true
		case "-f", "-force":
			force = true
		default:
			if !strings.HasPrefix(field, "-") {
				targets = append(targets, field)
			}
		}
	}
	if !recurse || !force {
		return false
	}
	for _, target := range targets {
		if isUnsafeDeleteTarget(target) {
			return true
		}
	}
	return false
}

func readsCredentialPath(command string) bool {
	if !commandHasCredentialReadVerb(command) {
		return false
	}
	normalized := shellSafetyText(command)
	// `~/.ssh/./id_rsa` and `~/.ssh//id_rsa` name the same file as `~/.ssh/id_rsa`.
	for strings.Contains(normalized, "//") {
		normalized = strings.ReplaceAll(normalized, "//", "/")
	}
	normalized = strings.ReplaceAll(normalized, "/./", "/")
	for _, fragment := range credentialPathFragments {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	// A glob only matters in the segment that reads, and a quoted word is not expanded by the shell.
	for _, segment := range shellSegments(command) {
		if !commandHasCredentialReadVerb(segment) {
			continue
		}
		for _, word := range strings.Fields(strings.NewReplacer("(", " ", ")", " ", "<", " ", ">", " ").Replace(segment)) {
			if !strings.ContainsAny(word, "'\"`") && globNamesCredential(word) {
				return true
			}
		}
	}
	return false
}

// commandHasCredentialReadVerb asks both spellings: `ca\t` is cat to the shell, while the backslash-to-slash
// rewrite reads it as a path (2026-09-29 audit, round 130, F130-L1-7).
func commandHasCredentialReadVerb(command string) bool {
	return hasCredentialReadVerb(shellSafetyFields(command)) || hasCredentialReadVerb(shellSafetyFields(strings.ReplaceAll(command, "\\", "")))
}

// globNamesCredential reports whether a shell glob word (`~/.ssh/id_rs?`, `~/.ssh/id_*`) can expand to a
// credential path: its trailing segments match a fragment's segments one for one.
func globNamesCredential(word string) bool {
	if !strings.ContainsAny(word, "*?[") {
		return false
	}
	segs := strings.Split(strings.Trim(word, "/"), "/")
	for _, fragment := range credentialPathFragments {
		want := strings.Split(strings.Trim(fragment, "/"), "/")
		if len(segs) < len(want) {
			continue
		}
		tail := segs[len(segs)-len(want):]
		ok := true
		for i := range want {
			// A shell glob never matches a dot-name unless its own pattern starts with a literal dot.
			if strings.HasPrefix(want[i], ".") && !strings.HasPrefix(tail[i], ".") {
				ok = false
				break
			}
			if m, err := path.Match(tail[i], want[i]); err != nil || !m {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// refuseCredentialWords is the door-parity half of readsCredentialPath: a word that lands on a credential file
// once relative paths and directory symlinks are resolved (`keys/id_rsa` with `keys -> ~/.ssh`) is refused,
// as the Read tool refuses it (2026-09-29 audit, round 129, F129-L1-3).
func refuseCredentialWords(workingDir, command string) error {
	if !commandHasCredentialReadVerb(command) {
		return nil
	}
	for _, word := range strings.Fields(strings.NewReplacer(";", " ", "&", " ", "|", " ", "(", " ", ")", " ", "\"", "", "'", "", "<", " ", ">", " ").Replace(command)) {
		if strings.HasPrefix(word, "-") || strings.ContainsAny(word, "*?[$`") || !strings.Contains(word, "/") {
			continue
		}
		if strings.HasPrefix(word, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				word = filepath.Join(home, word[2:])
			}
		}
		if err := refuseCredentialPath(workingDir, word); err != nil {
			return fmt.Errorf("refusing to run unsafe command: credential file reads are not allowed")
		}
	}
	return nil
}

// credentialPathFragments name the files whose contents are credentials. The bash tool refuses to read
// them; the read and edit tools apply the same list to the path they are given, absolute or not
// (2026-09-29 audit, round 127, F127-L1-4).
var credentialPathFragments = []string{
	"/.ssh/id_rsa",
	"/.ssh/id_dsa",
	"/.ssh/id_ecdsa",
	"/.ssh/id_ed25519",
	"/.ssh/config",
	"/.ssh/known_hosts",
	"/.aws/credentials",
	"/.aws/config",
	"/.config/gcloud/application_default_credentials.json",
	"/.kube/config",
	"/.netrc",
	"/.npmrc",
	"/.docker/config.json",
	"/.config/gh/hosts.yml",
	"/.gnupg/",
	"/etc/shadow",
}

// refuseCredentialPath is the file tools' form of the bash denylist: path is resolved against
// workingDir when relative.
func refuseCredentialPath(workingDir, path string) error {
	p := filepath.Clean(strings.TrimSpace(path))
	if !filepath.IsAbs(p) {
		p = filepath.Join(workingDir, p)
	}
	// The path as written AND where it lands: a directory symlink in the tree (`keys -> ~/.ssh`) names a
	// credential file by an in-tree-looking path, and a case-insensitive filesystem serves `.SSH/ID_RSA`
	// (bash lowercases the command; this did not) (2026-09-29 audit, round 128, F128-L1-3).
	candidates := []string{p}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		candidates = append(candidates, resolved)
	}
	for _, c := range candidates {
		c = strings.ToLower(filepath.ToSlash(c))
		for _, fragment := range credentialPathFragments {
			if strings.Contains(c, fragment) || strings.HasSuffix(c, strings.TrimSuffix(fragment, "/")) {
				return fmt.Errorf("refusing to touch %s: credential file reads are not allowed", path)
			}
		}
	}
	return nil
}

func hasCredentialReadVerb(fields []string) bool {
	for _, field := range fields {
		// `/bin/cat` and `\cat` (normalised to `/cat`) are the verb too, exactly as isRMCommand accepts `/bin/rm`.
		if i := strings.LastIndex(field, "/"); i >= 0 {
			field = field[i+1:]
		}
		switch field {
		case "cat", "less", "more", "head", "tail", "type", "get-content", "gc", "select-string", "grep", "rg", "sed", "awk":
			return true
		case "env", "printenv":
			return true
		}
	}
	return false
}

func isRMCommand(field string) bool {
	return field == "rm" || strings.HasSuffix(field, "/rm")
}

func isPowerShellDeleteCommand(field string) bool {
	switch field {
	case "remove-item", "del", "erase", "rd", "rmdir":
		return true
	default:
		return false
	}
}

func isUnsafeDeleteTarget(target string) bool {
	if target == "." || target == "./" || target == "*" {
		return true
	}
	// `**`, `?*` and `/**`, `/?*` expand to the same everything as `*` and `/*`.
	if bare := strings.TrimPrefix(target, "/"); bare != "" && strings.Trim(bare, "*?") == "" && strings.Contains(bare, "*") {
		return true
	}
	if target == "/*" {
		return true
	}
	target = strings.TrimSuffix(target, "/*")
	for _, prefix := range []string{"~/", "$home/", "${home}/", "$env:home/", "$env:userprofile/", "%userprofile%/"} {
		if strings.HasPrefix(target, prefix) {
			return true
		}
	}
	for _, prefix := range []string{"/etc/", "/bin/", "/sbin/", "/usr/", "/var/", "/lib/", "/library/", "/system/", "/applications/", "c:/windows/", "c:/program files/"} {
		if strings.HasPrefix(target, prefix) {
			return true
		}
	}
	for _, exact := range []string{"/", "~", "$home", "${home}", "$env:home", "$env:userprofile", "%userprofile%", "c:", "c:/", "/etc", "/bin", "/sbin", "/usr", "/var", "/lib", "/library", "/system", "/applications", "c:/windows", "c:/program files"} {
		if target == exact {
			return true
		}
	}
	return false
}

func shellSafetyFields(command string) []string {
	return strings.Fields(shellSafetyText(command))
}

func shellSafetyText(command string) string {
	command = strings.ToLower(command)
	return strings.NewReplacer(
		"\\", "/",
		"\n", " ",
		"\t", " ",
		";", " ",
		"&", " ",
		"|", " ",
		"(", " ",
		")", " ",
		"\"", "",
		"'", "",
		"`", "",
	).Replace(command)
}

func readFinalWorkingDir(path string) string {
	content, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	workingDir := strings.TrimPrefix(string(content), "\ufeff")
	workingDir = strings.TrimSpace(workingDir)
	if workingDir == "" {
		return ""
	}
	workingDir = normalizeBashWorkingDir(workingDir)
	info, err := os.Stat(workingDir)
	if err != nil || !info.IsDir() {
		return ""
	}
	return workingDir
}

func normalizeBashWorkingDir(workingDir string) string {
	if runtime.GOOS == "windows" && len(workingDir) >= 3 && workingDir[0] == '/' && workingDir[2] == '/' && isASCIIAlpha(workingDir[1]) {
		workingDir = strings.ToUpper(string(workingDir[1])) + ":" + workingDir[2:]
	}
	workingDir = filepath.Clean(filepath.FromSlash(workingDir))
	if runtime.GOOS == "windows" && len(workingDir) >= 2 && workingDir[1] == ':' && isASCIIAlpha(workingDir[0]) {
		workingDir = strings.ToUpper(string(workingDir[0])) + workingDir[1:]
	}
	return workingDir
}

func isASCIIAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

type boundedOutput struct {
	Limit   int
	buf     []byte
	omitted int
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.Limit <= 0 {
		b.omitted += len(p)
		return len(p), nil
	}
	remaining := b.Limit - len(b.buf)
	if remaining <= 0 {
		b.omitted += len(p)
		return len(p), nil
	}
	if len(p) <= remaining {
		b.buf = append(b.buf, p...)
		return len(p), nil
	}
	writeLen := utf8SafePrefixLen(p[:remaining])
	b.buf = append(b.buf, p[:writeLen]...)
	b.omitted += len(p) - writeLen
	return len(p), nil
}

func (b *boundedOutput) Len() int {
	return len(b.buf) + b.omitted
}

func (b *boundedOutput) String(label string) string {
	safeLen := utf8SafePrefixLen(b.buf)
	content := string(b.buf[:safeLen])
	omitted := b.omitted + len(b.buf) - safeLen
	if omitted == 0 {
		return content
	}
	return content + agent.TruncMarker(label, safeLen, 0, omitted, false, "")
}

func utf8SafePrefixLen(p []byte) int {
	if len(p) == 0 {
		return 0
	}
	for i := 0; i < len(p); {
		r, size := utf8.DecodeRune(p[i:])
		if r == utf8.RuneError && size == 1 {
			return i
		}
		i += size
	}
	return len(p)
}
