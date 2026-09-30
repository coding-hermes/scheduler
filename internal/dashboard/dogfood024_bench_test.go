package dashboard

import (
	"context"
	"os"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// DOGFOOD-024 baseline benchmarks against a production-shaped copy of the
// live scheduler DB (78k+ ticks). Set DOGFOOD_DB to a read-only copy of the
// live scheduler.db; without it these skip so `-short` CI stays hermetic.
func newDogfoodGen(b *testing.B) *Generator {
	b.Helper()
	path := os.Getenv("DOGFOOD_DB")
	if path == "" {
		b.Skip("DOGFOOD_DB not set")
	}
	db, err := database.InitDB(path)
	if err != nil {
		b.Fatalf("init db: %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })
	return NewGenerator(db, scheduler.NewUrgencyCalculator(30, 7200, 10))
}

func newDogfoodGenT(t *testing.T) *Generator {
	t.Helper()
	path := os.Getenv("DOGFOOD_DB")
	if path == "" {
		t.Skip("DOGFOOD_DB not set")
	}
	db, err := database.InitDB(path)
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewGenerator(db, scheduler.NewUrgencyCalculator(30, 7200, 10))
}

func BenchmarkDogfoodFleetRenderCold(b *testing.B) {
	g := newDogfoodGen(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		data := g.collect(ctx)
		if data.TotalProjects == 0 {
			b.Fatal("no projects")
		}
	}
}

// BenchmarkDogfoodFleetRenderWarm measures steady state: one throwaway render
// first populates the tickWork / GitReins / CI caches (the live daemon keeps
// these warm — it re-renders every 10s, and DOGFOOD-024's 24h tickWork TTL
// means the memo stays valid across renders), then times pure re-renders.
func BenchmarkDogfoodFleetRenderWarm(b *testing.B) {
	g := newDogfoodGen(b)
	ctx := context.Background()
	if data := g.collect(ctx); data.TotalProjects == 0 {
		b.Fatal("no projects")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		data := g.collect(ctx)
		if data.TotalProjects == 0 {
			b.Fatal("no projects")
		}
	}
}

func BenchmarkDogfoodFleetComponents(b *testing.B) {
	g := newDogfoodGen(b)
	ctx := context.Background()
	b.Run("batchCompletedSamples", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = g.batchCompletedSamples(ctx)
		}
	})
	b.Run("batchTickHealth", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = g.batchTickHealth(ctx)
		}
	})
	b.Run("fleetLearned", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = g.fleetLearned(ctx)
		}
	})
}
