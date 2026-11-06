# A scoreboard on the Azure free tier

This guide puts a FitHalo Scoreboard on Microsoft Azure, inside the allowances Azure keeps free
permanently: a container app that sleeps between visits, and Cosmos DB's free tier. The deployment takes
about twenty minutes and needs no software on the operator's computer. The [README](../README.md)
explains what a scoreboard is and defines the roles used here: the operator, the admin, and the members.

## What the deployment uses, and what the deployment costs

| Piece | Purpose | Azure's free allowance (October 2026) |
|---|---|---|
| Container Apps, 0.25 vCPU and 0.5 GiB, scaling to zero | Runs the scoreboard | 180,000 vCPU-seconds, 360,000 GiB-seconds, and 2 million requests a month, per subscription |
| Container Apps ingress | Gives the scoreboard an HTTPS address, TLS 1.2 or later | Included |
| Cosmos DB for NoSQL, 400 RU/s, strong consistency | Stores the scoreboard's data | 1,000 RU/s and 25 GB, for one account per subscription |

The Container Apps allowance is time a replica spends running rather than a count of requests: about 200
hours a month at this size. The server starts in a fraction of a second, so the container can sleep
between visits; a replica stays up for a few minutes after the last request, then stops. A small
scoreboard used through the day fits; the operator checks the usage in the Azure portal in the first
weeks. Azure's free services page states the current terms, which change; the operator checks the page
before starting.

Cosmos DB's free tier is one account per subscription. If the subscription already has a free-tier
account, the deployment fails with a message saying so. The operator either deploys into another
subscription, or runs the deployment with `freeTier=false`, in which case Cosmos DB is charged at the
normal rate. A deployment run by hand rather than by the script also needs a `setupCode` parameter of 16
characters or more.

## Before deploying

The deployment goes through four stages:

1. Deploy the server. The deploy script creates a resource group, deploys the template, and prints a
   setup code, a random secret that creates the admin account. The setup code is shown once; the
   operator keeps the setup code until the admin account exists.
