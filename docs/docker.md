# A scoreboard in Docker

The server's image is one static binary on an empty base, running as an unprivileged user with the data
in a volume. This guide runs the image with Docker Compose on any machine. The [README](../README.md)
explains what a scoreboard is and defines the roles used here: the operator, the admin, and the members.

## Before deploying

The deployment goes through four stages:

1. Deploy the server. `docker compose up` builds the image from the repository and starts the
   container, and the container's log prints a setup code, a random secret that creates the admin
   account. The operator keeps the setup code until the admin account exists.
2. Reach the scoreboard over HTTPS. The FitHalo app refuses a scoreboard without HTTPS, and passkeys
   require HTTPS. As shipped, the container expects a tunnel or reverse proxy on the same machine to
   terminate TLS; the container can also serve TLS itself from a mounted certificate. See
   [Step 2](#step-2-reach-the-scoreboard-over-https).
3. Create the admin account. The operator opens the scoreboard's address followed by `/admin/setup`,
   enters the setup code, names the scoreboard, and chooses the admin's username and password. The
   portal's Account page then offers to add a passkey, so the admin can sign in without the password.
4. Check the scoreboard. The `check` subcommand, run inside the container or from any computer, joins
   the scoreboard as a temporary member, exercises each part of the API, and leaves again.

### Secrets stored outside the data

Three values live outside the `scoreboard-data` volume.

| Secret | Where the secret lives with Docker |
|---|---|
| The setup code | The server generates the code at the first start and prints the code to the container's log. To avoid a code in the log, the operator sets `SCOREBOARD_SETUP_CODE` in `compose.yaml` before the first start; a configured code is never printed, and the log names the variable |
| The pepper | A random secret mixed into all password hashes, so a copy of the volume alone cannot be used to guess the admin's password. The operator writes the pepper to a file named `pepper` beside `compose.yaml`, which the compose file mounts as the Docker secret `/run/secrets/pepper`. The pepper never changes once set: changing the pepper invalidates the admin's password and the scoreboard's join password |
| The recovery code | Needed only where `docker compose exec` is not available, as the environment variable `SCOREBOARD_RECOVERY_CODE` on the container. Otherwise `fithalo-scoreboard reset-admin` prints a recovery code. See [Admin recovery](../README.md#admin-recovery) |

### Configuration reference

The image sets three variables, and [`compose.yaml`](../compose.yaml) sets the rest. The reference is
here for an operator who changes a value in `compose.yaml` or a `docker run` command.

| Variable | Meaning | Default |
|---|---|---|
| `SCOREBOARD_DATA` | The directory inside the container that stores the data | `/data`, set by the image |
| `SCOREBOARD_LISTEN` | The address and port to listen on inside the container | `0.0.0.0:8080`, set by the image |
| `SCOREBOARD_TLS` | `proxy` serves plain HTTP behind a proxy that terminates TLS; `files` serves TLS from a mounted certificate. See [Transport security](../README.md#transport-security) | `proxy`, set by the image |
| `SCOREBOARD_PUBLIC_URL` | The address members reach the scoreboard at. Must be `https` | `https://board.example.com` in `compose.yaml`, to be replaced |
| `SCOREBOARD_PEPPER_FILE` | The file that contains the pepper | `/run/secrets/pepper` in `compose.yaml` |
| `SCOREBOARD_PEPPER` | The pepper as a string, for a `docker run` command without a secrets file | None |
| `SCOREBOARD_SETUP_CODE` | The setup code, at least 16 characters | Generated at the first start |
| `SCOREBOARD_CERT`, `SCOREBOARD_KEY` | The certificate chain and private key files, in `files` mode | None |
| `SCOREBOARD_TRUSTED_PROXIES` | The proxies whose forwarding headers count, in `proxy` mode: addresses or CIDRs separated by commas, or `any`. See [Trusted proxies](../README.md#trusted-proxies) | Not set; the server listening on all interfaces trusts loopback, private, shared, and link-local peers only |
| `SCOREBOARD_RECOVERY_CODE` | A recovery code, at least 16 characters, where `exec` is not available | None |

The `check` subcommand reads the scoreboard's join password from `SCOREBOARD_CHECK_PASSWORD`, so the
password never appears in the process list.

## Step 1: Build and configure

From a copy of the repository:

```bash
head -c 32 /dev/urandom | base64 > pepper
```

The pepper file stays beside the compose file, out of the volume, so a copy of the scoreboard's data
alone cannot be used to guess the admin's password. The operator then sets the scoreboard's address in
[`compose.yaml`](../compose.yaml) (`SCOREBOARD_PUBLIC_URL`) and starts the scoreboard:

```bash
docker compose up -d --build
docker compose logs scoreboard
```

The log prints a setup code on the first start.

## Step 2: Reach the scoreboard over HTTPS

As shipped, [`compose.yaml`](../compose.yaml) publishes the scoreboard on `127.0.0.1:8080` in `proxy`
mode, for a tunnel or a reverse proxy on the same machine that terminates TLS: Tailscale Funnel,
Cloudflare Tunnel, Caddy, or nginx. The server refuses any request the proxy did not receive over HTTPS,
so the proxy must send `X-Forwarded-Proto: https`; Tailscale, Cloudflare, and Caddy do by default, and
nginx needs `proxy_set_header X-Forwarded-Proto $scheme;`.
[Step 2 of the Raspberry Pi guide](raspberry-pi.md#step-2-reach-the-scoreboard-from-outside) walks
through the tunnels.

The server reads the client's address for the rate limits from `X-Forwarded-For`, and trusts both
headers from any peer that reaches port 8080. Publishing on `127.0.0.1` keeps all peers on the same
machine. To narrow that further, the operator sets `SCOREBOARD_TRUSTED_PROXIES` to a comma-separated list
of the proxies' addresses or CIDRs. The server then trusts the forwarding headers only from those peers
and answers any other peer with status 421. Inside a container, a proxy on the host arrives from the
Docker network's gateway, not from `127.0.0.1`, and a proxy in another container arrives from that
container's address; `docker network inspect` shows both. For example,
`SCOREBOARD_TRUSTED_PROXIES: 172.16.0.0/12` covers Docker's default networks. Without the setting, a
server that listens on all interfaces, as the image does, trusts the forwarding headers only from
loopback, private, shared (`100.64.0.0/10`), and link-local addresses, and answers a peer on a public
address with status 421. The value `any` makes the server trust all peers.

### Without Compose

The same image runs with `docker run`. The operator publishes the port on `127.0.0.1` only, so the
plain-HTTP listener is never reachable from another machine:

```bash
docker build -t fithalo-scoreboard .
docker volume create scoreboard-data
docker run -d --name scoreboard --restart unless-stopped \
  -p 127.0.0.1:8080:8080 \
  -e SCOREBOARD_PUBLIC_URL=https://scoreboard.example.com \
  -e SCOREBOARD_PEPPER_FILE=/run/secrets/pepper \
  -v "$PWD/pepper:/run/secrets/pepper:ro" \
  -v scoreboard-data:/data \
  fithalo-scoreboard
```

A plain `-p 8080:8080` publishes the port on all interfaces of the machine, in plain text, and lets any
client set its own forwarding headers.

### Serving TLS from the container

To serve TLS from the container itself, the operator mounts the certificate and switches modes:

```yaml
    ports:
      - "443:8443"
    environment:
      SCOREBOARD_TLS: files
      SCOREBOARD_LISTEN: 0.0.0.0:8443
      SCOREBOARD_CERT: /certificates/fullchain.pem
      SCOREBOARD_KEY: /certificates/privkey.pem
    volumes:
      - scoreboard-data:/data
      - /etc/fithalo-scoreboard/tls:/certificates:ro
```

The server detects a renewed certificate on its own within half a minute. Certbot keeps its private key
readable by root only, and the container runs as user 65532, so the operator mounts a copy the container
can read, made by a certbot deploy hook as in
[Option C of the Raspberry Pi guide](raspberry-pi.md#option-c-a-forwarded-port-and-certbot), rather than
certbot's own folder. In the hook, the copies belong to the container's user:
`install -m 0400 -o 65532 ... /etc/fithalo-scoreboard/tls/`.

## Step 3: Create the admin account and check the scoreboard

The operator opens `https://<scoreboard>/admin/setup` and enters the setup code. The log prints a setup
code only when the server generated the code at start; a code set in `SCOREBOARD_SETUP_CODE` is never
written to the log, and the log names the variable. The admin's new password is checked against Have I
Been Pwned; when that service cannot be reached, the server skips the check, logs a warning, and accepts
the password.

The check then runs from any machine:

```bash
docker compose exec scoreboard /fithalo-scoreboard check -url https://<scoreboard> -invite <invite code>
```

The check has no password flag, so a scoreboard's join password never appears in the process list. To
check a scoreboard with a join password, the operator runs the check from a computer with the release
unpacked, with the password in `SCOREBOARD_CHECK_PASSWORD`, as
[Step 4 of the Raspberry Pi guide](raspberry-pi.md#step-4-check-the-scoreboard) shows.

## Operating a scoreboard in Docker

| Task | How |
|---|---|
| Backups | The `scoreboard-data` volume, and the `pepper` file kept somewhere else |
| Updating | `git pull`, then `docker compose up -d --build` |
| Locked out | `docker compose exec scoreboard /fithalo-scoreboard reset-admin -data /data` prints a recovery code that works once, for 15 minutes, at `/admin/recover`. Where `exec` is not available, `SCOREBOARD_RECOVERY_CODE` does the same job; see [Admin recovery](../README.md#admin-recovery) |
