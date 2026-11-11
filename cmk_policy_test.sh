#!/usr/bin/env bash
set -euo pipefail
mode="$1"; sub="$2"; region="$3"
run='cmk17843186a'
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
[[ "$mode" == run || "$mode" == cleanup ]] || die 'Use run or cleanup.'
[[ "$run" =~ ^[A-Za-z0-9-]{3,16}$ ]] || die 'Invalid run ID.'
sub="$(az account show --subscription "$sub" --query id -o tsv)"
rg="rg-hcp-cmk-test-$run"
scope="/subscriptions/$sub/resourceGroups/$rg"
assignment="$scope/providers/Microsoft.Authorization/policyAssignments/hcp-cmk-test"
exists() {
    local answer
    answer="$(az group exists --subscription "$sub" --name "$1" -o tsv)" ||
        die "Cannot check $1."
    [[ "$answer" == true || "$answer" == false ]] || die "Unexpected response for $1."
    [[ "$answer" == true ]]
}

if [[ "$mode" == cleanup ]]; then
    if exists "$rg"; then
        purpose="$(az group show --subscription "$sub" -n "$rg" --query tags.purpose -o tsv)"
        owner="$(az group show --subscription "$sub" -n "$rg" --query tags.testRun -o tsv)"
        [[ "$purpose" == pr17843186-policy-test && "$owner" == "$run" ]] ||
            die "Ownership tags do not match for $rg."
        found="$(az policy assignment list --subscription "$sub" --scope "$scope" \
            --query "[?name=='hcp-cmk-test'].name" -o tsv)"
        if [[ -n "$found" ]]; then
            az policy assignment delete --subscription "$sub" --scope "$scope" -n hcp-cmk-test
        fi
        az group delete --subscription "$sub" -n "$rg" --yes
    fi
    definitions="$(az policy definition list --subscription "$sub" \
        --query "[?policyType=='Custom' && metadata.sourcePullRequest=='17843186' && displayName=='TEST PR17843186 ARO HCP CMK ($run)'].name" -o tsv)"
    while IFS= read -r definition; do
        [[ -n "$definition" ]] || continue
        az policy definition delete --subscription "$sub" -n "$definition"
    done <<< "$definitions"
    printf 'Cleanup complete: %s.\n' "$run"
    exit 0
fi

[[ -f ./cmk_policy_test.bicep ]] || die 'Run from the directory containing the Bicep file.'
if exists "$rg"; then
    purpose="$(az group show --subscription "$sub" -n "$rg" --query tags.purpose -o tsv)"
    owner="$(az group show --subscription "$sub" -n "$rg" --query tags.testRun -o tsv)"
    [[ "$purpose" == pr17843186-policy-test && "$owner" == "$run" ]] ||
        die "$rg already exists but was not created by this script; choose a fresh run ID or clean up."
    printf '%s already exists for this run; reusing it.\n' "$rg"
fi

logs="$(mktemp -d "${TMPDIR:-/tmp}/hcp-cmk-$run.XXXXXX")"
printf 'Run: %s\nEvidence: %s\n' "$run" "$logs"

az deployment sub create --subscription "$sub" --location "$region" \
    --name "cmk-$run-setup" --template-file ./cmk_policy_test.bicep \
    --parameters "testRunName=$run" "location=$region" \
    --only-show-errors -o none
printf 'Policy definition and Deny assignment deployed to %s.\n' "$rg"
printf 'Waiting two minutes for policy assignment propagation.\n'
sleep 120

api='2026-10-01'
miId="$scope/providers/Microsoft.ManagedIdentity/userAssignedIdentities/deny-test-mi"
vnetId="$scope/providers/Microsoft.Network/virtualNetworks/deny-test-vnet/subnets/workers"

cluster_body() {
    # $1 = etcd.dataEncryption object (JSON)
    cat <<JSON
{
  "location": "$region",
  "identity": {
    "type": "UserAssigned",
    "userAssignedIdentities": { "$miId": {} }
  },
  "properties": {
    "version": { "id": "0.0" },
    "dns": {},
    "console": {},
    "network": {
      "networkType": "OVNKubernetes",
      "podCidr": "10.128.0.0/14",
      "serviceCidr": "172.30.0.0/16",
      "machineCidr": "10.0.0.0/16",
      "hostPrefix": 23
    },
    "etcd": { "dataEncryption": $1 },
    "api": { "visibility": "Public" },
    "clusterImageRegistry": { "state": "Enabled" },
    "platform": {
      "managedResourceGroup": "$rg-deny-test-managed",
      "subnetId": "$vnetId",
      "outboundType": "LoadBalancer",
      "operatorsAuthentication": {
        "userAssignedIdentities": { "serviceManagedIdentity": "$miId" }
      }
    }
  }
}
JSON
}

nodepool_body() {
    # $1 = platform.osDisk object (JSON)
    cat <<JSON
{
  "location": "$region",
  "properties": {
    "version": { "id": "0.0.0" },
    "replicas": 1,
    "platform": {
      "vmSize": "Standard_D8s_v3",
      "subnetId": "$vnetId",
      "osDisk": $1
    }
  }
}
JSON
}

# expect_denied NAME URL BODY
expect_denied() {
    local name="$1" url="$2" body="$3"
    local log="$logs/$name.log"
    printf '%s' "$body" >"$logs/$name.request.json"
    printf 'Running %s\n' "$name"
    if az rest --subscription "$sub" --method put --url "$url" --body "@$logs/$name.request.json" \
        --headers 'Content-Type=application/json' -o json >"$log" 2>&1; then
        cat "$log" >&2
        die "$name unexpectedly succeeded; resources may have been created, clean up this run."
    fi
    if ! grep -Fqi RequestDisallowedByPolicy "$log" || ! grep -Fqi "$assignment" "$log"; then
        cat "$log" >&2
        die "$name did not prove denial by the test assignment; see $log."
    fi
    printf 'PASS %s: denied by policy assignment.\n' "$name"
}

expect_denied 'DenyClusterPlatformManaged' \
    "https://management.azure.com$scope/providers/Microsoft.RedHatOpenShift/hcpOpenShiftClusters/deny-test-cluster?api-version=$api" \
    "$(cluster_body '{"keyManagementMode":"PlatformManaged"}')"

expect_denied 'DenyClusterMissingActiveKey' \
    "https://management.azure.com$scope/providers/Microsoft.RedHatOpenShift/hcpOpenShiftClusters/deny-test-cluster?api-version=$api" \
    "$(cluster_body '{"keyManagementMode":"CustomerManaged","customerManaged":{"encryptionType":"KMS","kms":{"vaultName":"deny-test-kv","visibility":"Public"}}}')"

expect_denied 'DenyNodePoolMissingEncryptionSetId' \
    "https://management.azure.com$scope/providers/Microsoft.RedHatOpenShift/hcpOpenShiftClusters/deny-test-cluster/nodePools/deny-test-np?api-version=$api" \
    "$(nodepool_body '{"sizeGiB":64,"diskStorageAccountType":"StandardSSD_LRS"}')"

printf 'All deny probes passed. Policy correctly denies non-CMK clusters and node pools. Evidence: %s\n' "$logs"
printf 'Run "%s cleanup %s %s" to remove the test resource group and policy definition.\n' "$0" "$sub" "$region"
