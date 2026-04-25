# Cloudflare Tunnel + Access setup

This document walks the operator through exposing the loopback-only
`prospect-api` to a public HTTPS URL backed by Cloudflare, with an
Access policy that limits which clients can reach the origin.

Run once. ~30 minutes.

## Prerequisites

- A Cloudflare account (free tier is sufficient).
- A domain managed in Cloudflare DNS (e.g., `example.com`).
- The `cloudflared` CLI on the Mac running prospect-api.
- The prospect-api running locally on port 8081.

```bash
brew install cloudflared
cloudflared --version
```

## Step 1. Authenticate cloudflared

```bash
cloudflared tunnel login
```

A browser window opens. Sign in to your Cloudflare account, pick the zone
(domain), and approve. A certificate is downloaded to
`~/.cloudflared/cert.pem`.

## Step 2. Create the tunnel

```bash
cloudflared tunnel create prospect-api
```

Output includes a tunnel ID and a credentials file path
(e.g., `~/.cloudflared/<UUID>.json`). Note both.

## Step 3. Add a DNS record

```bash
cloudflared tunnel route dns prospect-api prospect.example.com
```

Replace `prospect.example.com` with the public hostname you want. This
creates a CNAME in your Cloudflare DNS pointing to `<UUID>.cfargotunnel.com`.

## Step 4. Configure the tunnel

Create `~/.cloudflared/config.yml`:

```yaml
tunnel: <UUID>
credentials-file: /Users/<you>/.cloudflared/<UUID>.json

ingress:
  - hostname: prospect.example.com
    service: http://127.0.0.1:8081
  - service: http_status:404
```

The `http_status:404` rule is the catch-all required by `cloudflared`.

## Step 5. Run the tunnel

```bash
cloudflared tunnel run prospect-api
```

Leave this running. Verify:

```bash
curl https://prospect.example.com/healthz
# Should return the healthz JSON.
```

## Step 6. Lock the origin with Cloudflare Access

The tunnel is now public. Anyone with the URL can hit it. Add a Cloudflare
Access policy so only the AI Concierge can reach it.

### 6a. Create an Access application

In the Cloudflare dashboard:

1. Zero Trust > Access > Applications > Add an application.
2. Self-hosted.
3. Application name: `prospect-api`.
4. Application domain: `prospect.example.com`.
5. Session duration: 24 hours (or shorter for tighter rotation).

### 6b. Create a service token

1. Zero Trust > Access > Service Auth > Service Tokens > Create Service Token.
2. Name: `concierge-vercel`.
3. Note the Client ID and Client Secret (shown once).

### 6c. Add an allow-policy that requires the service token

1. Back on the Access application, Policies tab > Add a policy.
2. Action: Allow.
3. Configure rules:
   - Include > Service Auth > pick the `concierge-vercel` token.
4. Save.

Now requests to `prospect.example.com` must include:

```
Cf-Access-Client-Id: <client-id>
Cf-Access-Client-Secret: <client-secret>
```

The dev's concierge backend stores these as environment variables on Vercel
and includes them on every request.

### 6d. Enable trust check in prospect-api

In your prospect-api environment:

```
PROSPECT_REQUIRE_CF_ACCESS=true
```

Restart prospect-api. Now even with a valid bearer token, requests without
a Cloudflare Access JWT header are rejected. Defense in depth.

## Step 7. Optional: run cloudflared as a service

So the tunnel survives reboots:

```bash
sudo cloudflared service install
```

The launchd plist runs cloudflared on every boot and restarts it on crash.

## Step 8. Token handoff to the developer

Send the developer:

- The public URL: `https://prospect.example.com`
- The bearer token: `PROSPECT_BRIDGE_AUTH_TOKEN` (paste this; do not commit)
- The Cloudflare Access service token: client ID and client secret
- The endpoint spec: `prospect-api/SETUP.md` (in this repo)
- The threat model: `prospect-api/SECURITY-PROSPECT.md` (in this repo)

The dev's chat agent backend includes both the bearer and the Cf-Access
headers on every request.

## Verify end to end

From a machine the developer controls (not your Mac):

```bash
curl -X POST https://prospect.example.com/api/lookup-prospect \
  -H "Authorization: Bearer $BRIDGE_TOKEN" \
  -H "Cf-Access-Client-Id: $CF_ID" \
  -H "Cf-Access-Client-Secret: $CF_SECRET" \
  -H "Content-Type: application/json" \
  -d '{"phone":"+10000000000"}'
```

Expected: `{"matches":[],"latencyMs":...}` (no match for the test number,
but the response shape is correct).

## Troubleshooting

- **403 immediately**: Cloudflare Access rejected the request. Service
  token headers missing or wrong, or the policy doesn't include the
  service token.
- **401 from the Go server**: Bearer token mismatch. Compare with
  `PROSPECT_BRIDGE_AUTH_TOKEN` on the Mac.
- **502 from Cloudflare**: tunnel isn't running, or `prospect-api` isn't
  listening on 8081. SSH into the Mac, check `pgrep -fl prospect-api`
  and `cloudflared` process status.
- **Tunnel works locally but tokens leak**: rotate the service token in
  the Cloudflare dashboard. Old token is revoked immediately.

## Auditing on the prospect-api side

Every request hits the audit log on the Mac. Periodic check:

```bash
sqlite3 ~/.claude/whatsapp-mcp/prospect-api/store/presets.db \
  "SELECT datetime(ts,'unixepoch'), endpoint, result_summary, duration_ms FROM audit_log ORDER BY ts DESC LIMIT 20"
```

Watch for unexpected client IPs, unfamiliar endpoints, or 4xx clusters.
