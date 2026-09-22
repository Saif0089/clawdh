# clawdh gateway — VPS deployment

The gateway is the data plane: it holds the shared Claude subscriptions and
proxies each team member's Claude Code to Anthropic, so many people use one
account at once and none of them ever holds its credential.

- Host: a plain Linux box (Ubuntu). Binary at `/opt/clawdh-server/clawdh-server`,
  config at `/opt/clawdh-server/env` (0600).
- Fronted by nginx + Let's Encrypt at `https://clawdh-gw.<ip>.sslip.io`
  (`sslip.io` resolves any label to the IP, so no DNS to manage).
- `deploy/clawdh-server.service` is the systemd unit; `deploy/nginx-clawdh-gw.conf`
  the nginx site (certbot adds the 443 block).
- That site **must** set `client_max_body_size` (64m). A Claude Code request
  carries the whole conversation, so a session with a few screenshots in it is
  several megabytes; nginx's default is 1m, and the HTML 413 it returns is shown
  by Claude Code as "Request too large (max 32MB) … run /compact" — pointing the
  member at their conversation instead of at the proxy. `clawdh-server diagnose`
  probes for this, but only with `CLAWDH_PUBLIC_URL` set: the probe has to go in
  from outside, because a request to 127.0.0.1 skips the proxy being tested.
- CI (`deploy-gateway` job) cross-compiles for linux/amd64 and ships the binary
  on every push to main, then restarts the service. Secrets: `VPS_SSH_KEY`,
  `VPS_HOST`; gated on the `GATEWAY_DEPLOY` repo variable.
