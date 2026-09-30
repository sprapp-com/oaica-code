package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
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
			// One pass per spelling: an rm at index i is unsafe iff an unsafe target follows it with both -r and -f
			// between them; a PowerShell delete iff recurse, force and an unsafe target all follow it. Rescanning the
			// tail for every rm word was cubic: `rm -a rm -a …` took 1.2 s at 12 KB and two minutes at 48 KB, with no
			// timeout (2026-09-30 audit, round 138, F138-A-3).
			rmBoundary, psR, psF, psT := deleteBoundaries(fields)
			nextRm := make([]int, len(fields)) // nextRm[i] = the first rm word after i, or -1
			nxt := -1
			for k := len(fields) - 1; k >= 0; k-- {
				nextRm[k] = nxt
				if isRMCommand(fields[k]) {
					nxt = k
				}
			}
			xargsBoundary, _, _, _ := deleteBoundaries(append(append([]string{}, fields...), piped...))
			for i, field := range fields {
				if isRMCommand(field) && i < rmBoundary {
					return true
				}
				if field == "xargs" {
					// Flags and wrappers (`-0`, `-I{} `, `sudo`) sit between xargs and the rm it runs; the words
					// echo/printf piped in extend the rm's arguments. Both answers come from one precomputed pass.
					if j := nextRm[i]; j >= 0 && j < xargsBoundary {
						return true
					}
				}
				if isPowerShellDeleteCommand(field) && psR > i && psF > i && psT > i {
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

// deleteBoundaries: for fields, (a) M such that an rm word at index i deletes an unsafe target iff i < M (a target t
// counts when a -r flag and a -f flag both stand between i and t: M = max over unsafe t of min(last r-flag before t,
// last f-flag before t)); (b) the last index of a PowerShell recurse flag, of a force flag, and of an unsafe target.
func deleteBoundaries(fields []string) (rm, psRecurse, psForce, psTarget int) {
	rm, psRecurse, psForce, psTarget = -1, -1, -1, -1
	lastR, lastF := -1, -1
	for i, f := range fields {
		switch f {
		case "-r", "-recurse", "-recursive":
			psRecurse = i
		case "-f", "-force":
			psForce = i
		}
		if f == "--" {
			continue
		}
		if strings.HasPrefix(f, "-") {
			if strings.Contains(f, "r") {
				lastR = i
			}
			if strings.Contains(f, "f") {
				lastF = i
			}
			continue
		}
		if isUnsafeDeleteTarget(f) {
			psTarget = i
			if m := min(lastR, lastF); m > rm {
				rm = m
			}
		}
	}
	return
}

func rmCommandDeletesUnsafeTarget(fields []string) bool {
	// Booleans, not a growing string (used for `xargs rm`, whose targets come from elsewhere).
	var sawR, sawF bool
	for _, field := range fields {
		if field == "--" {
			continue
		}
		if strings.HasPrefix(field, "-") {
			if strings.Contains(field, "r") {
				sawR = true
			}
			if strings.Contains(field, "f") {
				sawF = true
			}
			continue
		}
		if sawR && sawF && isUnsafeDeleteTarget(field) {
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
	for _, text := range []string{normalized, shellSafetyText(strings.ReplaceAll(command, "\\", "")), shellDecodedText(command)} {
		for _, fragment := range credentialPathFragments {
			if containsCredentialFragment(text, fragment) {
				return true
			}
		}
	}
	// A glob only matters in the segment that reads, and a quoted word is not expanded by the shell.
	for _, segment := range shellSegments(command) {
		if !commandHasCredentialReadVerb(segment) {
			continue
		}
		for _, word := range shellWords(strings.NewReplacer("(", " ", ")", " ", "<", " ", ">", " ").Replace(segment)) {
			if globNamesCredential(unquotedGlobPattern(word)) {
				return true
			}
		}
	}
	return false
}

// shellWords splits on whitespace outside quotes, so a quoted argument (`awk '$1 ~ /.*/'`) stays one word.
func shellWords(s string) []string {
	var words []string
	var cur strings.Builder
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			cur.WriteRune(r)
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"' || r == '`':
			quote = r
			cur.WriteRune(r)
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if cur.Len() > 0 {
				words = append(words, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		words = append(words, cur.String())
	}
	return words
}

// unquotedGlobPattern turns a shell word into a path.Match pattern the way the shell reads it: characters inside
// quotes are literal (escaped), unquoted `*?[` stay live, and `\` outside quotes is the Windows separator. A
// word with quotes AND a live glob (`~/.ssh/"id_"*`) is still a glob (2026-09-29 audit, round 131, F131-L1-1/2).
func unquotedGlobPattern(word string) string {
	var b strings.Builder
	var quote rune
	for _, r := range word {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else if strings.ContainsRune(`*?[\`, r) {
				b.WriteRune('\\')
				b.WriteRune(r)
			} else {
				b.WriteRune(r)
			}
		case r == '\'' || r == '"' || r == '`':
			quote = r
		case r == '\\':
			b.WriteRune('/')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// commandHasCredentialReadVerb asks both spellings: `ca\t` is cat to the shell, while the backslash-to-slash
// rewrite reads it as a path (2026-09-29 audit, round 130, F130-L1-7).
func commandHasCredentialReadVerb(command string) bool {
	return hasCredentialReadVerb(shellSafetyFields(command)) || hasCredentialReadVerb(shellSafetyFields(strings.ReplaceAll(command, "\\", "")))
}

// globNamesCredential reports whether a shell glob word (`~/.ssh/id_rs?`, `~/.ssh/id_*`) can expand to a
// credential path: its trailing segments match a fragment's segments one for one.
func globNamesCredential(word string) bool {
	if !strings.ContainsAny(word, "*?[") || !strings.Contains(word, "/") {
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
	// A `cd` earlier in the same command moves where later relative words point (`cd ~/.ssh && cat id_rsa`);
	// the next tool call would be caught, this one was not (round 132, F132-L1-4).
	cwd := workingDir
	for _, segment := range strings.FieldsFunc(command, func(r rune) bool { return r == ';' || r == '&' || r == '|' || r == '\n' || r == '\r' }) {
		words := shellWords(strings.NewReplacer("(", " ", ")", " ", "<", " ", ">", " ").Replace(segment))
		if len(words) >= 2 && words[0] == "cd" {
			dir := strings.Trim(words[1], `"'`)
			if strings.HasPrefix(dir, "~") {
				if home, err := os.UserHomeDir(); err == nil {
					dir = filepath.Join(home, dir[1:])
				}
			}
			if filepath.IsAbs(dir) {
				cwd = dir
			} else if dir != "" && !strings.Contains(dir, "$") {
				cwd = filepath.Join(cwd, dir)
			}
			continue
		}
		for _, word := range words {
			word = strings.Trim(word, `"'`)
			if strings.HasPrefix(word, "-") || strings.ContainsAny(word, "$`") || word == "" {
				continue
			}
			if strings.ContainsAny(word, "*?[") {
				// A glob is judged where it lands: relative to the directory a `cd` moved to.
				if cwd != workingDir && globNamesCredential(filepath.ToSlash(filepath.Join(cwd, unquotedGlobPattern(word)))) {
					return fmt.Errorf("refusing to run unsafe command: credential file reads are not allowed")
				}
				continue
			}
			if !strings.Contains(word, "/") && cwd == workingDir {
				continue
			}
			if strings.HasPrefix(word, "~/") {
				if home, err := os.UserHomeDir(); err == nil {
					word = filepath.Join(home, word[2:])
				}
			}
			if err := refuseCredentialPath(cwd, word); err != nil {
				return fmt.Errorf("refusing to run unsafe command: credential file reads are not allowed")
			}
		}
	}
	return nil
}

// credentialPathFragments name the files whose contents are credentials. The bash tool refuses to read
// them; the read and edit tools apply the same list to the path they are given, absolute or not
// (2026-09-29 audit, round 127, F127-L1-4).
// containsCredentialFragment: the fragment names a credential file unless it is the start of a `.pub` name — the
// public half of a key pair is meant to be read and copied (2026-09-29 audit, round 131, F131-L1-5).
func containsCredentialFragment(text, fragment string) bool {
	for from := 0; ; {
		i := strings.Index(text[from:], fragment)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(fragment)
		after := text[end:]
		// The product's own stores live in the HOME directory: a project's own `.oaica/api_key_docs.md` or
		// `.oaica/remotes.json.example` is a project file, not this user's key (round 132, F132-L1-1).
		if homeScopedFragment(fragment) {
			if !homeAnchored(text[:i]) || (!strings.HasPrefix(fragment, "/.oaica/remotes.json") && len(after) > 0 && isNameByte(after[0])) {
				from = end
				continue
			}
			return true
		}
		from = i
		if !strings.HasSuffix(fragment, "/") && strings.HasPrefix(after, ".pub") {
			if rest := after[len(".pub"):]; rest == "" || !isNameByte(rest[0]) {
				from = end
				continue
			}
		}
		return true
	}
}

var (
	ansiCQuoteRe = regexp.MustCompile(`\$'([^']*)'`)
	braceVarRe   = regexp.MustCompile(`\$\{[a-z_][a-z0-9_]*\}`)
	ansiEscapeRe = regexp.MustCompile(`\\(x[0-9a-fA-F]{1,2}|[0-7]{1,3}|.)`)
)

// shellDecodedText is the third spelling the shell reads: `$'id_\x72sa'` and `$"id_rsa"` are id_rsa, and an unset
// `${x}` is nothing (round 132, F132-L1-3).
func shellDecodedText(command string) string {
	c := strings.ToLower(command)
	c = ansiCQuoteRe.ReplaceAllStringFunc(c, func(m string) string {
		inner := m[2 : len(m)-1]
		return ansiEscapeRe.ReplaceAllStringFunc(inner, func(e string) string {
			switch {
			case e[1] == 'x' && len(e) > 2:
				if n, err := strconv.ParseUint(e[2:], 16, 8); err == nil {
					return string(rune(n))
				}
			case e[1] >= '0' && e[1] <= '7':
				if n, err := strconv.ParseUint(e[1:], 8, 16); err == nil {
					return string(rune(n))
				}
			}
			return e[1:]
		})
	})
	c = strings.ReplaceAll(c, `$"`, `"`)
	c = braceVarRe.ReplaceAllStringFunc(c, func(m string) string {
		if m == "${home}" {
			return m
		}
		return ""
	})
	return shellSafetyText(c)
}

func homeScopedFragment(fragment string) bool {
	return strings.HasPrefix(fragment, "/.oaica/") || strings.HasPrefix(fragment, "/.ollama/")
}

// homeAnchored: the text in front of a home-scoped fragment names the user's home (`~`, `$HOME`, the resolved
// directory), not a project directory.
func homeAnchored(before string) bool {
	if i := strings.LastIndexAny(before, " \t\n"); i >= 0 {
		before = before[i+1:]
	}
	switch before {
	case "~", "$home", "${home}", "$env:userprofile", "%userprofile%":
		return true
	}
	if home, err := os.UserHomeDir(); err == nil {
		h := strings.ToLower(filepath.ToSlash(home))
		return h != "" && strings.HasSuffix(before, h)
	}
	return false
}

func isNameByte(b byte) bool {
	return b == '.' || b == '_' || b == '-' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

var credentialPathFragments = []string{
	"/.oaica/local_servers.json",
	"/.ollama/id_ed25519",
	"/.oaica/api_key",
	"/.oaica/license_key",
	"/.oaica/license.json",
	"/.oaica/auth.json",
	"/.oaica/remotes.json",
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
			if containsCredentialFragment(c, fragment) || strings.HasSuffix(c, strings.TrimSuffix(fragment, "/")) {
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
