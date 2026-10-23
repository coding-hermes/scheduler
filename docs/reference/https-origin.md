# HTTPS origin for the dashboard (secure context)

The dashboard's remote access path is a plain-HTTP tailnet forward. That origin is
**not a browser secure context**, and two capabilities are gated on being one:
`Notification` (OS/browser notifications) and `navigator.serviceWorker`. On the
tailnet origin the browser does not merely ask and get refused — it reports
`Notification.permission === "denied"` and exposes no `navigator.serviceWorker` at
all. It is not a permission an operator can grant, and no page-side code can fix
it. The origin has to change (SCHED-GAP-1600).

This page is the measured state, the two ways to get a trusted HTTPS origin, and
the retirement steps for the plain-HTTP forward.

## Measured state

Chrome 152 headless over CDP, same dashboard build, four origins of the same page:

| Origin (role) | `isSecureContext` | `Notification.permission` | `navigator.serviceWorker` |
|---|---|---|---|
| `http://127.0.0.1:9090` (daemon, loopback) | **true** | `default` (promptable) | present |
| `http://<tailnet-ip>:9090` (socat forward) | **false** | `denied` | **absent** |
| `http://<node>.<tailnet>.ts.net:9090` (socat, MagicDNS) | **false** | `denied` | **absent** |
| `https://<host>:9444` (self-signed) | `false` — "Privacy error" interstitial, `location.origin === "null"` | `denied` | **absent** |

Loopback is a secure context because browsers treat `127.0.0.1` as potentially
trustworthy regardless of scheme. That is why notifications can work for a
browser running **on the scheduler host** and can never work for a browser on any
other device through the plain-HTTP forward.

### Why self-signed is not a fix (measured, not assumed)

Bypassing the certificate warning on a self-signed origin produces a mixed state:

| Self-signed `https://` origin, cert error bypassed | Result |
|---|---|
| `isSecureContext` | **true** |
| `Notification.permission` | `default` — the prompt becomes requestable |
| `navigator.serviceWorker` | present |
| `serviceWorker.register('/sw.js')` | **FAILS** — `SecurityError: … An unknown error occurred when fetching the script` |

The same probe page and server over `http://127.0.0.1:<port>` registers the worker
successfully (`OK`), so the failure is attributable to the untrusted certificate,
not to the probe. A self-signed cert therefore buys notifications at the cost of a
per-device warning interstitial and still no service workers. Use a trusted
certificate.

## Path A — `tailscale serve` (preferred)

Terminates TLS inside `tailscaled` with a Tailscale-issued certificate, binds only
the tailnet interface, and needs no change to the daemon:

```bash
sudo tailscale serve --bg --https=443 http://127.0.0.1:9090
tailscale serve status          # expect https://<node>.<tailnet>.ts.net
```

**Blocked by one tailnet setting.** Measured on this host:

```
$ tailscale cert <node>.<tailnet>.ts.net
500 Internal Server Error: your Tailscale account does not support getting TLS certs
```

HTTPS certificates are a tailnet-level toggle. Until an admin enables them
(admin console → **DNS** → *HTTPS Certificates*, or the
`login.tailscale.com/f/serve?node=…` shortcut), `serve --https` cannot mint a
certificate and the command does not complete; it also writes nothing — the serve
config is left untouched, so a failed attempt is inert.

Once the toggle is on, Path A needs no rebuild and no daemon restart.

## Path B — the daemon's own TLS listener

`--tls-cert` / `--tls-key` serve the **same handler tree** (dashboard, `/api`,
`/mcp`, pprof) over TLS:

```bash
bin/schedulerd --listen 0.0.0.0:9090 \
  --tls-cert /path/fullchain.pem --tls-key /path/privkey.pem
# boot log then reads:
#   HTTPS: listening on 0.0.0.0:9090
#     Dashboard: https://0.0.0.0:9090/
#     Origin:    SECURE CONTEXT (TLS) — cert /path/fullchain.pem
```

