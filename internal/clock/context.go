package clock

import "context"

// ctxKey is the private context key used to carry a Clock.
type ctxKey struct{}

// WithClock returns a context carrying c. It is the injection form for FREE
// FUNCTIONS that already thread a context and own no component to hang a Clock
// field on (the sql-backed DB helpers, manifest ingestors). A nil clock is
// ignored, so WithClock(ctx, nil) can never silently move a caller onto a
// simulated clock.
func WithClock(ctx context.Context, c Clock) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, c)
}

// FromContext returns the Clock carried by ctx, or the wall clock when none
// was installed. It never returns nil, so callers can read time without a
// nil check.
func FromContext(ctx context.Context) Clock {
	if ctx != nil {
		if c, ok := ctx.Value(ctxKey{}).(Clock); ok && c != nil {
			return c
		}
	}
	return Real()
}
