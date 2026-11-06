#!/bin/sh
#
# Deploys a FitHalo Scoreboard to Azure, or updates one, from Azure Cloud Shell.
#
#   sh deploy-azure.sh                                      a new board, or the latest image on one
#   sh deploy-azure.sh my-board westeurope you@example.com
#
# The arguments are the resource group, its region on a first deploy, and where a budget alert goes if
# the group's spending ever passes one dollar in a month.
set -eu

readonly APP='fithalo-scoreboard'
readonly REPOSITORY='jacobjohnston/FitHalo-Scoreboards'
readonly RELEASES="https://github.com/${REPOSITORY}/releases/latest/download"

group="${1:-fithalo-scoreboard}"
location="${2:-eastus}"
email="${3:-}"
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

echo "Downloading the latest release."
for asset in azure-template.bicep SHA256SUMS; do
  curl -fsSL -o "${work}/${asset}" "${RELEASES}/${asset}"
done
(cd "${work}" && grep -E ' \./azure-template\.bicep$' SHA256SUMS | sha256sum -c -)
verify_attestations azure-template.bicep SHA256SUMS

# The release's template names the image it was built with, by digest.
image="$(sed -n "s/^param image string = '\([^']*\)'\$/\1/p" "${work}/azure-template.bicep")"
if [ -z "${image}" ]; then
  echo "The release's template names no image." >&2
  exit 1
fi

if az containerapp show --resource-group "${group}" --name "${APP}" >/dev/null 2>&1; then
  # Deploying the template again would make a new pepper and setup code and lock out the admin.
  echo "Updating ${APP} to ${image}."
  az containerapp update --resource-group "${group}" --name "${APP}" --image "${image}" \
    --revision-suffix "update$(date +%s)" >/dev/null
  url="$(az containerapp show --resource-group "${group}" --name "${APP}" \
    --query properties.configuration.ingress.fqdn --output tsv)"
  echo "Updated. The board is at https://${url}"
  exit 0
fi

# The setup code goes in through a file readable only by this user, and is a secure parameter, so it
# stays out of the process list and the deployment's history.
setup_code="$(head -c 18 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=')"
(umask 077 && cat > "${work}/secrets.json" <<PARAMETERS
{
  "\$schema": "https://schema.management.azure.com/schemas/2019-04-01/deploymentParameters.json#",
  "contentVersion": "1.0.0.0",
  "parameters": {
    "setupCode": {"value": "${setup_code}"}
  }
}
PARAMETERS
)

echo "Creating the resource group ${group} in ${location}."
az group create --name "${group}" --location "${location}" >/dev/null
echo "Deploying the board. Cosmos DB takes a few minutes."
url="$(az deployment group create --resource-group "${group}" \
  --template-file "${work}/azure-template.bicep" \
  --parameters "@${work}/secrets.json" \
  --parameters appName="${APP}" budgetEmail="${email}" \
  --query properties.outputs.boardUrl.value --output tsv)"
echo
echo "The board is at ${url}"
echo "Create its admin at ${url}/admin/setup with the setup code ${setup_code}"
echo "Keep that code until you have: it is not shown again."
