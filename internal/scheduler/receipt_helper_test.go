package scheduler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

func TestSCHEDGAP16970_ReceiptHelperSettingAndArguments(t *testing.T) {
	oldExec := receiptHelperExec
	oldTimeout := receiptHelperTimeout
	t.Cleanup(func() {
		receiptHelperExec = oldExec
		receiptHelperTimeout = oldTimeout
	})

	var calls [][]string
	var sawDeadline bool
	receiptHelperTimeout = time.Second
	receiptHelperExec = func(ctx context.Context, helper string, args ...string) error {
		calls = append(calls, append([]string{helper}, args...))
		_, sawDeadline = ctx.Deadline()
		return nil
	}

	t.Setenv(receiptHelperEnv, "")
	if err := os.Unsetenv(receiptHelperEnv); err != nil {
		t.Fatalf("unset helper setting: %v", err)
	}
	printTickReceipt("tick-unset")
	t.Setenv(receiptHelperEnv, "   ")
	printTickReceipt("tick-empty")
	if len(calls) != 0 {
		t.Fatalf("helper called while unset/empty: %v", calls)
	}

	t.Setenv(receiptHelperEnv, "/local/receipt-helper")
	printTickReceipt("exact-tick-id-17")
	want := [][]string{{"/local/receipt-helper", "--print-tick", "exact-tick-id-17"}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v, want %#v", calls, want)
	}
	if !sawDeadline {
		t.Fatal("helper context has no hard deadline")
	}
}

func TestSCHEDGAP16970_ReceiptHelperMalformedSettingIsOneLiteralPath(t *testing.T) {
	oldExec := receiptHelperExec
	t.Cleanup(func() { receiptHelperExec = oldExec })

	var gotHelper string
	var gotArgs []string
	receiptHelperExec = func(_ context.Context, helper string, args ...string) error {
		gotHelper = helper
		gotArgs = append([]string(nil), args...)
		return errors.New("not found")
	}

	// A setting containing spaces is not parsed as shell or argv syntax.
	t.Setenv(receiptHelperEnv, "/tmp/helper --unexpected")
	printTickReceipt("tick-literal")
	if gotHelper != "/tmp/helper --unexpected" {
		t.Fatalf("helper path = %q, want the literal setting", gotHelper)
	}
	if !reflect.DeepEqual(gotArgs, []string{"--print-tick", "tick-literal"}) {
		t.Fatalf("args = %#v", gotArgs)
	}
}

func TestSCHEDGAP16970_ReceiptHelperFailureAndTimeoutAreBestEffort(t *testing.T) {
	t.Run("failure does not change link send", func(t *testing.T) {
		oldExec := receiptHelperExec
		t.Cleanup(func() { receiptHelperExec = oldExec })
		receiptHelperExec = func(context.Context, string, ...string) error {
			return errors.New("printer process failed")
		}

		capture, _ := setupFakeHermesB64(t)
		resetPublicBaseURL(t, "https://scheduler.example")
		deliverOutputWithMode(clock.NewManualSimClock(time.Unix(0, 0)), "receipt-test", "tick-link-16970", "telegram:-100123:7", "prompt", reportBuffer(), DeliverModeLink)
		// The helper runs strictly after this same delivery call at the
		// completion path. Its failure has no return channel to alter it.
		oldTimeout := receiptHelperTimeout
		receiptHelperTimeout = time.Second
		t.Cleanup(func() { receiptHelperTimeout = oldTimeout })
		printTickReceipt("tick-link-16970")

		args := captureArgs(t, capture)
		if got := findArg(args, "--to"); got != "telegram:-100123:7" {
			t.Fatalf("delivery target = %q", got)
		}
		wantMessage := "🤖 receipt-test [tick-link-16970] · prompt\n\n_tick-link-16970 · prompt_\n\nReport: https://scheduler.example/ticks/tick-link-16970"
		if got := args[len(args)-1]; got != wantMessage {
			t.Fatalf("link message changed:\n got: %q\nwant: %q", got, wantMessage)
		}
	})

	t.Run("deadline cancels helper", func(t *testing.T) {
		oldExec := receiptHelperExec
		oldTimeout := receiptHelperTimeout
		t.Cleanup(func() {
			receiptHelperExec = oldExec
			receiptHelperTimeout = oldTimeout
		})
		receiptHelperTimeout = 20 * time.Millisecond
		finished := false
		receiptHelperExec = func(ctx context.Context, _ string, _ ...string) error {
			<-ctx.Done()
			finished = true
			return ctx.Err()
		}
		t.Setenv(receiptHelperEnv, "/unused/fake-helper")
		printTickReceipt("tick-timeout")
		if !finished {
			t.Fatal("helper runner was not cancelled at deadline")
		}
	})
}

func TestSCHEDGAP16970_ReceiptHelperUsesCommandContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell helper is Unix-only")
	}
	dir := t.TempDir()
	helper := filepath.Join(dir, "fake-helper")
	capture := filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$#:$1:$2\" > \"$RECEIPT_CAPTURE\"\n"
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RECEIPT_CAPTURE", capture)
	t.Setenv(receiptHelperEnv, helper)

	oldExec := receiptHelperExec
	oldTimeout := receiptHelperTimeout
	receiptHelperExec = func(ctx context.Context, helper string, args ...string) error {
		return oldExec(ctx, helper, args...)
	}
	receiptHelperTimeout = time.Second
	t.Cleanup(func() {
		receiptHelperExec = oldExec
		receiptHelperTimeout = oldTimeout
	})
	printTickReceipt("tick-command-context")
	got, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read fake helper capture: %v", err)
	}
	if string(got) != "2:--print-tick:tick-command-context\n" {
		t.Fatalf("helper argv capture = %q", got)
	}
}

func TestSCHEDGAP16970_ReceiptHelperCommandTimeout(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "slow-helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nexec sleep 5\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(receiptHelperEnv, helper)
	oldTimeout := receiptHelperTimeout
	receiptHelperTimeout = 25 * time.Millisecond
	t.Cleanup(func() { receiptHelperTimeout = oldTimeout })

	start := time.Now()
	printTickReceipt("tick-command-timeout")
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("helper timeout took %s, expected prompt CommandContext cancellation", elapsed)
	}
}
