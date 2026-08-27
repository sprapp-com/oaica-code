package cmd

// gpu_clean.go — `oaica gpu ps` / `oaica gpu clean`: diagnostic and cleanup
// for orphaned GPU-memory-holding processes, specifically vLLM's
// VLLM::EngineCore child.
//
// Why this exists: proven live during the 2026-08-27/28 production-
// readiness stress test on prism-a100b. Killing vLLM's api_server PID does
// NOT release VRAM — vLLM's V1 engine runs the actual model in a SEPARATE
// process (visible in `ps` as "VLLM::EngineCore") that gets reparented to
// PID 1 when its parent dies, rather than dying with it. `nvidia-smi
// --query-compute-apps` can also report a stale/wrong PID for the same
// GPU memory — `fuser -v /dev/nvidiaN` is the reliable source (it queries
// the driver's actual open-file-descriptor table, not a cached process
// list). This tool automates what took repeated manual fuser/kill cycles
// during that session.
//
// Deliberately conservative: `gpu clean` only ever kills a process whose
// PPID is 1 (reparented — its original parent is provably gone) AND whose
// command name matches a known worker-process pattern (VLLM::EngineCore
// today; add more as other engines show the same behavior). It never
// touches a process that still has a live parent, even if that parent
// looks unfamiliar — a process with a live parent might be someone else's
// actively-managed job on a shared box, and this tool has no way to know
// whose. `gpu ps` (no --kill) is always safe to run: read-only.

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// orphanWorkerPatterns are command names known to survive their parent's
// death while still holding GPU memory. Matched via strings.Contains
// against the full ps command line, not the argv[0] basename, since these
// vLLM's own multiprocessing naming shows up as the whole "cmd" field
// (e.g. "VLLM::EngineCore").
var orphanWorkerPatterns = []string{
	"VLLM::EngineCore",
}

type gpuHolder struct {
	PID     int
	PPID    int
	Cmd     string
	Orphan  bool
	// MemMiB is best-effort from nvidia-smi --query-compute-apps; 0 when
	// nvidia-smi didn't report this PID (fuser found it, nvidia-smi's
	// cached list didn't — itself a symptom of the same staleness this
	// tool exists to work around).
	MemMiB int
}

// listGPUHolders cross-references fuser's live driver-fd view (source of
// truth for "is this process actually touching a GPU right now") with
// nvidia-smi's compute-apps list (best-effort memory figures, can be
// stale). Every device node under /dev/nvidia[0-9]+ is queried since a
// process can hold memory on any of them.
func listGPUHolders() ([]gpuHolder, error) {
	devices, err := findNvidiaDevices()
	if err != nil {
		return nil, err
	}
	if len(devices) == 0 {
		return nil, fmt.Errorf("no /dev/nvidia* device nodes found — is this a GPU box?")
	}

	pids := map[int]bool{}
	for _, dev := range devices {
		out, err := exec.Command("fuser", "-v", dev).CombinedOutput()
		if err != nil {
			// fuser exits non-zero when it finds nothing on some
			// systems — only a hard error (binary missing) is fatal.
			if _, lookErr := exec.LookPath("fuser"); lookErr != nil {
				return nil, fmt.Errorf("fuser not found on PATH — required for accurate GPU-holder detection (nvidia-smi's own list can be stale)")
			}
		}
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			// fuser -v format: "USER    PID ACCESS COMMAND" (header) or
			// "/dev/nvidia0:  root  1234 F.... python3" (data rows, device
			// name only on the FIRST row of a device's block).
			pidField := fields[len(fields)-3]
			if len(fields) == 3 {
				// header row or malformed — skip
				continue
			}
			pid, err := strconv.Atoi(pidField)
			if err == nil && pid > 0 {
				pids[pid] = true
			}
		}
	}

	memByPID := nvidiaComputeAppsMemory()

	holders := make([]gpuHolder, 0, len(pids))
	for pid := range pids {
		ppid, cmdLine, ok := procInfo(pid)
		if !ok {
			continue // process exited between fuser's snapshot and now
		}
		h := gpuHolder{PID: pid, PPID: ppid, Cmd: cmdLine, MemMiB: memByPID[pid]}
		h.Orphan = ppid == 1 && matchesOrphanPattern(cmdLine)
		holders = append(holders, h)
	}
	return holders, nil
}

func matchesOrphanPattern(cmdLine string) bool {
	for _, p := range orphanWorkerPatterns {
		if strings.Contains(cmdLine, p) {
			return true
		}
	}
	return false
}

func findNvidiaDevices() ([]string, error) {
	entries, err := os.ReadDir("/dev")
	if err != nil {
		return nil, err
	}
	var devices []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "nvidia") && len(name) > len("nvidia") {
			// nvidia0, nvidia1, ... — exclude nvidiactl, nvidia-uvm, etc.
			rest := name[len("nvidia"):]
			if _, err := strconv.Atoi(rest); err == nil {
				devices = append(devices, "/dev/"+name)
			}
		}
	}
	return devices, nil
}

