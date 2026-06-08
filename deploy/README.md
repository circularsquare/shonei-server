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

## Routine redeploys

Just run `.\deploy\deploy.ps1` again. It stops the service, uploads the new
binary + `traders.json`, and restarts. **State files (`pricelog.json`,
`traderstock.json`) on the server are never touched**, so the live market
survives a redeploy. To wipe state and start the market fresh, delete those two
files on the box and restart: `rm /opt/shonei/pricelog.json /opt/shonei/traderstock.json && systemctl restart shonei`.

## Unity client

Point `TradingClient.cs` at the deployed server:
```
wss://market.anita.garden/ws?name=PlayerName
```
(was `ws://127.0.0.1:8083/ws?...` in local dev.)

## Notes / future

- **No auth.** Anyone with the URL can connect under any name and place orders.
  Fine for trusted friends on an unadvertised subdomain; if it ever matters, add
  a shared-secret `?key=` check in `serveWs` and reject mismatches.
- **Backups.** Not enabled (Hetzner backups are a ~20% surcharge). The only state
  is the two JSON files — `scp` them down occasionally if you want a copy.
