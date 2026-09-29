package launch

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/term"
)

// runChildStdinIsTerminal reports whether the child shares our terminal, in which case Ctrl-C
// reaches it through the foreground process group without our help. A variable so a test can say
// either way.
var runChildStdinIsTerminal = func(cmd *exec.Cmd) bool {
	f, ok := cmd.Stdin.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// childTermGrace is how long a child gets, after being sent SIGTERM, before it is killed.
var childTermGrace = 10 * time.Second

// runChild starts cmd and waits for it without orphaning it. The launch doors ran their
// child with cmd.Run() and installed no signal handler, so a SIGTERM to oaica alone (a
// supervisor stop, `kill <pid>`, an ssh wrapper) ended oaica at once by Go's default and left
// the child running against a translation proxy that had died with its parent, on the same
// tty and session; cmd/oaica_pull_serve.go already handled this properly for pull and serve
// (2026-09-29 audit, round 113, F113-L2-2).
//
// SIGTERM and SIGHUP are forwarded to the child and the child is waited for; one that ignores
// the signal is killed after childTermGrace. SIGINT is only absorbed: Ctrl-C reaches the child
// through the foreground process group, and oaica must not exit before it. The child's own
// exit status is what the caller sees.
func runChild(cmd *exec.Cmd) error {
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	if err := cmd.Start(); err != nil {
		return err
	}
	interactive := runChildStdinIsTerminal(cmd)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case err := <-done:
			return err
		case sig := <-sigs:
			// SIGINT is absorbed only when a terminal delivered it: Ctrl-C already reached the
			// child through the foreground process group, and a second delivery would be a
			// double Ctrl-C, which Claude Code reads as "exit". With no terminal (a supervisor,
			// `kill -INT <pid>`) nothing else will deliver it, and absorbing it left the launcher
			// and its child running until something else stopped them (2026-09-29 audit, round
			// 114, F114-L2-3).
			if sig == syscall.SIGINT && interactive {
				continue
			}
			if err := cmd.Process.Signal(sig); err != nil {
				_ = cmd.Process.Kill() // a platform that cannot deliver the signal
			}
			select {
			case err := <-done:
				return err
			case <-time.After(childTermGrace):
				_ = cmd.Process.Kill()
				return <-done
			}
		}
	}
}
