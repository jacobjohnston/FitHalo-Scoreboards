#!/bin/sh
#
# Deploys or updates a FitHalo Scoreboard in your AWS account, from AWS CloudShell.
#
#   sh deploy-aws.sh                     a new board, or the latest release on an existing one
#   sh deploy-aws.sh my-board you@example.com
#   RELEASE_DIR=. sh deploy-aws.sh       the release files in that directory, instead of the latest release
#   PUBLIC_URL=https://board.example.com sh deploy-aws.sh    the board's own domain, in front of the function URL
#
# The first argument names the stack; the second, on a first deploy, receives a budget alert if the
# account's spending ever passes one dollar in a month.
set -eu

readonly REPOSITORY='jacobjohnston/FitHalo-Scoreboards'
readonly RELEASES="https://github.com/${REPOSITORY}/releases/latest/download"

stack="${1:-fithalo-scoreboard}"
email="${2:-}"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

# verify_attestations checks the named files in work against the release workflow's provenance, if gh can.
verify_attestations() {
  if ! command -v gh >/dev/null 2>&1 || ! gh auth status >/dev/null 2>&1 \
    || ! gh attestation --help >/dev/null 2>&1; then
    echo "Skipped the attestation check: the gh CLI is not installed or not signed in."
    return 0
  fi
  for asset in "$@"; do
    if ! gh attestation verify "${work}/${asset}" --repo "${REPOSITORY}" \
      --signer-workflow "${REPOSITORY}/.github/workflows/release.yml" >/dev/null; then
      echo "The attestation of ${asset} does not match the release workflow." >&2
      exit 1
    fi
  done
  echo "Checked the attestations of $*."
}

if [ -n "${RELEASE_DIR:-}" ]; then
  echo "Using the release in ${RELEASE_DIR}."
  for asset in fithalo-scoreboard-lambda-arm64.zip aws-template.yaml SHA256SUMS; do
    cp "${RELEASE_DIR}/${asset}" "${work}/${asset}"
  done
else
  echo "Downloading the latest release."
  for asset in fithalo-scoreboard-lambda-arm64.zip aws-template.yaml SHA256SUMS; do
    curl -fsSL -o "${work}/${asset}" "${RELEASES}/${asset}"
  done
fi
(cd "${work}" \
  && grep -E ' \./(fithalo-scoreboard-lambda-arm64\.zip|aws-template\.yaml)$' SHA256SUMS \
  | sha256sum -c -)
if [ -n "${RELEASE_DIR:-}" ]; then
  echo "Skipped the attestation check: the files in ${RELEASE_DIR} may be a local build."
else
  verify_attestations fithalo-scoreboard-lambda-arm64.zip aws-template.yaml SHA256SUMS
fi

region="${AWS_REGION:-$(aws configure get region)}"
account="$(aws sts get-caller-identity --query Account --output text)"
bucket="$(printf '%s-code-%s-%s' "${stack}" "${account}" "$(date +%s)" \
  | tr 'A-Z' 'a-z' | cut -c1-63)"
key="fithalo-scoreboard-lambda-arm64-$(date +%s).zip"

echo "Uploading the code to a temporary bucket, ${bucket}."
aws s3 mb "s3://${bucket}" --region "${region}" >/dev/null
aws s3 cp "${work}/fithalo-scoreboard-lambda-arm64.zip" "s3://${bucket}/${key}" >/dev/null

overrides="CodeBucket=${bucket} CodeKey=${key}"
if [ -n "${PUBLIC_URL:-}" ]; then
  overrides="${overrides} PublicURL=${PUBLIC_URL}"
fi
first_deploy=false
if ! aws cloudformation describe-stacks --stack-name "${stack}" --region "${region}" \
  >/dev/null 2>&1; then
  first_deploy=true
  setup_code="$(head -c 18 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=')"
  pepper="$(head -c 32 /dev/urandom | base64 | tr -d '=')"
  overrides="${overrides} SetupCode=${setup_code} Pepper=${pepper} BudgetEmail=${email}"
fi

echo "Deploying the stack ${stack} in ${region}."
# The overrides are split into words on purpose, one per parameter.
# shellcheck disable=SC2086
aws cloudformation deploy --region "${region}" --stack-name "${stack}" \
  --template-file "${work}/aws-template.yaml" --capabilities CAPABILITY_IAM \
  --parameter-overrides ${overrides} --no-fail-on-empty-changeset
aws s3 rb "s3://${bucket}" --force --region "${region}" >/dev/null

url="$(aws cloudformation describe-stacks --stack-name "${stack}" --region "${region}" \
  --query "Stacks[0].Outputs[?OutputKey=='BoardURL'].OutputValue" --output text)"
url="${url%/}"
echo
echo "The board is at ${url}"
if [ "${first_deploy}" = true ]; then
  echo "Create its admin at ${url}/admin/setup with the setup code ${setup_code}"
  echo "Keep that code until you have: it is not shown again."
fi
