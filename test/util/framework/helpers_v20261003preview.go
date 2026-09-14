// Copyright 2026 Microsoft Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package framework

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"embed"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/go-logr/logr"

	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"

	"github.com/Azure/ARO-HCP/internal/api/coreapi"
	"github.com/Azure/ARO-HCP/internal/api/metadataapi"
	hcpsdk20261003preview "github.com/Azure/ARO-HCP/test/sdk/v20261003preview/resourcemanager/redhatopenshifthcp/armredhatopenshifthcp"
)

type ClusterParams20261003 struct {
	OpenshiftVersionId            string
	ClusterName                   string
	ManagedResourceGroupName      string
	NsgResourceID                 string
	NsgName                       string
	SubnetResourceID              string
	SubnetName                    string
	VnetName                      string
	UserAssignedIdentitiesProfile *hcpsdk20261003preview.UserAssignedIdentitiesProfile
	Identity                      *hcpsdk20261003preview.ManagedServiceIdentity
	// KeyEncryptionKeyURL is the full key URL, e.g. https://vault.vault.azure.net/keys/key/version
	// or https://hsm.managedhsm.azure.net/keys/key/version for Managed HSM.
	// PopulateClusterParamsFromCustomerInfraDeployment20261003 sets this from bicep outputs
	// using the standard .vault.azure.net suffix; override afterward for mHSM testing.
	KeyEncryptionKeyURL         string
	EncryptionKeyManagementMode string
	EncryptionType              string
	VnetIntegrationSubnetID     string
	KeyVaultVisibility          string
	IngressType                 string
	Network                     NetworkConfig
	APIVisibility               string
	ImageRegistryState          string
	ChannelGroup                string
	AuthorizedCIDRs             []*string
	Autoscaling                 *hcpsdk20261003preview.ClusterAutoscalingProfile
	CryptoRestrictions          *hcpsdk20261003preview.CryptoRestrictions
	Tags                        map[string]*string
}

func NewDefaultClusterParams20261003() ClusterParams20261003 {
	params := ClusterParams20261003{
		OpenshiftVersionId: DefaultOpenshiftControlPlaneVersionId(),
		Network: NetworkConfig{
			NetworkType: "OVNKubernetes",
			PodCIDR:     DefaultPodCIDR,
			ServiceCIDR: DefaultServiceCIDR,
			MachineCIDR: "10.0.0.0/16",
			HostPrefix:  23,
		},
		EncryptionKeyManagementMode: "CustomerManaged",
		EncryptionType:              "KMS",
		KeyVaultVisibility:          "Public",
		IngressType:                 "Public",
		APIVisibility:               "Public",
		ImageRegistryState:          "Enabled",
		ChannelGroup:                DefaultOpenshiftChannelGroup(),
		Tags: map[string]*string{
			metadataapi.TagClusterSizeOverride:        to.Ptr(string(coreapi.MinimalControlPlanePodSizing)),
			metadataapi.TagClusterMaxCreationDuration: to.Ptr((ClusterCreationTimeout - time.Minute).String()),
			metadataapi.TagClusterMaxDeletionDuration: to.Ptr((HCPClusterDeletionTimeout - time.Minute).String()),
		},
	}
	applyCPOImageOverride(params.Tags)
	return params
}

func ConvertToUserAssignedIdentitiesProfile20261003(value interface{}) (*hcpsdk20261003preview.UserAssignedIdentitiesProfile, error) {
	if value == nil {
		return nil, nil
	}
	b, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal UserAssignedIdentitiesValue: %w", err)
	}
	var uamis hcpsdk20261003preview.UserAssignedIdentitiesProfile
	if err := json.Unmarshal(b, &uamis); err != nil {
		return nil, fmt.Errorf("failed to unmarshal UserAssignedIdentitiesValue: %w", err)
	}
	return &uamis, nil
}

func ConvertToManagedServiceIdentity20261003(value interface{}) (*hcpsdk20261003preview.ManagedServiceIdentity, error) {
	if value == nil {
		return nil, nil
	}
	b, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal IdentityValue: %w", err)
	}
	var msi hcpsdk20261003preview.ManagedServiceIdentity
	if err := json.Unmarshal(b, &msi); err != nil {
		return nil, fmt.Errorf("failed to unmarshal IdentityValue: %w", err)
	}
	return &msi, nil
}

