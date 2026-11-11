// Policy source: PR 17843186, commit db7d8634ece1215b98ecfaa47d2e0b884163128c.
// https://msazure.visualstudio.com/One/_git/mgmt-Governance-Policy/pullrequest/17843186
//
// Deny-only test: Azure Policy with effect=Deny is evaluated by Resource Manager before a
// request ever reaches the ARO HCP resource provider, so proving the deny behavior does not
// require any real cluster/node-pool infrastructure (VNets, identities, Key Vault). This
// template only creates the test resource group, the policy definition, and a Deny-effect
// assignment scoped to that resource group. cmk_policy_test.sh fires skeleton `az rest put`
// requests against that resource group and checks for RequestDisallowedByPolicy.
targetScope = 'subscription'

@description('Unique identifier for this disposable test run. Never reuse an unrelated existing resource group.')
@minLength(3)
@maxLength(16)
param testRunName string

@description('ARO HCP-supported Azure region.')
param location string

var definitionName = 'hcp-cmk-test-${uniqueString(subscription().id, testRunName)}'
var assignmentName = 'hcp-cmk-test'
var resourceGroupName = 'rg-hcp-cmk-test-${testRunName}'
var tags = {
  purpose: 'pr17843186-policy-test'
  testRun: testRunName
}

resource testGroup 'Microsoft.Resources/resourceGroups@2024-03-01' = {
  name: resourceGroupName
  location: location
  tags: tags
}

resource policy 'Microsoft.Authorization/policyDefinitions@2023-04-01' = {
  name: definitionName
  properties: {
    displayName: 'TEST PR17843186 ARO HCP CMK (${testRunName})'
    description: 'Use customer-managed keys to manage encryption at rest for Azure Red Hat OpenShift hosted control plane clusters and their node pools. Clusters must encrypt etcd data using a Key Management Service (KMS) key that you control in Azure Key Vault, and node pools must encrypt OS disks using a disk encryption set that you control, including key rotation and management. Customer-managed keys cannot be enabled on existing clusters or node pools; non-compliant resources must be recreated to comply.'
    policyType: 'Custom'
    mode: 'Indexed'
    metadata: {
      category: 'Sovereignty'
      version: '1.0.0-preview'
      preview: true
      sourcePullRequest: '17843186'
      sourceCommit: 'db7d8634ece1215b98ecfaa47d2e0b884163128c'
    }
    parameters: {
      effect: {
        type: 'String'
        metadata: {
          displayName: 'Effect'
          description: 'Enable or disable the execution of the policy'
        }
        allowedValues: [
          'Audit'
          'Deny'
          'Disabled'
        ]
        defaultValue: 'Audit'
      }
    }
    policyRule: {
      if: {
        anyOf: [
          {
            allOf: [
              {
                field: 'type'
                equals: 'Microsoft.RedHatOpenShift/hcpOpenShiftClusters'
              }
              {
                anyOf: [
                  {
                    field: 'Microsoft.RedHatOpenShift/hcpOpenShiftClusters/etcd.dataEncryption.keyManagementMode'
                    notEquals: 'CustomerManaged'
                  }
                  {
                    field: 'Microsoft.RedHatOpenShift/hcpOpenShiftClusters/etcd.dataEncryption.customerManaged.kms.activeKey.name'
                    exists: false
                  }
                  {
                    field: 'Microsoft.RedHatOpenShift/hcpOpenShiftClusters/etcd.dataEncryption.customerManaged.kms.activeKey.name'
                    equals: ''
                  }
                ]
              }
            ]
          }
          {
            allOf: [
              {
                field: 'type'
                equals: 'Microsoft.RedHatOpenShift/hcpOpenShiftClusters/nodePools'
              }
              {
                anyOf: [
                  {
                    field: 'Microsoft.RedHatOpenShift/hcpOpenShiftClusters/nodePools/platform.osDisk.encryptionSetId'
                    exists: false
                  }
                  {
                    field: 'Microsoft.RedHatOpenShift/hcpOpenShiftClusters/nodePools/platform.osDisk.encryptionSetId'
                    equals: ''
                  }
                ]
              }
            ]
          }
        ]
      }
      then: {
        effect: '[parameters(\'effect\')]'
      }
    }
  }
}

module assignment 'cmk-policy-assignment.bicep' = {
  name: 'hcp-cmk-assignment-${testRunName}'
  scope: resourceGroup(testGroup.name)
  params: {
    definitionId: policy.id
    assignmentName: assignmentName
    effect: 'Deny'
  }
}

output policyDefinitionId string = policy.id
output resourceGroupName string = testGroup.name
output assignmentId string = '${testGroup.id}/providers/Microsoft.Authorization/policyAssignments/${assignmentName}'
