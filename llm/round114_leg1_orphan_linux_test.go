package llm

// round114_leg1_orphan_linux_test.go — leg 1, round 114 (2026-09-29 audit), F114-L1-2.
// RECORDED, not changed. Linux-only (the _linux suffix is the build constraint).
//
// A runner is started with LlamaServerSysProcAttr = &syscall.SysProcAttr{}: nothing ties its life to the
// server's. When the server dies without running unloadAllRunners (SIGKILL, an OOM kill, a panic in a
// producer goroutine that gin's Recovery does not cover, a systemd TimeoutStopSec kill) each llama-server
// child is reparented to PID 1 and keeps its model resident in VRAM and RAM, and its port, until it
// happens to write a log line to the dead pipe; the restarted server has no record of it and loads a
// second copy, so orphans accumulate across crashes. Probed with a helper child killed with its parent.
//
// Recorded rather than changed: Pdeathsig fires when the THREAD that forked the child exits, not the
// process, so setting it without runtime.LockOSThread around the spawn kills runners at random when
// the Go runtime retires a thread, which is worse than the orphan; the alternative, a pidfile per
// runner reaped at Serve() startup, is a design of its own. The probe used sleep for the child, and a
// real llama-server may die on SIGPIPE at its next log write, though an idle loaded model logs
// nothing. The pin states today's reading and goes red when either mechanism lands.

import "testing"

func TestMine114ARunnerIsNotTiedToTheServersLife(t *testing.T) {
	if LlamaServerSysProcAttr == nil {
		t.Skip("no SysProcAttr on this platform")
	}
	if LlamaServerSysProcAttr.Pdeathsig != 0 {
		t.Errorf("Pdeathsig is %v: a runner is now tied to its parent, so this record is spent — check that the spawn is thread-locked, since Pdeathsig fires when the creating THREAD exits (2026-09-29 audit, round 114, F114-L1-2)", LlamaServerSysProcAttr.Pdeathsig)
	}
}