2. Reach the scoreboard over HTTPS. Container Apps ingress provides an HTTPS address, so nothing more
   is needed. A custom domain is optional; see [Operating a scoreboard on Azure](#operating-a-scoreboard-on-azure).
3. Create the admin account. The operator opens the scoreboard's address followed by `/admin/setup`,
   enters the setup code, names the scoreboard, and chooses the admin's username and password. The
   portal's Account page then offers to add a passkey, so the admin can sign in without the password.
4. Check the scoreboard. The `check` subcommand, run from any computer, joins the scoreboard as a
   temporary member, exercises each part of the API, and leaves again.

### Secrets stored outside the data

Three values live in the container app's configuration and never in Cosmos DB.

| Secret | Where the secret lives on Azure |
|---|---|
| The setup code | The deploy script generates the code and passes the code to the template as a secure parameter, which Azure keeps out of the resource group's deployment history. The template stores the code as the container app secret `setup-code` and exposes the secret as `SCOREBOARD_SETUP_CODE`. The scoreboard's log never prints a configured code; the log names the variable |
| The pepper | A random secret mixed into all password hashes, so a copy of the database alone cannot be used to guess the admin's password. The template generates the pepper and stores the pepper as the container app secret `pepper`, exposed as `SCOREBOARD_PEPPER`. The pepper never changes once set: changing the pepper invalidates the admin's password and the scoreboard's join password, which is why the deploy script never redeploys the template on an existing scoreboard |
| The recovery code | Set only while the admin is locked out, as the environment variable `SCOREBOARD_RECOVERY_CODE` on the container app. See [Admin recovery](../README.md#admin-recovery) |

### Configuration reference

The template sets these environment variables on the container app. The reference is here for an
operator who changes a value by hand in the Azure portal.

| Variable | Meaning | Set by |
|---|---|---|
| `SCOREBOARD_STORE` | `cosmos` | The template |
| `SCOREBOARD_COSMOS_ENDPOINT` | The Cosmos DB account's endpoint | The template |
| `SCOREBOARD_COSMOS_KEY` | The account key, from the container app secret `cosmos-key` | The template |
| `SCOREBOARD_COSMOS_DATABASE` | `scoreboard` | The template |
| `SCOREBOARD_COSMOS_CONTAINER` | `board` | The template |
| `SCOREBOARD_TLS` | `proxy`: the container serves plain HTTP behind Azure's ingress, which terminates TLS | The template |
| `SCOREBOARD_LISTEN` | `0.0.0.0:8080` | The template |
| `SCOREBOARD_PUBLIC_URL` | The scoreboard's address: the container app's default domain, or a custom domain once one is added | The template |
| `SCOREBOARD_SETUP_CODE` | The setup code, at least 16 characters | The template, from the secret `setup-code` |
| `SCOREBOARD_PEPPER` | The pepper, at least 16 characters | The template, from the secret `pepper` |
| `SCOREBOARD_RECOVERY_CODE` | A recovery code, at least 16 characters, while the admin is locked out | The operator |
| `SCOREBOARD_TRUSTED_PROXIES` | Not set. Azure's ingress reaches the container from a private address, which the server trusts by default when listening on all interfaces. See [Trusted proxies](../README.md#trusted-proxies) | |

The `check` subcommand, run on the operator's computer, reads the scoreboard's join password from
`SCOREBOARD_CHECK_PASSWORD`, so the password never appears in the process list.

## Step 1: Open Cloud Shell

The operator signs in to the Azure portal and opens **Cloud Shell** from the toolbar, choosing Bash.
Cloud Shell is a terminal in the browser, already signed in to the subscription.

## Step 2: Deploy

In Cloud Shell:

```bash
curl -fsSLO https://github.com/jacobjohnston/FitHalo-Scoreboards/releases/latest/download/deploy-azure.sh
sh deploy-azure.sh fithalo-scoreboard eastus operator@example.com
```

The arguments are the resource group to create, the group's region, and the email address a budget alert
goes to if the group's spending ever passes one dollar in a month. The script:

1. downloads the template and checks its checksum, and also checks its build attestation when the `gh`
   CLI is installed and signed in, reporting the attestation check as skipped otherwise,
2. makes a random setup code and passes the code to the template as a secure parameter, which Azure
   keeps out of the resource group's deployment history,
3. deploys the scoreboard, running the image the release's template names by digest, so the image
   cannot change after the release, and
4. prints the scoreboard's address and the setup code, once.

The operator keeps the setup code until step 3: neither the script nor Azure shows the code again.

The template also makes a random pepper, stored as a secret of the container app and never in Cosmos DB.

## Step 3: Create the admin account

The operator opens the scoreboard's address followed by `/admin/setup`, enters the setup code, names the
scoreboard, and chooses a username and password. The portal's Account page then offers to add a passkey.

The address looks like `https://fithalo-scoreboard.<label>.<region>.azurecontainerapps.io`. Members never
type the address: members join from the invitation link or the link's QR code. The first visit after a
period without requests takes a moment longer while the container starts.

## Step 4: Check the scoreboard

From any computer, the operator downloads the release for that computer from the project's releases
page, unpacks the release, and runs:

```bash
./fithalo-scoreboard check -url https://<scoreboard> -invite <invite code>
```

The invite code is the last part of the invitation link in the portal. On a scoreboard that approves new
members, the check runs the checks that do not need an approved member and says so; the admin approves
the temporary member and runs the check again for the rest.

## Operating a scoreboard on Azure

| Task | How |
|---|---|
| Updating | Run `sh deploy-azure.sh fithalo-scoreboard` again. On an existing scoreboard the script downloads and checks the latest release's template, then moves the container app to the image that template names by digest. The script never redeploys the template on an existing scoreboard, which would make a new pepper and lock the admin out |
| Logs | The container app's **Log stream** in the portal |
| Backups | Cosmos DB's periodic backups are on by default |
| Locked out | Add an environment variable `SCOREBOARD_RECOVERY_CODE` to the container app, with a long random value of 16 characters or more, for example the output of `head -c 24 /dev/urandom \| base64`. Enter the value at `/admin/recover`, then remove the variable. Each value works once |
| A custom domain | Add the domain to the container app under **Custom domains**, with Azure's free managed certificate, and set `SCOREBOARD_PUBLIC_URL` to the domain so invitation links and passkeys use the domain |
| Removing the scoreboard | Delete the resource group |
