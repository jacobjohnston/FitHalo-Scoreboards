# A scoreboard on a Raspberry Pi

This guide puts a FitHalo Scoreboard on a Raspberry Pi at home, or on any small Linux machine: an old
laptop, a home server, or one of the free cloud machines Oracle and Google offer. The deployment takes
about half an hour and ends with a scoreboard the FitHalo app can join. The [README](../README.md)
explains what a scoreboard is and defines the roles used here: the operator, the admin, and the members.

## Requirements

- A Raspberry Pi 3, 4, 5, or Zero 2 W running Raspberry Pi OS, or any Linux machine with systemd.
- A way for phones outside the home network to reach the Pi over HTTPS. Three ways are compared under
  [Step 2](#step-2-reach-the-scoreboard-from-outside); the operator picks one before starting.

## Before deploying

The deployment goes through four stages:

1. Deploy the server. The installer puts the server on the Pi as a systemd service and prints a setup
   code, a random secret that creates the admin account. The setup code is shown once; the operator
   keeps the setup code until the admin account exists.
2. Reach the scoreboard over HTTPS. The FitHalo app refuses a scoreboard without HTTPS, and passkeys
   require HTTPS. A Pi at home needs a tunnel or a certificate; [Step 2](#step-2-reach-the-scoreboard-from-outside)
   compares the three options.
3. Create the admin account. The operator opens the scoreboard's address followed by `/admin/setup`,
   enters the setup code, names the scoreboard, and chooses the admin's username and password. The
   portal's Account page then offers to add a passkey, so the admin can sign in without the password.
4. Check the scoreboard. The `check` subcommand, run from any computer, joins the scoreboard as a
   temporary member, exercises each part of the API, and leaves again.

### Secrets stored outside the data

Three values live in `/etc/fithalo-scoreboard` and never in the data directory
`/var/lib/fithalo-scoreboard`.

| Secret | Where the secret lives on the Pi |
|---|---|
| The setup code | The installer generates the code on the first run and writes the code to `/etc/fithalo-scoreboard/environment` as `SCOREBOARD_SETUP_CODE`. The scoreboard's log never prints a configured code; the log names the variable |
| The pepper | A random secret mixed into all password hashes, so a copy of the data directory alone cannot be used to guess the admin's password. The installer generates the pepper on the first run and writes the pepper to `/etc/fithalo-scoreboard/pepper`, readable by root and the service's group only. The pepper never changes once set: changing the pepper invalidates the admin's password and the scoreboard's join password |
| The recovery code | Not needed on a Pi: `fithalo-scoreboard reset-admin` prints a recovery code from the shell. See [Admin recovery](../README.md#admin-recovery) |

### Configuration reference

The installer writes `/etc/fithalo-scoreboard/environment`, which the systemd service reads. The
reference is here for an operator who changes a value by hand; after a change, run
`sudo systemctl restart fithalo-scoreboard`.

| Variable | Meaning | Written by the installer |
|---|---|---|
| `SCOREBOARD_PUBLIC_URL` | The address members reach the scoreboard at, with any path the scoreboard is served under. Must be `https` | The installer's first argument |
| `SCOREBOARD_TLS` | `files` serves TLS from certificate files; `proxy` serves plain HTTP behind a tunnel or proxy on the Pi that terminates TLS. See [Transport security](../README.md#transport-security) | The installer's second argument |
| `SCOREBOARD_LISTEN` | The address and port to listen on | `0.0.0.0:443` in `files` mode; `127.0.0.1:8080` in `proxy` mode |
| `SCOREBOARD_DATA` | The directory that stores the data | `/var/lib/fithalo-scoreboard` |
| `SCOREBOARD_PEPPER_FILE` | The file that contains the pepper | `/etc/fithalo-scoreboard/pepper` |
| `SCOREBOARD_SETUP_CODE` | The setup code, at least 16 characters | Generated on the first run |
| `SCOREBOARD_CERT` | The certificate chain file, in `files` mode | `/etc/fithalo-scoreboard/tls/fullchain.pem` |
| `SCOREBOARD_KEY` | The certificate's private key file, in `files` mode | `/etc/fithalo-scoreboard/tls/privkey.pem` |
| `SCOREBOARD_TRUSTED_PROXIES` | The proxies whose forwarding headers count, in `proxy` mode: addresses or CIDRs separated by commas, or `any`. See [Trusted proxies](../README.md#trusted-proxies) | Not written. In `proxy` mode on `127.0.0.1`, any peer that reaches the port is trusted, and only programs on the Pi can |
| `SCOREBOARD_RECOVERY_CODE` | A recovery code, at least 16 characters, for a host with no shell | Not written |

The same settings are available as command-line flags (`-public-url`, `-tls`, `-listen`, `-data`,
`-cert`, `-key`, `-trusted-proxies`) for running the binary by hand. The `check` subcommand reads the
scoreboard's join password from `SCOREBOARD_CHECK_PASSWORD`, so the password never appears in the
process list.

## Step 1: Install the scoreboard

On the Pi, the operator downloads the release for the Pi's processor from the project's releases page. A
Pi running the 64-bit Raspberry Pi OS needs `linux-arm64`; the 32-bit Raspberry Pi OS needs
`linux-armv7`. The operator then unpacks the release and runs the installer with the address the
scoreboard will have and the way the scoreboard will be reached:

```bash
tar -xzf fithalo-scoreboard-*-linux-arm64.tar.gz
cd fithalo-scoreboard-*-linux-arm64
sudo ./install.sh https://scoreboard.example.com proxy
```

The second argument is `proxy` with Tailscale Funnel or Cloudflare Tunnel, and `files` with a forwarded
port and certbot.

The installer creates a `fithalo-scoreboard` system user, stores the scoreboard's data in
`/var/lib/fithalo-scoreboard`, the settings in `/etc/fithalo-scoreboard/environment`, and a random pepper
in `/etc/fithalo-scoreboard/pepper`. The installer prints a setup code; the operator keeps the setup code
for step 3. Running the installer again later updates the scoreboard and keeps everything else.

## Step 2: Reach the scoreboard from outside

| | Tailscale Funnel | Cloudflare Tunnel | Port forward and certbot |
|---|---|---|---|
| Address | `https://pi-name.tailnet-name.ts.net` | `https://scoreboard.example.com` | `https://scoreboard.example.com` |
| Needs a domain | No | Yes, on Cloudflare | Yes, anywhere |
| Router changes | None | None | Forward ports 80 and 443 |
| Third party in the path | Tailscale | Cloudflare | None |
| Cost | Free | The domain, about $10 a year | The domain |
| Installer mode | `proxy` | `proxy` | `files` |

Tailscale and Cloudflare both see the scoreboard's traffic pass through them: encrypted end to end with
Tailscale, and terminated at Cloudflare's edge with Cloudflare. With a forwarded port nothing stands
between the phones and the Pi, but the home network is open on port 443.

### Option A: Tailscale Funnel

1. Install Tailscale on the Pi and sign in: `curl -fsSL https://tailscale.com/install.sh | sh`, then
   `sudo tailscale up`.
2. In the Tailscale admin console, turn on HTTPS certificates and Funnel for the tailnet.
3. Expose the scoreboard's port to the internet: `sudo tailscale funnel --bg 8080`.
4. The scoreboard's address is the one Tailscale prints, `https://<pi-name>.<tailnet>.ts.net`. If that
   address differs from the address given to the installer, change `SCOREBOARD_PUBLIC_URL` in
   `/etc/fithalo-scoreboard/environment` and run `sudo systemctl restart fithalo-scoreboard`.

### Option B: Cloudflare Tunnel

1. Move the domain's DNS to Cloudflare, if the DNS is not there already.
2. Install `cloudflared` on the Pi from Cloudflare's package repository.
3. Create the tunnel and assign the scoreboard's hostname to the tunnel:

   ```bash
   cloudflared tunnel login
   cloudflared tunnel create scoreboard
   cloudflared tunnel route dns scoreboard scoreboard.example.com
   ```

4. Point the tunnel at the scoreboard, in `/etc/cloudflared/config.yml`:

   ```yaml
   tunnel: scoreboard
   credentials-file: /root/.cloudflared/<tunnel id>.json
   ingress:
     - hostname: scoreboard.example.com
       service: http://127.0.0.1:8080
     - service: http_status:404
   ```

5. Run the tunnel as a service: `sudo cloudflared service install`.

### Option C: A forwarded port and certbot

1. Point `scoreboard.example.com` at the home's public IP address, with a dynamic DNS service if that
   address changes.
2. On the router, forward ports 80 and 443 to the Pi.
3. Install certbot and get a certificate. Port 80 has to be free while certbot runs:

   ```bash
   sudo apt install certbot
   sudo certbot certonly --standalone -d scoreboard.example.com
   ```

4. Give the scoreboard a readable copy of the certificate, now and after each renewal, with a deploy hook
   in `/etc/letsencrypt/renewal-hooks/deploy/fithalo-scoreboard`:

   ```bash
   #!/bin/sh
   install -m 0640 -g fithalo-scoreboard /etc/letsencrypt/live/scoreboard.example.com/fullchain.pem /etc/fithalo-scoreboard/tls/
   install -m 0640 -g fithalo-scoreboard /etc/letsencrypt/live/scoreboard.example.com/privkey.pem /etc/fithalo-scoreboard/tls/
   ```

   Make the hook executable, run the hook once by hand, then run `sudo systemctl start fithalo-scoreboard`.
   The server detects a renewed certificate on its own, within half a minute, with no restart.

## Step 3: Create the admin account

The operator opens `https://<scoreboard>/admin/setup`, enters the setup code from step 1, names the
scoreboard, and chooses a username and password. The portal's Account page then offers to add a passkey,
so the admin can sign in without the password.

The installer stores the setup code in `SCOREBOARD_SETUP_CODE`, so the scoreboard's log names that
variable and never prints the code. The new password is checked against Have I Been Pwned; when that
service cannot be reached, the server skips the check, logs a warning, and accepts the password.

## Step 4: Check the scoreboard

From any computer with the release unpacked:

```bash
./fithalo-scoreboard check -url https://<scoreboard> -invite <invite code>
```

The invite code is the last part of the invitation link in the portal. On a scoreboard that approves new
members, the check runs the checks that do not need an approved member and says so; the admin approves
the temporary member and runs the check again for the rest.

A scoreboard with a join password needs the password in `SCOREBOARD_CHECK_PASSWORD`. The check has no
password flag, so the password never appears in the process list. `read` keeps the password out of the
shell history too:

```bash
read -rs SCOREBOARD_CHECK_PASSWORD && export SCOREBOARD_CHECK_PASSWORD
./fithalo-scoreboard check -url https://<scoreboard> -invite <invite code>
```

## Operating a scoreboard on a Raspberry Pi

| Task | How |
|---|---|
| Logs | `journalctl -u fithalo-scoreboard` |
| Backups | Copy `/var/lib/fithalo-scoreboard`. Each file there contains one document, base64-encoded and usually DEFLATE-compressed, so the documents are not readable as text. The pepper and settings in `/etc/fithalo-scoreboard` are kept apart on purpose; the operator backs up those two files somewhere else |
| Updating | Download the new release and run its `install.sh` again with the same arguments |
| Privileges | The service runs with no Linux capabilities. In `files` mode the installer adds a drop-in, `/etc/systemd/system/fithalo-scoreboard.service.d/bind-port-443.conf`, that grants only `CAP_NET_BIND_SERVICE` so the server can listen on port 443; in `proxy` mode the installer removes the drop-in |
| Locked out | `sudo -u fithalo-scoreboard fithalo-scoreboard reset-admin -data /var/lib/fithalo-scoreboard` prints a recovery code that works once, for 15 minutes, at `/admin/recover`. Recovery and passkey sign-in stay available while repeated failed attempts keep the password sign-in at its rate limit |
| Trusted proxies | In `proxy` mode the server reads the client's address for the rate limits from `X-Forwarded-For`. The installer listens on `127.0.0.1`, so only programs on the Pi reach the server. A proxy on another machine needs a private network address in `SCOREBOARD_LISTEN`; the operator then adds `SCOREBOARD_TRUSTED_PROXIES` to `/etc/fithalo-scoreboard/environment` with the proxy's address or CIDR, several separated by commas, and runs `sudo systemctl restart fithalo-scoreboard`. See [Trusted proxies](../README.md#trusted-proxies) |

## On a free cloud machine

Oracle's Always Free Ampere machines and Google's `e2-micro` run the same `linux-arm64` and
`linux-amd64` releases. The steps are the same. With a forwarded port, the router is the provider's
firewall: the operator opens ports 80 and 443 to the machine in Oracle's security list or Google's
firewall rules. Tailscale Funnel and Cloudflare Tunnel need no ports opened at all.
