/*
Copyright 2026.

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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// WorkloadKind is the Kubernetes workload kind that runs a Trino component.
// +kubebuilder:validation:Enum=Deployment;StatefulSet
type WorkloadKind string

const (
	WorkloadKindDeployment  WorkloadKind = "Deployment"
	WorkloadKindStatefulSet WorkloadKind = "StatefulSet"
)

// WorkloadReference points at the Kubernetes workload (in the TrinoCluster namespace)
// that runs a Trino component.
type WorkloadReference struct {
	// kind of the workload.
	// +required
	Kind WorkloadKind `json:"kind"`

	// name of the workload.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
}

// TrinoClusterWorkload describes the Kubernetes workloads backing the cluster.
type TrinoClusterWorkload struct {
	// coordinator workload, informational only. Readiness does not prove query completion.
	// +required
	Coordinator WorkloadReference `json:"coordinator"`

	// workers workload. Informational in v0.1.
	// +optional
	Workers *WorkloadReference `json:"workers,omitempty"`
}

// CoordinatorSpec describes how the operator reaches the Trino coordinator.
type CoordinatorSpec struct {
	// url is the base URL of the coordinator HTTP endpoint,
	// e.g. http://general-1-coordinator.trino.svc:8080
	// +kubebuilder:validation:Pattern=`^https?://.+`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="coordinator URL is immutable"
	// +required
	URL string `json:"url"`

	// credentialsSecret optionally references a Secret in the TrinoCluster namespace
	// holding `username` and `password` keys used for HTTP basic authentication
	// against the coordinator (/metrics is protected by the Trino authentication
	// configuration). When omitted the operator only sends an X-Trino-User header.
	// +optional
	CredentialsSecret *corev1.LocalObjectReference `json:"credentialsSecret,omitempty"`
}

// TrinoClusterSpec defines the desired state of TrinoCluster
type TrinoClusterSpec struct {
	// routingGroup is the name of the TrinoRoutingGroup this backend belongs to.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="routingGroup is immutable"
	// +kubebuilder:validation:MinLength=1
	// +required
	RoutingGroup string `json:"routingGroup"`

	// gatewayBackend is the backend name registered in Trino Gateway.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="gatewayBackend is immutable"
	// +kubebuilder:validation:MinLength=1
	// +required
	GatewayBackend string `json:"gatewayBackend"`

	// coordinator describes how to reach the Trino coordinator.
	// +required
	Coordinator CoordinatorSpec `json:"coordinator"`

	// workload references the Kubernetes workloads running this cluster.
	// +optional
	Workload *TrinoClusterWorkload `json:"workload,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Routing-Group",type=string,JSONPath=`.spec.routingGroup`
// +kubebuilder:printcolumn:name="Gateway-Backend",type=string,JSONPath=`.spec.gatewayBackend`
// +kubebuilder:printcolumn:name="Coordinator",type=string,JSONPath=`.spec.coordinator.url`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// TrinoCluster describes one real Trino/Starburst backend that sits behind Trino Gateway.
// It is a configuration resource: the operator reads it but never changes it.
type TrinoCluster struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of TrinoCluster
	// +required
	Spec TrinoClusterSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// TrinoClusterList contains a list of TrinoCluster
type TrinoClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []TrinoCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &TrinoCluster{}, &TrinoClusterList{})
		return nil
	})
}
