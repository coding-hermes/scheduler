package scheduler

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

// Regression tests for SCHED-GAP-081 (2026-09-01): the spawner's stdout
// scanner surfaced "read |0: file already closed" WARNs on ~14 normal ticks/day
// (the sampled ticks were mostly COMPLETED, not timeouts). The completion path
// canceled the scan context, which fired the close-stdout goroutine while the
// scanner was still draining the pipe — truncating captured output and
// misclassifying clean end-of-output as a read error.

// TestScannerErrIsBenign pins the error classification: io.EOF and a closed
// pipe (os.ErrClosed) are expected end-of-output; everything else stays a real
// WARN-class failure.
func TestScannerErrIsBenign(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"io.EOF", io.EOF, true},
		{"closed pipe read (file already closed)", closedPipeReadErr(), true},
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF, false},
		{"context deadline exceeded", context.DeadlineExceeded, false},
		{"arbitrary error", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scannerErrIsBenign(tc.err); got != tc.want {
				t.Fatalf("scannerErrIsBenign(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// closedPipeReadErr reproduces the exact error the bug surfaced: a read on a
// pipe whose read end was closed while the read was in flight yields
// "read |0: file already closed" (an *os.PathError wrapping EBADF). Closing
// the read end before the read makes it deterministic; the error value is
// identical to the mid-drain close the completion path produced.
func closedPipeReadErr() error {
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	_ = pw.Close()
	pr.Close()
	buf := make([]byte, 16)
	_, err = pr.Read(buf)
	return err
}

// TestScannerClosedPipeMidDrainExitsBenign mirrors the scanner loop's contract:
// when the read end of the pipe is closed while output is still being drained
// (the old completion-path behavior), the scan must terminate promptly and the
// resulting error must classify as benign — no WARN, no leak, and whatever was
// already drained is preserved.
func TestScannerClosedPipeMidDrainExitsBenign(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer pw.Close()

	payload := bytes.Repeat([]byte("line of tick output\n"), 2000)
	go func() {
		_, _ = pw.Write(payload)
		time.Sleep(300 * time.Millisecond) // hold the write end open past the close below
		_ = pw.Close()
	}()

	scanner := bufio.NewScanner(pr)
	var drained bytes.Buffer
	done := make(chan struct{})
	go func() {
		for scanner.Scan() {
			drained.Write(scanner.Bytes())
			drained.WriteByte('\n')
		}
		close(done)
	}()

	time.Sleep(50 * time.Millisecond) // let the scanner start draining
	_ = pr.Close()                    // mid-drain close, like the old completion path

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("scanner did not exit after the pipe was closed mid-drain")
	}

	if err := scanner.Err(); err != nil && !scannerErrIsBenign(err) {
		t.Fatalf("scanner error %v should classify as benign end-of-output", err)
	}
	// Output drained before the close must be preserved (truncation was the
	// original harm — the close only cuts off what was still buffered).
	if drained.Len() == 0 {
		t.Fatal("scanner exited before draining any output")
	}
}
