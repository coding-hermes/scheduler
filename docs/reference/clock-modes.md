# Clock implementations and time env vars

Three implementations:

| Clock | Use |
|-------|-----|
| `clock.Real()` | production; stdlib delegation, identical to the calls it replaced (the default) |
| `clock.NewFixed(t)` | the ADV-R04/G6 determinism seam: frozen decision instant, real waits |
| `clock.NewSimClock(scale)` | the test-time simulator |

| Env var | Values | Meaning |
|---------|--------|---------|
| `SCHEDULER_TIME_MODE` | `real` (default) \| `sim` | selects the implementation |
| `SCHEDULER_TIME_SCALE` | positive float, default `1.0` | simulator speed: a blocked wait costs `d/scale` of REAL time while virtual now advances the full `d` (10x/100x/1000x) |
| `SCHEDULER_TIME_START` | RFC3339 | simulator start instant (default: now) |
| `SCHEDULER_TIME_AUTOADVANCE` | `0` (default) \| `1` | skip straight to the next armed timer at zero real cost |