// nvidiaComputeAppsMemory is best-effort — errors just mean an empty map,
// so gpu ps/clean still work off fuser alone (memory column shows 0/blank).
func nvidiaComputeAppsMemory() map[int]int {
	out, err := exec.Command("nvidia-smi", "--query-compute-apps=pid,used_memory", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return map[int]int{}
	}
	m := map[int]int{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) != 2 {
			continue
		}
		pid, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		mem, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err1 == nil && err2 == nil {
			m[pid] = mem
		}
	}
	return m
}

// procInfo reads PPID and the full command line for pid from /proc.
// Returns ok=false if the process has already exited.
func procInfo(pid int) (ppid int, cmdLine string, ok bool) {
	statData, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, "", false
	}
	// /proc/PID/stat: "PID (COMM) STATE PPID ...". COMM can contain
	// spaces/parens, so split on the LAST ')' to find where the fixed
	// fields resume.
	stat := string(statData)
	closeParen := strings.LastIndex(stat, ")")
	if closeParen == -1 || closeParen+2 >= len(stat) {
		return 0, "", false
	}
	fields := strings.Fields(stat[closeParen+2:])
	if len(fields) < 2 {
		return 0, "", false
	}
	ppid, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, "", false
	}

	cmdData, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return 0, "", false
	}
	cmdLine = strings.TrimSpace(strings.ReplaceAll(string(cmdData), "\x00", " "))
	if cmdLine == "" {
		// Kernel threads / some worker processes have an empty cmdline;
		// fall back to comm from /proc/PID/stat's parenthesized field.
		openParen := strings.Index(stat, "(")
		if openParen != -1 && closeParen > openParen {
			cmdLine = stat[openParen+1 : closeParen]
		}
	}
	return ppid, cmdLine, true
}

func gpuCleanCmd() *cobra.Command {
	gpuCmd := &cobra.Command{
		Use:   "gpu",
		Short: "Inspect and clean up local GPU-memory-holding processes",
	}

	psCmd := &cobra.Command{
		Use:   "ps",
		Short: "List processes currently holding GPU memory (read-only)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			holders, err := listGPUHolders()
			if err != nil {
				return err
			}
			if len(holders) == 0 {
				fmt.Println("No processes currently hold GPU memory.")
				return nil
			}
			fmt.Printf("%-8s %-8s %-10s %-8s %s\n", "PID", "PPID", "MEM(MiB)", "ORPHAN", "CMD")
			for _, h := range holders {
				orphan := ""
				if h.Orphan {
					orphan = "YES"
				}
				cmdShown := h.Cmd
				if len(cmdShown) > 60 {
					cmdShown = cmdShown[:57] + "..."
				}
				fmt.Printf("%-8d %-8d %-10d %-8s %s\n", h.PID, h.PPID, h.MemMiB, orphan, cmdShown)
			}
			return nil
		},
	}

	var yes bool
	cleanCmd := &cobra.Command{
		Use:   "clean",
		Short: "Kill orphaned GPU worker processes (PPID=1, known worker pattern only)",
		Long: `Kills processes that are BOTH reparented to PID 1 (their original
parent has provably exited) AND match a known GPU-worker pattern
(currently: VLLM::EngineCore). This is exactly the case discovered during
the 2026-08-27/28 production-readiness stress test: killing vLLM's
api_server does not release the GPU memory its EngineCore child holds.

Never touches a process that still has a live parent — on a shared GPU
box that could be someone else's actively-managed job. Run 'oaica gpu ps'
first to see what would be affected.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			holders, err := listGPUHolders()
			if err != nil {
				return err
			}
			var orphans []gpuHolder
			for _, h := range holders {
				if h.Orphan {
					orphans = append(orphans, h)
				}
			}
			if len(orphans) == 0 {
				fmt.Println("No orphaned GPU worker processes found.")
				return nil
			}
			fmt.Printf("Found %d orphaned GPU worker process(es):\n", len(orphans))
			for _, h := range orphans {
				fmt.Printf("  PID %d (%d MiB): %s\n", h.PID, h.MemMiB, h.Cmd)
			}
			if !yes {
				fmt.Println("\nRe-run with --yes to kill these.")
				return nil
			}
			for _, h := range orphans {
				proc, err := os.FindProcess(h.PID)
				if err != nil {
					fmt.Printf("  PID %d: %v\n", h.PID, err)
					continue
				}
				if err := proc.Kill(); err != nil {
					fmt.Printf("  PID %d: kill failed: %v\n", h.PID, err)
					continue
				}
				fmt.Printf("  PID %d: killed\n", h.PID)
			}
			return nil
		},
	}
	cleanCmd.Flags().BoolVar(&yes, "yes", false, "Actually kill the orphaned processes (default: dry run, lists only)")

	gpuCmd.AddCommand(psCmd, cleanCmd)
	return gpuCmd
}
