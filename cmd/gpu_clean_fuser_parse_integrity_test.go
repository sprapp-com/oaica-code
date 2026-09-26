package cmd

// gpu_clean_fuser_parse_integrity_test.go — the fuser row parser read the PID
// as "three fields from the end" (2026-09-26 audit).
//
// fuser -v prints "USER PID ACCESS COMMAND", and COMMAND is the process's name
// as fuser sees it — for vLLM's worker that is "VLLM::EngineCore", but for
// anything with arguments in its comm it can contain a space. Counting
// backwards from the end then lands on a word of the command, Atoi fails, and
// the PID is silently dropped: `oaica gpu ps` reports no holder for a device
// that has one, which is the one thing this tool exists to get right (its
// whole premise is that nvidia-smi's list is stale and fuser's is not).
//
// The PID is the field BEFORE the access field, which is a fixed shape; the
// command comes after it and can be anything.

import "testing"

func TestFuserOutputYieldsThePIDsWhateverTheCommandLooksLike(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want []int
	}{
		{
			name: "the shape fuser really prints",
			out: "                     USER        PID ACCESS COMMAND\n" +
				"/dev/nvidia0:        root       1234 F...m python3\n",
			want: []int{1234},
		},
		{
			name: "a command with arguments in its name",
			out:  "/dev/nvidia0:        root       4321 F.... python3 -m vllm.entrypoints\n",
			want: []int{4321},
		},
		{
			name: "several holders, continuation rows without the device name",
			out: "/dev/nvidia1:        root       1111 F.... VLLM::EngineCore\n" +
				"                     alice      2222 F.... python3 -m vllm\n",
			want: []int{1111, 2222},
		},
		{
			name: "header and noise rows only",
			out: "                     USER        PID ACCESS COMMAND\n" +
				"\n" +
				"/dev/nvidia0:\n",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fuserPIDs(tc.out)
			if len(got) != len(tc.want) {
				t.Fatalf("fuserPIDs = %v, want %v\ninput:\n%s", got, tc.want, tc.out)
			}
			for _, want := range tc.want {
				found := false
				for _, g := range got {
					if g == want {
						found = true
					}
				}
				if !found {
					t.Errorf("PID %d was in fuser's output and is not in %v — a command containing a space used to shift the field offset and drop it, which is a GPU holder this tool then cannot report or clean\ninput:\n%s", want, got, tc.out)
				}
			}
		})
	}
}
