@description('Resource ID of the custom policy definition to assign.')
param definitionId string

@description('Name of the policy assignment.')
param assignmentName string

@description('Effect parameter value to pass to the policy definition.')
param effect string

resource assignment 'Microsoft.Authorization/policyAssignments@2024-04-01' = {
  name: assignmentName
  properties: {
    displayName: 'TEST PR17843186 ARO HCP CMK'
    policyDefinitionId: definitionId
    enforcementMode: 'Default'
    parameters: {
      effect: {
        value: effect
      }
    }
  }
}
