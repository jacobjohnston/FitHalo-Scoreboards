# A scoreboard on the AWS free tier

This guide puts a FitHalo Scoreboard on Amazon Web Services, inside the allowances AWS keeps free
permanently: a Lambda function and a DynamoDB table. The deployment takes about fifteen minutes and
needs no software on the operator's computer. The [README](../README.md) explains what a scoreboard is
and defines the roles used here: the operator, the admin, and the members.

## What the deployment uses, and what the deployment costs

| Piece | Purpose | AWS's free allowance (October 2026) |
|---|---|---|
| Lambda, 128 MB, arm64 | Runs the scoreboard | 1 million requests and 400,000 GB-seconds a month, indefinitely |
| A function URL | Gives the scoreboard an HTTPS address | No charge |
| DynamoDB, 10 read and 10 write capacity units | Stores the scoreboard's data | 25 GB and 25 units of each, indefinitely |
| An hourly EventBridge schedule | Deletes old weekly challenges, ended sign-ins, and old nonces | Scheduled rules are free |
| CloudWatch Logs, kept 7 days | The scoreboard's log | 5 GB a month free |

A scoreboard of fifty members who each open the scoreboard a few times a day makes around 30,000
requests a month, three percent of Lambda's allowance. AWS's free tier page states the current terms,
which change; the operator checks the page before starting.

A new AWS account starts on the Free plan, which closes after six months or when the account's credits
run out, whichever comes first. The allowances above carry on under the Paid plan, where AWS charges only
for usage beyond the allowances. Before the six months are up, the operator moves the account to the Paid
plan from the Billing console. The deploy script can set a one-dollar budget alarm, which sends an email
if the account's spending ever passes one dollar in a month.

## Before deploying

The deployment goes through four stages:

1. Deploy the server. The deploy script creates a CloudFormation stack and prints a setup code, a
   random secret that creates the admin account. The setup code is shown once; the operator keeps the
   setup code until the admin account exists.
