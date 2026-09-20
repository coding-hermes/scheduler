package scheduler

import (
	"context"
	"log"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// ADV-R13 — host load/memory telemetry, MEASUREMENT ONLY.
//
// The fleet previously persisted ZERO load telemetry: the only readers were
// the admission gate (currentLoad1m, a live unix.Sysinfo read at admission
// time, never stored) and the dashboard's own runtime.MemStats render. With
// nothing recorded, any future 1/5/15-minute load threshold would have been
// fabricated on zero measurements. This row is the measurement foundation
// ONLY: one sample persisted per evaluation cycle, queryable afterwards.
//
// HARD SCOPE BOUNDARY — no admission-path consumer may ever read these
// samples. Admission/spawn/packer code is unchanged (load_gate.go,
// load_gate_linux.go, load_gate_other.go, slot_pool.go, admission_mode.go,
// spawner*.go, packer*.go); a sample failure never aborts or alters an
// evaluation pass. Reviving the loadavg question requires neighbor
// attribution analysis, an escalation-amplification answer, a fail-open
// ruling and owner re-ratification — all later rows.

// HostLoadSample is one host load/memory reading. Values are platform units
// as read (load averages unitless; memory in bytes); Source records where
// the reading came from (e.g. "proc") or "" when the platform provided
// nothing.
type HostLoadSample struct {
	Load1             float64
	Load5             float64
	Load15            float64
	MemTotalBytes     int64
	MemAvailableBytes int64
	Source            string
}

// recordHostSample samples the host once and persists the reading. It is the
// ONLY consumer of the sampleHostLoad var and the ONLY writer of the
// host_samples table.
//
// Contract (ADV-R13):
//   - MEASUREMENT ONLY: nothing here reads or influences admission state;
//     evaluation behavior is byte-identical with and without a working
//     sampler.
//   - A sampler failure or a persist failure is swallowed into the log —
//     a missing reading is a reported gap, never a fabricated zero row,
//     and never an evaluation abort.
//
// The timestamp comes from the loop's clock seam (SCHED-GAP-169), like every
// other persisted timestamp in the daemon, so tests can pin it.
func (l *Loop) recordHostSample(now time.Time) {
	if sampleHostLoad == nil {
		return
	}
	sample, ok := sampleHostLoad()
	if !ok {
		log.Println("TELEMETRY: host sample unavailable this cycle (gap recorded, evaluation unaffected)")
		return
	}
	ctx := context.Background()
	if err := database.InsertHostSample(ctx, l.db, &database.HostSample{
		SampledAt:         now.UTC().Format(time.RFC3339),
		Load1:             sample.Load1,
		Load5:             sample.Load5,
		Load15:            sample.Load15,
		MemTotalBytes:     sample.MemTotalBytes,
		MemAvailableBytes: sample.MemAvailableBytes,
		Source:            sample.Source,
	}); err != nil {
		log.Printf("TELEMETRY: host sample persist failed (evaluation unaffected): %v", err)
	}
}
