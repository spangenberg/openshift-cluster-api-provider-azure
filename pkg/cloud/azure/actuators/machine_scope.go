/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package actuators

import (
	"context"
	"fmt"

	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/adal"
	"github.com/Azure/go-autorest/autorest/azure"
	clusterv1 "github.com/openshift/cluster-api/pkg/apis/cluster/v1alpha1"
	machinev1 "github.com/openshift/cluster-api/pkg/apis/machine/v1beta1"
	machineclient "github.com/openshift/cluster-api/pkg/client/clientset_generated/clientset/typed/machine/v1beta1"
	"github.com/pkg/errors"
	apicorev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog"
	"sigs.k8s.io/cluster-api-provider-azure/pkg/apis/azureprovider/v1beta1"
	controllerclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

const (
	// AzureCredsSubscriptionIDKey subcription ID
	AzureCredsSubscriptionIDKey = "azure_subscription_id"
	// AzureCredsClientIDKey client id
	AzureCredsClientIDKey = "azure_client_id"
	// AzureCredsClientSecretKey client secret
	AzureCredsClientSecretKey = "azure_client_secret"
	// AzureCredsTenantIDKey tenant ID
	AzureCredsTenantIDKey = "azure_tenant_id"
	// AzureCredsResourceGroupKey resource group
	AzureCredsResourceGroupKey = "azure_resourcegroup"
	// AzureCredsRegionKey region
	AzureCredsRegionKey = "azure_region"
	// AzureResourcePrefix resource prefix for created azure resources
	AzureResourcePrefix = "azure_resource_prefix"
)

// MachineScopeParams defines the input parameters used to create a new MachineScope.
type MachineScopeParams struct {
	AzureClients
	Cluster    *clusterv1.Cluster
	Machine    *machinev1.Machine
	Client     machineclient.MachineV1beta1Interface
	CoreClient controllerclient.Client
}

// NewMachineScope creates a new MachineScope from the supplied parameters.
// This is meant to be called for each machine actuator operation.
func NewMachineScope(params MachineScopeParams) (*MachineScope, error) {
	scope, err := NewScope(ScopeParams{AzureClients: params.AzureClients, Client: nil, Cluster: params.Cluster})
	if err != nil {
		return nil, err
	}

	machineConfig, err := MachineConfigFromProviderSpec(params.Client, params.Machine.Spec.ProviderSpec)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get machine config")
	}

	machineStatus, err := v1beta1.MachineStatusFromProviderStatus(params.Machine.Status.ProviderStatus)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get machine provider status")
	}

	machineClient := params.Client.Machines(params.Machine.Namespace)

	if machineConfig.CredentialsSecret != nil {
		if err = updateScope(params.CoreClient, machineConfig.CredentialsSecret, scope); err != nil {
			return nil, errors.Wrap(err, "failed to update cluster")
		}
	}

	scope.ClusterConfig.NetworkResourceGroup = scope.ClusterConfig.ResourceGroup

	if machineConfig.ResourceGroup != "" {
		scope.ClusterConfig.ResourceGroup = machineConfig.ResourceGroup
	}

	if machineConfig.NetworkResourceGroup != "" {
		scope.ClusterConfig.NetworkResourceGroup = machineConfig.NetworkResourceGroup
	}

	if machineConfig.Location != "" {
		scope.ClusterConfig.Location = machineConfig.Location
	}

	return &MachineScope{
		Scope: scope,
		// Deep copy the machine since it's change outside of the machine scope
		// by consumers of the machine scope (e.g. reconciler).
		Machine:       params.Machine.DeepCopy(),
		MachineClient: machineClient,
		CoreClient:    params.CoreClient,
		MachineConfig: machineConfig,
		MachineStatus: machineStatus,
		// Once set, they can not be changed. Otherwise, status change computation
		// might be invalid and result in skipping the status update.
		origMachine:       params.Machine.DeepCopy(),
		origMachineStatus: machineStatus.DeepCopy(),
	}, nil
}