Properties enforced by `cmd/schedulerd/tls_listen_test.go`:

- **All-or-nothing.** Either flag alone is a fatal boot error naming the missing
  half. It never falls back to cleartext: an operator who believes the origin is
  secure while it is not has exactly the defect these flags remove.
- **Empty pair = plain HTTP**, byte-identical to the previous behaviour, so
  existing deployments and every loopback consumer are untouched.
- **No auth change.** TLS is a transport swap in front of the same `pprofMux`
  handler: the operator-credential model, the read-open route split, and the
  fail-closed default are all unchanged (see `internal/api/auth.go`).

Certificate options for Path B:

| Certificate source | Trusted? | Notes |
|---|---|---|
| `tailscale cert <node>.<tailnet>.ts.net` | yes | Same tailnet toggle as Path A; renews on demand, cert lifetime ~90 days — schedule a renewal + reload |
| Public CA via **DNS-01** for a name whose A record points at the tailnet IP | yes | No inbound public exposure is needed and the name stays unreachable off-tailnet, but it requires DNS API credentials and an owner decision about the name |
| `openssl`/`mkcert` self-signed | **no** | Measured above: interstitial on every device, service workers still fail |

Prefer Path A when a certificate already exists: it keeps one moving part
(`tailscaled`) and needs no port juggling. Path B is the option when the
certificate comes from somewhere other than Tailscale (the DNS-01 case) or when a
TLS front must live in the daemon itself.

With TLS enabled, keep `--public-url` on the `https://` origin. A cleartext
`--public-url` while TLS is on logs a WARN at boot: tick-report permalinks would
otherwise send operators to a page whose notification affordances are dead.

## Retiring the plain-HTTP forward

Today's remote exposure is a systemd **user** unit that forwards the tailnet IP to
loopback in cleartext:

```bash
systemctl --user cat sched-ui-tailnet.service    # socat TCP-LISTEN:9090,bind=<tailnet-ip> -> 127.0.0.1:9090
```

Retire it in the **same change** that lands a working HTTPS origin, so exactly one
exposure path exists. `ops/enable-https-origin.sh` does this in the safe order and
refuses to proceed unless the replacement is proven:

```bash
ops/enable-https-origin.sh --cert /path/fullchain.pem --key /path/privkey.pem
ops/enable-https-origin.sh --tailscale            # Path A, once HTTPS certs are enabled
```

The script starts the HTTPS origin, probes it for `isSecureContext`-equivalent
evidence (a verified TLS handshake plus the dashboard answering over it), and only
then stops and disables `sched-ui-tailnet.service`. If the probe fails, the
cleartext forward is left running — cutting it while the replacement is unproven
would leave the dashboard with zero exposure paths, which is strictly worse than
the defect being fixed.

Do **not** retire the forward first "because the new path should work". Order
matters: provision, prove, then retire.

## Verification

Browser probe (DevTools console on the new origin, or the CDP harness):

```js
({
  origin: location.origin,
  isSecureContext: window.isSecureContext,                 // must be true
  notification: Notification.permission,                   // must be "default", never "denied"
  serviceWorker: typeof navigator.serviceWorker !== 'undefined' // must be true
});
```

A `serviceWorker.register()` check is worth running too — on an untrusted
certificate `isSecureContext` is `true` while registration still fails, so the
registration result is the assertion that separates a real fix from a warning-page
workaround.

Single-exposure check:

```bash
ss -ltn | grep -E ':9090'                       # one listener, not two
systemctl --user is-enabled sched-ui-tailnet.service   # expect: disabled (or not-found)
tailscale serve status                          # for Path A: exactly one handler for the dashboard
```

Reachability is unchanged by design: Path A listens on the tailnet interface
only, and Path B binds whatever `--listen` says. Neither adds an authentication
surface — the dashboard still has no login, so the exposure boundary stays
"whoever is on the tailnet", exactly as before.
