package scheduler

import (
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

const receiptHelperEnv = "SCHEDULER_RECEIPT_HELPER"

// receiptHelperTimeout bounds the optional local printer sidecar. Tests may
// shorten it to exercise cancellation without waiting for the production cap.
var receiptHelperTimeout = 15 * time.Second

// receiptHelperExec is the narrow process seam; production always delegates
// to exec.CommandContext and passes the tick ID as a single literal argument.
var receiptHelperExec = func(ctx context.Context, helper string, args ...string) error {
	return exec.CommandContext(ctx, helper, args...).Run()
}

// printTickReceipt invokes the optional local receipt helper for a finalized
// tick. It is intentionally fire-and-forget from the scheduler's perspective:
// a missing setting disables printing, and failures/timeouts are logged but
// cannot alter the already-persisted tick or Telegram delivery.
func printTickReceipt(tickID string) {
	helper := strings.TrimSpace(os.Getenv(receiptHelperEnv))
	if helper == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), receiptHelperTimeout)
	defer cancel()

	if err := receiptHelperExec(ctx, helper, "--print-tick", tickID); err != nil {
		if ctx.Err() != nil {
			log.Printf("WARN: local receipt helper timed out tick=%s after %s", tickID, receiptHelperTimeout)
			return
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			log.Printf("WARN: local receipt helper exited unsuccessfully tick=%s exit_code=%d", tickID, exitErr.ExitCode())
			return
		}
		log.Printf("WARN: local receipt helper could not run tick=%s error_type=%T", tickID, err)
	}
}