// MachineScope defines a scope defined around a machine and its cluster.
type MachineScope struct {
	*Scope

	Machine       *machinev1.Machine
	MachineClient machineclient.MachineInterface
	CoreClient    controllerclient.Client
	MachineConfig *v1beta1.AzureMachineProviderSpec
	MachineStatus *v1beta1.AzureMachineProviderStatus

	// origMachine captures original value of machine before it is updated (to
	// skip object updated if nothing is changed)
	origMachine *machinev1.Machine
	// origMachineStatus captures original value of machine provider status
	// before it is updated (to skip object updated if nothing is changed)
	origMachineStatus *v1beta1.AzureMachineProviderStatus
}

// Name returns the machine name.
func (m *MachineScope) Name() string {
	return m.Machine.Name
}

// Namespace returns the machine namespace.
func (m *MachineScope) Namespace() string {
	return m.Machine.Namespace
}

// Role returns the machine role from the labels.
func (m *MachineScope) Role() string {
	return m.Machine.Labels[v1beta1.MachineRoleLabel]
}

// Location returns the machine location.
func (m *MachineScope) Location() string {
	return m.Scope.Location()
}

func (m *MachineScope) storeMachineSpec() error {
	ext, err := v1beta1.EncodeMachineSpec(m.MachineConfig)
	if err != nil {
		return err
	}

	m.Machine.Spec.ProviderSpec.Value = ext
	latestMachine, err := m.MachineClient.Update(m.Machine)
	if err != nil {
		return err
	}

	m.Machine = latestMachine
	return nil
}

func (m *MachineScope) storeMachineStatus() error {
	if equality.Semantic.DeepEqual(m.MachineStatus, m.origMachineStatus) && equality.Semantic.DeepEqual(m.Machine.Status.Addresses, m.origMachine.Status.Addresses) {
		klog.Infof("%s: status unchanged", m.Machine.Name)
		return nil
	}

	klog.V(4).Infof("Storing machine status for %q, resourceVersion: %v, generation: %v", m.Machine.Name, m.Machine.ResourceVersion, m.Machine.Generation)
	ext, err := v1beta1.EncodeMachineStatus(m.MachineStatus)
	if err != nil {
		return err
	}

	m.Machine.Status.ProviderStatus = ext

	time := metav1.Now()
	m.Machine.Status.LastUpdated = &time
	latestMachine, err := m.MachineClient.UpdateStatus(m.Machine)
	if err != nil {
		return err
	}
	m.Machine = latestMachine
	return err
}

// Persist the machine spec and machine status.
func (m *MachineScope) Persist() error {
	if m.MachineClient == nil {
		return fmt.Errorf("machine client is empty")
	}

	// The machine status needs to be updated first since
	// the next call to storeMachineSpec updates entire machine
	// object. If done in the reverse order, the machine status
	// could be updated without setting the LastUpdated field
	// in the machine status. The following might occur:
	// 1. machine object is updated (including its status)
	// 2. the machine object is updated by different component/user meantime
	// 3. storeMachineStatus is called but fails since the machine object
	//    is outdated. The operation is reconciled but given the status
	//    was already set in the previous call, the status is no longer updated
	//    since the status updated condition is already false. Thus,
	//    the LastUpdated is not set/updated properly.
	if err := m.storeMachineStatus(); err != nil {
		return fmt.Errorf("[machinescope] failed to store provider status for machine %q in namespace %q: %v", m.Machine.Name, m.Machine.Namespace, err)
	}

	if err := m.storeMachineSpec(); err != nil {
		return fmt.Errorf("[machinescope] failed to update machine %q in namespace %q: %v", m.Machine.Name, m.Machine.Namespace, err)
	}

	return nil
}