func PopulateClusterParamsFromCustomerInfraDeployment20261003(params ClusterParams20261003, customerInfraDeploymentResult *armresources.DeploymentExtended) (ClusterParams20261003, error) {
	if customerInfraDeploymentResult == nil {
		return params, fmt.Errorf("customerInfraDeploymentResult cannot be nil")
	}

	keyVaultName, err := GetOutputValueString(customerInfraDeploymentResult, "keyVaultName")
	if err != nil {
		return params, fmt.Errorf("failed to get keyVaultName from customer infra deployment: %w", err)
	}
	etcdEncryptionKeyVersion, err := GetOutputValueString(customerInfraDeploymentResult, "etcdEncryptionKeyVersion")
	if err != nil {
		return params, fmt.Errorf("failed to get etcdEncryptionKeyVersion from customer infra deployment: %w", err)
	}
	etcdEncryptionKeyName, err := GetOutputValueString(customerInfraDeploymentResult, "etcdEncryptionKeyName")
	if err != nil {
		return params, fmt.Errorf("failed to get etcdEncryptionKeyName from customer infra deployment: %w", err)
	}
	nsgResourceID, err := GetOutputValueString(customerInfraDeploymentResult, "nsgID")
	if err != nil {
		return params, fmt.Errorf("failed to get nsgID from customer infra deployment: %w", err)
	}
	subnetResourceID, err := GetOutputValueString(customerInfraDeploymentResult, "vnetSubnetID")
	if err != nil {
		return params, fmt.Errorf("failed to get vnetSubnetID from customer infra deployment: %w", err)
	}
	vnetIntegrationSubnetID, err := GetOutputValueString(customerInfraDeploymentResult, "vnetIntegrationSubnetID")
	if err != nil {
		return params, fmt.Errorf("failed to get vnetIntegrationSubnetID from customer infra deployment: %w", err)
	}
	vnetName, err := GetOutputValueString(customerInfraDeploymentResult, "vnetName")
	if err != nil {
		return params, fmt.Errorf("failed to get vnetName from customer infra deployment: %w", err)
	}
	nsgName, err := GetOutputValueString(customerInfraDeploymentResult, "nsgName")
	if err != nil {
		return params, fmt.Errorf("failed to get nsgName from customer infra deployment: %w", err)
	}
	subnetName, err := GetOutputValueString(customerInfraDeploymentResult, "vnetSubnetName")
	if err != nil {
		return params, fmt.Errorf("failed to get vnetSubnetName from customer infra deployment: %w", err)
	}

	params.KeyEncryptionKeyURL = fmt.Sprintf("https://%s.vault.azure.net/keys/%s/%s", keyVaultName, etcdEncryptionKeyName, etcdEncryptionKeyVersion)
	params.NsgResourceID = nsgResourceID
	params.SubnetResourceID = subnetResourceID
	params.VnetIntegrationSubnetID = vnetIntegrationSubnetID
	params.VnetName = vnetName
	params.NsgName = nsgName
	params.SubnetName = subnetName
	return params, nil
}

func PopulateClusterParamsFromManagedIdentitiesDeployment20261003(params ClusterParams20261003, managedIdentitiesDeploymentResult *armresources.DeploymentExtended) (ClusterParams20261003, error) {
	if managedIdentitiesDeploymentResult == nil {
		return params, fmt.Errorf("managedIdentitiesDeploymentResult cannot be nil")
	}

	userAssignedIdentities, err := GetOutputValue(managedIdentitiesDeploymentResult, "userAssignedIdentitiesValue")
	if err != nil {
		return params, fmt.Errorf("failed to get userAssignedIdentitiesValue from managed identity deployment: %w", err)
	}
	userAssignedIdentitiesProfile, err := ConvertToUserAssignedIdentitiesProfile20261003(userAssignedIdentities)
	if err != nil {
		return params, fmt.Errorf("failed to convert userAssignedIdentitiesValue: %w", err)
	}

	identityValue, err := GetOutputValue(managedIdentitiesDeploymentResult, "identityValue")
	if err != nil {
		return params, fmt.Errorf("failed to get identityValue from managed identity deployment: %w", err)
	}
	identityProfile, err := ConvertToManagedServiceIdentity20261003(identityValue)
	if err != nil {
		return params, fmt.Errorf("failed to convert identityValue: %w", err)
	}

	params.UserAssignedIdentitiesProfile = userAssignedIdentitiesProfile
	params.Identity = identityProfile
	return params, nil
}

