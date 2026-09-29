package launch

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

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
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case err := <-done:
			return err
		case sig := <-sigs:
			if sig == syscall.SIGINT {
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
