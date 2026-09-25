//go:build schedgap1589probe

// SCHED-GAP-1589 one-shot render probe: renders /queue against a DB path
// passed as os.Args[1] and prints the HTML to stdout. Build with
//   go build -tags schedgap1589probe -o /tmp/queueprobe ./cmd/queueprobe/
// Proves acceptance 3 against real DB state (a COPY of the live file — the
// live DB is never opened) without running the daemon.

package main

import (
	"fmt"
	"os"
	"time"

	"github.com/coding-hermes/scheduler/internal/dashboard"
	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

func main() {
	db, err := database.InitDB(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer db.Close()
	calc := scheduler.NewUrgencyCalculator(30*time.Second, 24*time.Hour, 10)
	g := dashboard.NewGenerator(db, calc)
	var sb stringsBuilder
	if err := g.GenerateQueue(&sb); err != nil {
		panic(err)
	}
	fmt.Print(sb.s)
}

type stringsBuilder struct{ s string }

func (b *stringsBuilder) Write(p []byte) (int, error) {
	b.s += string(p)
	return len(p), nil
}