func (tc *perItOrDescribeTestContext) CreateClusterCustomerResources20261003(ctx context.Context, resourceGroup *armresources.ResourceGroup, clusterParams ClusterParams20261003, infraParameters map[string]interface{}, artifactsFS embed.FS, rbacScope RBACScope) (ClusterParams20261003, error) {
	startTime := time.Now()
	defer func() {
		tc.RecordTestStep(fmt.Sprintf("Deploy customer resources in resource group %s", *resourceGroup.Name), startTime, time.Now())
	}()

	randomSuffix := utilrand.String(6)
	customerInfraDeploymentName := fmt.Sprintf("customer-infra-%s-%s", clusterParams.ClusterName, randomSuffix)
	managedIdentitiesDeploymentName := fmt.Sprintf("mi-%s-%s", clusterParams.ClusterName, randomSuffix)

	infraParameters["clusterName"] = clusterParams.ClusterName

	customerInfraDeploymentResult, err := tc.CreateBicepTemplateAndWait(ctx,
		WithTemplateFromFS(artifactsFS, "test-artifacts/generated-test-artifacts/modules/customer-infra.json"),
		WithDeploymentName(customerInfraDeploymentName),
		WithScope(BicepDeploymentScopeResourceGroup),
		WithClusterResourceGroup(*resourceGroup.Name),
		WithParameters(infraParameters),
		WithTimeout(45*time.Minute),
	)
	if err != nil {
		return clusterParams, fmt.Errorf("failed to create customer-infra: %w", err)
	}
	clusterParams, err = PopulateClusterParamsFromCustomerInfraDeployment20261003(clusterParams, customerInfraDeploymentResult)
	if err != nil {
		return clusterParams, fmt.Errorf("failed to populate cluster params from customer-infra: %w", err)
	}

	managedIdentityDeploymentResult, err := tc.DeployManagedIdentities(ctx,
		clusterParams.ClusterName,
		rbacScope,
		WithTemplateFromFS(artifactsFS, "test-artifacts/generated-test-artifacts/modules/managed-identities.json"),
		WithDeploymentName(managedIdentitiesDeploymentName),
		WithClusterResourceGroup(*resourceGroup.Name),
		WithParameters(map[string]interface{}{
			"nsgName":      clusterParams.NsgName,
			"vnetName":     clusterParams.VnetName,
			"subnetName":   clusterParams.SubnetName,
			"keyVaultName": extractVaultName(clusterParams.KeyEncryptionKeyURL),
		}),
	)
	if err != nil {
		return clusterParams, fmt.Errorf("failed to create managed identities: %w", err)
	}
	clusterParams, err = PopulateClusterParamsFromManagedIdentitiesDeployment20261003(clusterParams, managedIdentityDeploymentResult)
	if err != nil {
		return clusterParams, fmt.Errorf("failed to populate cluster params from managed identities: %w", err)
	}
	return clusterParams, nil
}

// extractVaultName returns the subdomain of the host in keyURL, e.g. "myvault" from
// "https://myvault.vault.azure.net/keys/key/ver".
func extractVaultName(keyURL string) string {
	for i := range len(keyURL) {
		if keyURL[i] == '/' {
			break
		}
	}
	// strip scheme
	s := keyURL
	if len(s) > 8 && s[:8] == "https://" {
		s = s[8:]
	}
	for i, c := range s {
		if c == '.' {
			return s[:i]
		}
	}
	return s
}