// MachineConfigFromProviderSpec tries to decode the JSON-encoded spec, falling back on getting a MachineClass if the value is absent.
func MachineConfigFromProviderSpec(clusterClient machineclient.MachineClassesGetter, providerConfig machinev1.ProviderSpec) (*v1beta1.AzureMachineProviderSpec, error) {
	var config v1beta1.AzureMachineProviderSpec
	if providerConfig.Value != nil {
		klog.V(4).Info("Decoding ProviderConfig from Value")
		return unmarshalProviderSpec(providerConfig.Value)
	}

	return &config, nil
}

func unmarshalProviderSpec(spec *runtime.RawExtension) (*v1beta1.AzureMachineProviderSpec, error) {
	var config v1beta1.AzureMachineProviderSpec
	if spec != nil {
		if err := yaml.Unmarshal(spec.Raw, &config); err != nil {
			return nil, err
		}
	}
	klog.V(6).Infof("Found ProviderSpec: %+v", config)
	return &config, nil
}

func updateScope(coreClient controllerclient.Client, credentialsSecret *apicorev1.SecretReference, scope *Scope) error {
	if credentialsSecret == nil {
		return errors.New("provided empty credentials secret")
	}

	secretType := types.NamespacedName{Namespace: credentialsSecret.Namespace, Name: credentialsSecret.Name}
	var secret apicorev1.Secret
	if err := coreClient.Get(
		context.Background(),
		secretType,
		&secret); err != nil {
		return err
	}

	subscriptionID, ok := secret.Data[AzureCredsSubscriptionIDKey]
	if !ok {
		return errors.Errorf("Azure subscription id %v did not contain key %v",
			secretType.String(), AzureCredsSubscriptionIDKey)
	}
	clientID, ok := secret.Data[AzureCredsClientIDKey]
	if !ok {
		return errors.Errorf("Azure client id %v did not contain key %v",
			secretType.String(), AzureCredsClientIDKey)
	}
	clientSecret, ok := secret.Data[AzureCredsClientSecretKey]
	if !ok {
		return errors.Errorf("Azure client secret %v did not contain key %v",
			secretType.String(), AzureCredsClientSecretKey)
	}
	tenantID, ok := secret.Data[AzureCredsTenantIDKey]
	if !ok {
		return errors.Errorf("Azure tenant id %v did not contain key %v",
			secretType.String(), AzureCredsTenantIDKey)
	}
	resourceGroup, ok := secret.Data[AzureCredsResourceGroupKey]
	if !ok {
		return errors.Errorf("Azure resource group %v did not contain key %v",
			secretType.String(), AzureCredsResourceGroupKey)
	}
	region, ok := secret.Data[AzureCredsRegionKey]
	if !ok {
		return errors.Errorf("Azure region %v did not contain key %v",
			secretType.String(), AzureCredsRegionKey)
	}
	clusterName, ok := secret.Data[AzureResourcePrefix]
	if !ok {
		return errors.Errorf("Azure resource prefix %v did not contain key %v",
			secretType.String(), AzureResourcePrefix)
	}

	env, err := azure.EnvironmentFromName("AzurePublicCloud")
	if err != nil {
		return err
	}
	oauthConfig, err := adal.NewOAuthConfig(
		env.ActiveDirectoryEndpoint, string(tenantID))
	if err != nil {
		return err
	}

	token, err := adal.NewServicePrincipalToken(
		*oauthConfig, string(clientID), string(clientSecret), env.ResourceManagerEndpoint)
	if err != nil {
		return err
	}

	authorizer, err := autorest.NewBearerAuthorizer(token), nil
	if err != nil {
		return errors.Errorf("failed to create azure session: %v", err)
	}

	scope.Cluster.ObjectMeta.Name = string(clusterName)
	scope.Authorizer = authorizer
	scope.SubscriptionID = string(subscriptionID)
	scope.ClusterConfig.ResourceGroup = string(resourceGroup)
	scope.ClusterConfig.Location = string(region)

	return nil
}
