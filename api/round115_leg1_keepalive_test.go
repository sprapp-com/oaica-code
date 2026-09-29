package api

import (
	"encoding/json"
	"testing"
)

func TestRound115KeepAliveOverflow(t *testing.T) {
	for _, body := range []string{`-1`, `"-1s"`, `3600`, `9.2e9`, `9.3e9`, `1e10`, `1e300`, `"2562047h"`} {
		var r ChatRequest
		err := json.Unmarshal([]byte(`{"model":"m","keep_alive":`+body+`}`), &r)
		if err != nil {
			t.Logf("keep_alive=%-11s -> error %v", body, err)
			continue
		}
		verdict := "kept loaded"
		if r.KeepAlive.Duration <= 0 {
			verdict = "UNLOADED at once (sched.go:386 sessionDuration <= 0)"
		}
		t.Logf("keep_alive=%-11s -> Duration=%d  %s", body, int64(r.KeepAlive.Duration), verdict)
	}
	var r ChatRequest
	json.Unmarshal([]byte(`{"model":"m","keep_alive":1e10}`), &r)
	if r.KeepAlive.Duration <= 0 {
		t.Fatalf("RED: keep_alive=1e10 (317 years) reads as %v", r.KeepAlive.Duration)
	}
}