func (tc *perItOrDescribeTestContext) CreateHCPClusterFromParam20261003(ctx context.Context, logger logr.Logger, resourceGroupName string, parameters ClusterParams20261003, imageDigestMirrors []*hcpsdk20261003preview.ImageDigestMirror, timeout time.Duration) error {
	if timeout > 0*time.Second {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, timeout, fmt.Errorf("timeout '%f' minutes exceeded during CreateHCPCluster20261003FromParam for cluster %s in resource group %s", timeout.Minutes(), parameters.ClusterName, resourceGroupName))
		defer cancel()
	}

	startTime := time.Now()
	defer func() {
		tc.RecordTestStep(fmt.Sprintf("Deploy HCP cluster %s/%s (v20261003preview)", resourceGroupName, parameters.ClusterName), startTime, time.Now())
	}()

	cluster, err := BuildHCPClusterFromParams20261003(parameters, tc.Location(), imageDigestMirrors)
	if err != nil {
		return fmt.Errorf("failed to build HCP cluster %s: %w", parameters.ClusterName, err)
	}

	if _, err := CreateHCPClusterAndWait20261003(ctx, logger, tc.Get20261003ClientFactoryOrDie(ctx).NewHcpOpenShiftClustersClient(), resourceGroupName, parameters.ClusterName, cluster, timeout); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("failed to create HCP cluster %s, caused by: %w, error: %w", parameters.ClusterName, context.Cause(ctx), err)
		}
		return fmt.Errorf("failed to create HCP cluster %s: %w", parameters.ClusterName, err)
	}
	return nil
}

func BuildHCPClusterFromParams20261003(parameters ClusterParams20261003, location string, imageDigestMirrors []*hcpsdk20261003preview.ImageDigestMirror) (hcpsdk20261003preview.HcpOpenShiftCluster, error) {
	var identity *hcpsdk20261003preview.ManagedServiceIdentity
	if parameters.Identity != nil {
		var err error
		identity, err = convertViaJSON[hcpsdk20261003preview.ManagedServiceIdentity](parameters.Identity)
		if err != nil {
			return hcpsdk20261003preview.HcpOpenShiftCluster{}, fmt.Errorf("failed to convert Identity: %w", err)
		}
	}

	var uamis *hcpsdk20261003preview.UserAssignedIdentitiesProfile
	if parameters.UserAssignedIdentitiesProfile != nil {
		var err error
		uamis, err = convertViaJSON[hcpsdk20261003preview.UserAssignedIdentitiesProfile](parameters.UserAssignedIdentitiesProfile)
		if err != nil {
			return hcpsdk20261003preview.HcpOpenShiftCluster{}, fmt.Errorf("failed to convert UserAssignedIdentitiesProfile: %w", err)
		}
	}

	return hcpsdk20261003preview.HcpOpenShiftCluster{
		Location: to.Ptr(location),
		Identity: identity,
		Tags:     parameters.Tags,
		Properties: &hcpsdk20261003preview.HcpOpenShiftClusterProperties{
			Version: &hcpsdk20261003preview.VersionProfile{
				ID:           to.Ptr(parameters.OpenshiftVersionId),
				ChannelGroup: to.Ptr(parameters.ChannelGroup),
			},
			Platform: &hcpsdk20261003preview.PlatformProfile{
				ManagedResourceGroup:    to.Ptr(parameters.ManagedResourceGroupName),
				NetworkSecurityGroupID:  to.Ptr(parameters.NsgResourceID),
				SubnetID:                to.Ptr(parameters.SubnetResourceID),
				VnetIntegrationSubnetID: to.Ptr(parameters.VnetIntegrationSubnetID),
				OperatorsAuthentication: &hcpsdk20261003preview.OperatorsAuthenticationProfile{
					UserAssignedIdentities: uamis,
				},
			},
			Network: &hcpsdk20261003preview.NetworkProfile{
				NetworkType: to.Ptr(hcpsdk20261003preview.NetworkType(parameters.Network.NetworkType)),
				PodCIDR:     to.Ptr(parameters.Network.PodCIDR),
				ServiceCIDR: to.Ptr(parameters.Network.ServiceCIDR),
				MachineCIDR: to.Ptr(parameters.Network.MachineCIDR),
				HostPrefix:  to.Ptr(parameters.Network.HostPrefix),
			},
			API: &hcpsdk20261003preview.APIProfile{
				Visibility:      to.Ptr(hcpsdk20261003preview.Visibility(parameters.APIVisibility)),
				AuthorizedCIDRs: parameters.AuthorizedCIDRs,
			},
			Ingress: &hcpsdk20261003preview.IngressProfile{
				Type: to.Ptr(hcpsdk20261003preview.IngressType(parameters.IngressType)),
			},
			ClusterImageRegistry: &hcpsdk20261003preview.ClusterImageRegistryProfile{
				State: to.Ptr(hcpsdk20261003preview.ClusterImageRegistryState(parameters.ImageRegistryState)),
			},
			CryptoRestrictions: parameters.CryptoRestrictions,
			Etcd: &hcpsdk20261003preview.EtcdProfile{
				DataEncryption: &hcpsdk20261003preview.EtcdDataEncryptionProfile{
					KeyManagementMode: to.Ptr(hcpsdk20261003preview.EtcdDataEncryptionKeyManagementModeType(parameters.EncryptionKeyManagementMode)),
					CustomerManaged: &hcpsdk20261003preview.CustomerManagedEncryptionProfile{
						EncryptionType: to.Ptr(hcpsdk20261003preview.CustomerManagedEncryptionType(parameters.EncryptionType)),
						Kms: &hcpsdk20261003preview.KmsEncryptionProfile{
							KeyEncryptionKeyURL: to.Ptr(parameters.KeyEncryptionKeyURL),
							Visibility:          to.Ptr(hcpsdk20261003preview.KeyVaultVisibility(parameters.KeyVaultVisibility)),
						},
					},
				},
			},
			ImageDigestMirrors: imageDigestMirrors,
		},
	}, nil
}

