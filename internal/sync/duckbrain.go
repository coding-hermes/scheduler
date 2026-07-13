package sync

import (
	"context"
	"database/sql"
	"log"
	"time"
)

// DuckBrainSync pushes fleet state to DuckBrain as a read replica.
// TODO(GAP-004): Replace os/exec with MCP HTTP client.
type DuckBrainSync struct {
	db        *sql.DB
	namespace string
	interval  time.Duration
}

// NewDuckBrainSync creates a DuckBrain syncer.
func NewDuckBrainSync(db *sql.DB, namespace string) *DuckBrainSync {
	return &DuckBrainSync{
		db:        db,
		namespace: namespace,
		interval:  5 * time.Minute,
	}
}

// Run starts the periodic sync loop. Blocks until ctx is cancelled.
func (d *DuckBrainSync) Run(ctx context.Context) {
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	log.Printf("SYNC: DuckBrain sync — paused (GAP-004: MCP HTTP client needed)")

	for {
		select {
		case <-ctx.Done():
			log.Println("SYNC: stopping")
			return
		case <-ticker.C:
			// No-op until MCP HTTP client is implemented.
		}
	}
}
