package agent

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Public Load and its raw read are measured separately: provenance may load
// children and persist real revocations, while raw reads must preserve bytes.
// Only disposable server-side incident copies may be supplied to this test.
func TestRefsDeepIncidentRead(t *testing.T) {
	for _, mode := range []string{"raw", "verified"} {
		t.Run(mode, func(t *testing.T) {
			root := os.Getenv("TRACE_INCIDENT_FIXTURE_DIR")
			if root == "" {
				t.Skip("requires isolated complete incident fixture")
			}
			root, err := filepath.Abs(root)
			if err != nil || !strings.HasPrefix(filepath.ToSlash(root), "/home/ubuntu/trace/validation/") {
				t.Fatal("requires disposable server validation directory")
			}
			store, err := NewStore(root)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			const id = "21747a3b09678fd8d7e854dc82969267"
			started := time.Now()
			var snap Snapshot
			if mode == "raw" {
				snap, err = store.loadRawLocked(id)
			} else {
				snap, err = store.Load(id)
			}
			elapsed := time.Since(started)
			if err != nil {
				t.Fatal(err)
			}
			if snap.Run.ID != id || len(snap.Requests) != 422 || len(snap.Messages) != 644 || len(snap.Events) < 21165 {
				t.Fatal("lost complete incident identity or historical records")
			}
			// Hash without buffering the complete snapshot. The raw read hash is
			// an exact semantic oracle across binaries on the same fixture.
			hash := sha256.New()
			if err = encodeSnapshot(hash, snap); err != nil {
				t.Fatal(err)
			}
			t.Logf("mode=%s read_ms=%.2f requests=%d messages=%d events=%d event_seq=%d snapshot_sha256=%s", mode, float64(elapsed.Microseconds())/1000, len(snap.Requests), len(snap.Messages), len(snap.Events), snap.Run.EventSeq, fmt.Sprintf("%x", hash.Sum(nil)))
		})
	}
}