func CreateHCPClusterAndWait20261003(ctx context.Context, logger logr.Logger, hcpClient *hcpsdk20261003preview.HcpOpenShiftClustersClient, resourceGroupName string, hcpClusterName string, cluster hcpsdk20261003preview.HcpOpenShiftCluster, timeout time.Duration) (*hcpsdk20261003preview.HcpOpenShiftCluster, error) {
	if timeout > 0*time.Second {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, timeout, fmt.Errorf("timeout '%f' minutes exceeded during CreateHCPCluster20261003AndWait for cluster %s in resource group %s", timeout.Minutes(), hcpClusterName, resourceGroupName))
		defer cancel()
	}

	logger.Info("Starting HCP cluster creation (v20261003preview)", "clusterName", hcpClusterName, "resourceGroup", resourceGroupName)
	poller, err := hcpClient.BeginCreateOrUpdate(ctx, resourceGroupName, hcpClusterName, cluster, nil)
	if err != nil {
		return nil, fmt.Errorf("failed starting cluster creation %q in resourcegroup=%q: %w", hcpClusterName, resourceGroupName, err)
	}

	if timeout > 0*time.Second {
		operationResult, err := poller.PollUntilDone(ctx, &runtime.PollUntilDoneOptions{
			Frequency: StandardPollInterval,
		})
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, fmt.Errorf("failed waiting for cluster=%q in resourcegroup=%q to finish creating, caused by: %w, error: %w", hcpClusterName, resourceGroupName, context.Cause(ctx), err)
			}
			return nil, fmt.Errorf("failed waiting for cluster=%q in resourcegroup=%q to finish creating: %w", hcpClusterName, resourceGroupName, err)
		}
		switch m := any(operationResult).(type) {
		case hcpsdk20261003preview.HcpOpenShiftClustersClientCreateOrUpdateResponse:
			return &m.HcpOpenShiftCluster, nil
		default:
			fmt.Printf("unknown type %T: content=%v", m, spew.Sdump(m))
			return nil, fmt.Errorf("unknown type %T", m)
		}
	}

	_, err = poller.Poll(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed checking for deployment %q in resourcegroup=%q: %w", hcpClusterName, resourceGroupName, err)
	}
	return nil, nil
}

func GetHCPCluster20261003(ctx context.Context, hcpClient *hcpsdk20261003preview.HcpOpenShiftClustersClient, resourceGroupName string, hcpClusterName string) (*hcpsdk20261003preview.HcpOpenShiftCluster, error) {
	resp, err := hcpClient.Get(ctx, resourceGroupName, hcpClusterName, nil)
	if err != nil {
		return nil, err
	}
	return &resp.HcpOpenShiftCluster, nil
}

func (tc *perItOrDescribeTestContext) Get20261003ClientFactory(ctx context.Context) (*hcpsdk20261003preview.ClientFactory, error) {
	tc.contextLock.RLock()
	if tc.clientFactory20261003 != nil {
		defer tc.contextLock.RUnlock()
		return tc.clientFactory20261003, nil
	}
	tc.contextLock.RUnlock()

	tc.contextLock.Lock()
	defer tc.contextLock.Unlock()

	return tc.get20261003ClientFactoryUnlocked(ctx)
}