2. Reach the scoreboard over HTTPS. Lambda's function URL provides an HTTPS address, so nothing more is
   needed. A custom domain is optional; see [Operating a scoreboard on AWS](#operating-a-scoreboard-on-aws).
3. Create the admin account. The operator opens the scoreboard's address followed by `/admin/setup`,
   enters the setup code, names the scoreboard, and chooses the admin's username and password. The
   portal's Account page then offers to add a passkey, so the admin can sign in without the password.
4. Check the scoreboard. The `check` subcommand, run from any computer, joins the scoreboard as a
   temporary member, exercises each part of the API, and leaves again.

### Secrets stored outside the data

Three values live in the Lambda function's configuration and never in the DynamoDB table.

| Secret | Where the secret lives on AWS |
|---|---|
| The setup code | The deploy script generates the code on the first deploy and passes the code to the stack as a `NoEcho` parameter, which sets `SCOREBOARD_SETUP_CODE` on the function. The scoreboard's log never prints a configured code; the log names the variable |
| The pepper | A random secret mixed into all password hashes, so a copy of the table alone cannot be used to guess the admin's password. The deploy script generates the pepper on the first deploy and passes the pepper to the stack as a `NoEcho` parameter, which sets `SCOREBOARD_PEPPER` on the function. The pepper never changes once set: changing the pepper invalidates the admin's password and the scoreboard's join password |
| The recovery code | Set only while the admin is locked out, as the environment variable `SCOREBOARD_RECOVERY_CODE` on the function. See [Admin recovery](../README.md#admin-recovery) |

### Configuration reference

The stack sets these environment variables on the function. The reference is here for an operator who
changes a value by hand in the Lambda console.

| Variable | Meaning | Set by |
|---|---|---|
| `SCOREBOARD_DYNAMODB_TABLE` | The DynamoDB table that stores the scoreboard | The stack |
| `AWS_REGION` | The table's region | Lambda |
| `SCOREBOARD_PUBLIC_URL` | The scoreboard's address when a custom domain is in front of the function URL. Empty uses the function URL | The stack's `PublicURL` parameter |
| `SCOREBOARD_SETUP_CODE` | The setup code, at least 16 characters | The stack's `SetupCode` parameter |
| `SCOREBOARD_PEPPER` | The pepper, at least 16 characters | The stack's `Pepper` parameter |
| `SCOREBOARD_RECOVERY_CODE` | A recovery code, at least 16 characters, while the admin is locked out | The operator |

On Lambda the server always uses DynamoDB and always serves behind the function URL, so the store,
listen address, TLS mode, and trusted-proxy settings that other hosts need do not apply. The `check`
subcommand, run on the operator's computer, reads the scoreboard's join password from
`SCOREBOARD_CHECK_PASSWORD`, so the password never appears in the process list.

## Step 1: Open CloudShell

The operator signs in to the AWS console, chooses the region nearest the scoreboard's members in the
menu at the top right, and opens **CloudShell** from the toolbar. CloudShell is a terminal in the
browser, already signed in to the account.

## Step 2: Deploy

In CloudShell:

```bash
curl -fsSLO https://github.com/jacobjohnston/FitHalo-Scoreboards/releases/latest/download/deploy-aws.sh
sh deploy-aws.sh fithalo-scoreboard operator@example.com
```

The first argument names the scoreboard's CloudFormation stack; the second is the email address the
budget alarm goes to. The script:

1. downloads the latest release and checks its checksums, and also checks its build attestations when
   the `gh` CLI is installed and signed in, reporting the attestation check as skipped otherwise,
2. puts the scoreboard's code in a temporary bucket in the account,
3. creates the stack: the function, the function URL, the table, the schedule, the log group, and the
   alarm,
4. deletes the temporary bucket, since Lambda keeps a copy of the code, and
5. prints the scoreboard's address and the setup code.

The operator keeps the setup code until step 3. The script also makes a random pepper, stored in the
function's configuration and never in the table.

## Step 3: Create the admin account

The operator opens the scoreboard's address followed by `/admin/setup`, enters the setup code, names the
scoreboard, and chooses a username and password. The portal's Account page then offers to add a passkey.

The address looks like `https://abcdefghijklmnop.lambda-url.us-east-1.on.aws`. Members never type the
address: members join from the invitation link or the link's QR code.

## Step 4: Check the scoreboard

From any computer, the operator downloads the release for that computer from the project's releases
page, unpacks the release, and runs:

```bash
./fithalo-scoreboard check -url https://<scoreboard> -invite <invite code>
```

The invite code is the last part of the invitation link in the portal. On a scoreboard that approves new
members, the check runs the checks that do not need an approved member and says so; the admin approves
the temporary member and runs the check again for the rest.

## Operating a scoreboard on AWS

| Task | How |
|---|---|
| Updating | Run `sh deploy-aws.sh fithalo-scoreboard` again. The script keeps the setup code, the pepper, and the data |
| Deploying a local build | Run `./release <version>` on a computer with the source, upload `dist/fithalo-scoreboard-aws-upload.zip` to CloudShell, and run `unzip fithalo-scoreboard-aws-upload.zip && RELEASE_DIR=. sh deploy-aws.sh fithalo-scoreboard`. The bundle contains `deploy-aws.sh`, `fithalo-scoreboard-lambda-arm64.zip`, `aws-template.yaml`, and `SHA256SUMS`. The script checks the checksums of those files and skips the attestation check, since a local build has no attestation |
| Logs | The CloudWatch log group `/aws/lambda/fithalo-scoreboard` |
| Backups | DynamoDB's on-demand backups, from the table's page in the console. AWS charges for backups by size, which for a scoreboard is a fraction of a cent |
| Point-in-time recovery | The template leaves point-in-time recovery off, because DynamoDB charges for the feature by the table's size each month. An operator who wants to restore the table to any second of the last 35 days turns the feature on from the table's **Backups** tab |
| Reserved concurrency | The template reserves none. Lambda keeps at least 100 of an account's concurrent executions unreserved, and a new account's quota is often 10, so a reservation fails there. On an account with a higher quota, a small reservation such as 5, set on the function's **Configuration** tab, caps how many copies run at once, which caps the cost of a flood of requests and keeps the per-copy rate limits close to their intended values |
| Locked out | In the Lambda console, add an environment variable `SCOREBOARD_RECOVERY_CODE` with a long random value of 16 characters or more, for example the output of `head -c 24 /dev/urandom \| base64`. Enter the value at `/admin/recover`, then remove the variable. Each value works once |
| A custom domain | Put CloudFront in front of the function URL, and run the script with `PUBLIC_URL=https://scoreboard.example.com`, which sets the stack's `PublicURL` parameter so invitation links and passkeys use the domain. Later runs keep the parameter. See [Behind CloudFront](#behind-cloudfront) |
| Removing the scoreboard | Delete the stack. The table is kept, so the scoreboard's data is not lost by accident; the operator deletes the table from the DynamoDB console when sure |

## Limits on Lambda

Each running copy of the function keeps its own rate limits in memory, so the limits are looser on Lambda
than on a single server. Nonces live in the table, so a replayed request is refused whichever copy the
request reaches. The [README](../README.md#rate-limits) lists the limits.

## Behind CloudFront

With CloudFront in front of the function URL, the scoreboard sees the address of the CloudFront edge
server that forwarded each request, not the member's own address. The rate limits therefore apply per
edge address: members who reach the same edge share one limit, and a client whose requests spread across
edges is limited less than on a direct connection.

The function URL also stays reachable directly, so a request can bypass CloudFront and anything set on
CloudFront, such as AWS WAF rules or geographic restrictions. CloudFront's origin access control can close
the function URL to everything but CloudFront, but origin access control requires the function URL's
authentication type to be `AWS_IAM` and requires each request with a body to carry a hash of that body,
which neither the FitHalo app nor a browser using the admin portal sends. The template therefore keeps the
function URL public.
