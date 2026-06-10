# Deploying the Shonei market server

The server is a single Go binary that listens on `127.0.0.1:8083`, persists
state to JSON files in its working directory, and is fronted by Caddy for TLS
(`wss://`). It runs as a `systemd` service so it survives crashes and reboots.

- **Host:** Hetzner CPX11, Ashburn — `5.161.112.237`
- **Subdomain:** `market.anita.garden`
- **Layout on the box:** everything lives in `/opt/shonei/`

```
internet ──wss:443──▶ Caddy ──http:8083(localhost)──▶ shonei-market-linux
```

Three pieces: **the box** (a Linux PC that's always on, reached with
`ssh root@5.161.112.237`); **the Go server** (the market — you don't run it by
hand, `systemd` keeps it running under the name `shonei`); and **Caddy** (the
front door that handles HTTPS and forwards traffic inward). You almost never
touch the Go server directly — you tell systemd what to do with it. After a
reboot, systemd starts everything back up on its own.

---

## Day-to-day operation (cheat sheet)

**Push a code change live** — from this repo on your PC. This is the whole
update workflow (rebuild + upload + restart; live market data is preserved):
```powershell
.\deploy\deploy.ps1
```

**Get onto the box** for anything else:
```powershell
ssh root@5.161.112.237
```

> **On Git Bash / MINGW (Windows):** plain `ssh` connects but accepts no keyboard
> input — it looks frozen at the prompt because MINGW doesn't give the session a
> PTY. Use `winpty ssh root@5.161.112.237` instead. Or skip the interactive shell
> entirely and run one-shot commands, which need no PTY:
> `ssh root@5.161.112.237 "systemctl status shonei --no-pager"`.

**Once SSH'd in** — `systemctl` talks to systemd; the service is named `shonei`:
```bash
systemctl status shonei      # is it alive? running/stopped + recent logs
systemctl restart shonei     # off and on again — fixes most hiccups
systemctl stop shonei        # off
systemctl start shonei       # on
journalctl -u shonei -f      # watch logs live (Ctrl+C to stop); -n 50 instead of -f for last 50 lines
```

The three situations you'll actually hit:
- *Changed server code* → `.\deploy\deploy.ps1` on your PC. Nothing else.
- *Is it up / seems broken* → SSH in, `systemctl status shonei`; if stuck, `systemctl restart shonei`.
- *Want to watch/debug* → SSH in, `journalctl -u shonei -f`.

Everything below (Caddy, firewall, TLS, the systemd unit) is one-time setup that
now runs itself — certs even auto-renew. You only need it if changing how the
deployment works.

---

## One-time server bootstrap

Do this once. SSH in: `ssh root@5.161.112.237`

### 1. Create the app directory
```bash
mkdir -p /opt/shonei
```

### 2. Install Caddy
(Official apt repo — see https://caddyserver.com/docs/install#debian-ubuntu-raspbian)
```bash
apt install -y debian-keyring debian-archive-keyring apt-transport-https curl
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | tee /etc/apt/sources.list.d/caddy-stable.list
apt update && apt install -y caddy
```

### 3. Install the Caddyfile
Copy `deploy/Caddyfile` from this repo to `/etc/caddy/Caddyfile` on the box
(edit the domain first), then:
```bash
systemctl reload caddy
```
Caddy will fetch the TLS cert automatically — but only once DNS is pointing at
the box (next step).

### 4. Point DNS at the box  (Cloudflare)
In the Cloudflare dashboard for anita.garden, add an **A record**:

| Type | Name   | Content        | Proxy status        |
|------|--------|----------------|---------------------|
| A    | market | 5.161.112.237  | **DNS only (grey)** |

Grey-cloud is important: proxied/orange-cloud would put Cloudflare in front and
fight Caddy over TLS. Grey = traffic goes straight to Hetzner, Caddy owns the cert.

### 5. Install the systemd service
Copy `deploy/shonei.service` to `/etc/systemd/system/shonei.service`, then:
```bash
systemctl daemon-reload
systemctl enable shonei      # start on boot (won't run yet — no binary uploaded)
```

### 6. First deploy
Back on your Windows machine, from the repo root:
```powershell
.\deploy\deploy.ps1
```
This builds the Linux binary, ships it + `traders.json`, and starts the service.

### 7. Verify
```bash
systemctl status shonei      # should be active (running)
journalctl -u shonei -f      # live logs; you should see "server starting on 127.0.0.1:8083"
```
From anywhere: visit `https://market.anita.garden` in a browser — Caddy should
answer with TLS (a 404/empty body is fine; it means TLS + proxy are working, the
path just isn't `/ws`).

---

## Auth secret (one-time, required)

The server signs login tokens with an HMAC secret read from `SHONEI_SECRET`,
supplied via `/opt/shonei/shonei.env` (referenced by `shonei.service`, kept off
git). **The unit will not start without it** — deliberate, so the public server
never silently falls back to insecure `?name=` mode. Set it up once:

```bash
# On the box. Generate a long random secret and write the env file.
echo "SHONEI_SECRET=$(openssl rand -hex 32)" > /opt/shonei/shonei.env
chmod 600 /opt/shonei/shonei.env
```

If you change `deploy/shonei.service`, re-copy it to
`/etc/systemd/system/shonei.service` and `systemctl daemon-reload` (the deploy
script does NOT touch the unit file — that's one-time bootstrap).

Locally (no `SHONEI_SECRET`), the server runs in **insecure dev mode**: it still
accepts `?name=` so the CLI test client works, and logs a loud warning.

## Routine redeploys

Just run `.\deploy\deploy.ps1` again. It stops the service, uploads the new
binary + `traders.json`, and restarts. **Runtime state on the server is never
touched** — `pricelog.json`, `traderstock.json`, `accounts.json`, and
`shonei.env` are not uploaded — so the live market and registered accounts
survive a redeploy. To wipe market state, delete the first two and restart:
`rm /opt/shonei/pricelog.json /opt/shonei/traderstock.json && systemctl restart shonei`
(leave `accounts.json` alone unless you mean to drop all accounts).

## Unity client

Point `TradingClient.cs` at the deployed server:
```
wss://market.anita.garden/ws?name=PlayerName
```
(was `ws://127.0.0.1:8083/ws?...` in local dev.)

## Notes / future

- **Auth.** Username/password accounts (`auth.go`): `POST /register` and
  `POST /login` (JSON `{username, password}`) return an HMAC-signed token; the WS
  handshake takes `?token=` instead of `?name=`. Tokens are stateless — they
  can't be revoked before their 30-day expiry. Player names that collide with NPC
  traders are reserved. See `plans/account-system.md` for the full roadmap (this
  is Phase 0; client login UI + account-owned saves are later phases).
- **Backups.** Not enabled (Hetzner backups are a ~20% surcharge). State is now
  `pricelog.json`, `traderstock.json`, and `accounts.json` (password hashes) —
  `scp` them down occasionally. **Revisit before account-owned saves ship** (real
  player data on a box with no backups).