func (tc *perItOrDescribeTestContext) Get20261003ClientFactoryOrDie(ctx context.Context) *hcpsdk20261003preview.ClientFactory {
	return Must(tc.Get20261003ClientFactory(ctx))
}

func (tc *perItOrDescribeTestContext) get20261003ClientFactoryUnlocked(ctx context.Context) (*hcpsdk20261003preview.ClientFactory, error) {
	if tc.clientFactory20261003 != nil {
		return tc.clientFactory20261003, nil
	}

	creds, err := tc.perBinaryInvocationTestContext.getAzureCredentials()
	if err != nil {
		return nil, err
	}
	subscriptionID, err := tc.getSubscriptionIDUnlocked(ctx)
	if err != nil {
		return nil, err
	}
	clientFactory, err := hcpsdk20261003preview.NewClientFactory(subscriptionID, creds, tc.perBinaryInvocationTestContext.getHCPClientFactoryOptions())
	if err != nil {
		return nil, err
	}
	tc.clientFactory20261003 = clientFactory
	return tc.clientFactory20261003, nil
}

func (tc *perItOrDescribeTestContext) GetAdminRESTConfigForHCPCluster20261003(ctx context.Context, hcpClient *hcpsdk20261003preview.HcpOpenShiftClustersClient, resourceGroupName string, hcpClusterName string, timeout time.Duration) (*rest.Config, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, timeout, fmt.Errorf("timeout '%f' minutes exceeded during GetAdminRESTConfigForHCPCluster20261003 for cluster %s in resource group %s", timeout.Minutes(), hcpClusterName, resourceGroupName))
	defer cancel()

	startTime := time.Now()
	defer func() {
		tc.RecordTestStep("Collect admin credentials for cluster", startTime, time.Now())
	}()

	privKey, err := rsa.GenerateKey(cryptorand.Reader, 4096)
	if err != nil {
		return nil, fmt.Errorf("failed to generate RSA key: %w", err)
	}

	csrDER, err := x509.CreateCertificateRequest(cryptorand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:   "system:customer-break-glass:system-admin",
			Organization: []string{"system:masters"},
		},
	}, privKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create CSR: %w", err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	adminCredentialRequestPoller, err := hcpClient.BeginRequestAdminCredential(
		ctx,
		resourceGroupName,
		hcpClusterName,
		hcpsdk20261003preview.HcpOpenShiftClusterAdminCredentialRequest{
			CertificateSigningRequest: to.Ptr(string(csrPEM)),
		},
		nil,
	)
	if err != nil {
		fallbackFactory, fallbackErr := tc.Get20240610ClientFactory(ctx)
		if fallbackErr != nil {
			return nil, fmt.Errorf("1003 credential request failed: %w; fallback client factory error: %w", err, fallbackErr)
		}
		return tc.GetAdminRESTConfigForHCPCluster20240610(ctx, fallbackFactory.NewHcpOpenShiftClustersClient(), resourceGroupName, hcpClusterName, timeout)
	}

	operationResult, err := adminCredentialRequestPoller.PollUntilDone(ctx, &runtime.PollUntilDoneOptions{
		Frequency: StandardPollInterval,
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("failed waiting for hcpCluster=%q in resourcegroup=%q to finish getting creds, caused by: %w, error: %w", hcpClusterName, resourceGroupName, context.Cause(ctx), err)
		}
		return nil, fmt.Errorf("failed waiting for hcpCluster=%q in resourcegroup=%q to finish getting creds: %w", hcpClusterName, resourceGroupName, err)
	}

	if operationResult.Kubeconfig == nil {
		return nil, fmt.Errorf("kubeconfig content is nil")
	}

	privKeyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privKey),
	})

	kubeconfigData, err := clientcmd.Load([]byte(*operationResult.Kubeconfig))
	if err != nil {
		return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
	}
	for _, authInfo := range kubeconfigData.AuthInfos {
		authInfo.ClientKeyData = privKeyPEM
	}

	restConfig, err := clientcmd.NewDefaultClientConfig(*kubeconfigData, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, err
	}

	tc.contextLock.Lock()
	tc.hcpAdminConfigs[resourceGroupName+"/"+hcpClusterName] = restConfig
	tc.contextLock.Unlock()

	return restConfig, nil
}
