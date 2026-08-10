# Edge

The public surface is a **Cloudflare Tunnel**, and it is the only one.

## The property this buys

`cloudflared` holds an **outbound** connection to Cloudflare. Nothing in the
estate listens publicly — no ingress, no LoadBalancer, no inbound firewall rule
for 80/443. A port that is never open cannot be scanned, and the control plane
that moves capital stays dark.

## One origin

The tunnel points at **web-bff only**. The BFF serves the compiled SPA and
proxies `/api/*` to the api-gateway, so the browser sees one hostname.

That is not a convenience. The session is an httpOnly, `SameSite=Lax` cookie; a
second origin for the SPA would force `SameSite=None` — the setting that makes a
cookie usable in a cross-site context, and therefore the one CSRF defences exist
to prevent — plus CORS with credentials and a cookie domain kept in step with
whatever the edge is called this week.

The api-gateway has **no** ingress rule. A hostname reaching it directly would
bypass the BFF and the session with it, so a stolen bearer would work from
anywhere instead of only from a browser holding the cookie.

## Provisioning a node

Two variables and one command:

```sh
export TUNNEL_TOKEN=...          # from the Cloudflare dashboard
export BFF_INTERNAL_PORT=8084    # matches WEB_BFF_LISTEN
cloudflared tunnel run --token "$TUNNEL_TOKEN"
```

The token form needs no config file. `cloudflared-config.yml` is the declarative
alternative for a node that manages its own credentials; substitute `${TUNNEL_ID}`
and `${APP_HOSTNAME}` at apply time and mount the credentials file. **Neither the
token nor the credentials belong in this repository.**

## What the BFF must be told

Behind the tunnel every request arrives from cloudflared, so the BFF sees ONE
peer address for every user. The identity service rate-limits by caller, so the
real address has to be carried and — this is the part that matters — trusted only
where it cannot be forged:

```sh
export WEB_BFF_TRUSTED_PROXY_HEADER=CF-Connecting-IP
export WEB_BFF_TRUSTED_PROXIES=127.0.0.1      # wherever cloudflared runs
```

Set neither and the limiter still works, but keyed on the tunnel — so it becomes
estate-wide rather than per-caller, and one user's failed sign-ins throttle
everybody. Set the header **without** the trusted list and the BFF refuses to
start, because a header honoured from an untrusted peer is not a limit at all:
an attacker varies it per request and guesses without bound while the traffic
looks like many well-behaved clients.
