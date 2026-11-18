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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// TrinoRoutingGroupSpec defines the availability policy of a routing group.
type TrinoRoutingGroupSpec struct {
	// minAvailable is the minimum number of backends of this routing group that must
	// stay active and HEALTHY in Gateway when deactivating another backend.
	// With three backends and minAvailable=2 one backend can be drained at a time;
	// once only two backends are active no further drain is permitted.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	// +optional
	MinAvailable int32 `json:"minAvailable"`
}

// ActiveMaintenance identifies the TrinoMaintenance that currently owns the routing group.
// Ownership spans the whole maintenance window of a backend: it is acquired by a Drain and
// released by the Resume of the same TrinoCluster.
type ActiveMaintenance struct {
	// namespace of the owning TrinoMaintenance (and of its TrinoCluster).
	// +required
	Namespace string `json:"namespace"`

	// name of the owning TrinoMaintenance.
	// +required
	Name string `json:"name"`

	// clusterName is the TrinoCluster under maintenance.
	// +required
	ClusterName string `json:"clusterName"`

	// clusterUID pins this window to the registered backend, not a recreated CR.
	ClusterUID types.UID `json:"clusterUID"`
	// drainUID identifies this incarnation of the named PreSync request.
	DrainUID types.UID `json:"drainUID"`
	// previousResumeUID is the pre-existing named Resume, if any. It can never
	// open this window, even after an operator restart or retry of the same revision.
	PreviousResumeUID types.UID `json:"previousResumeUID,omitempty"`
	// verifiedOffline records a successful idle observation for this backend.
	// Cleared before any activation attempt, including one that times out.
	VerifiedOffline bool `json:"verifiedOffline,omitempty"`
}

// TrinoRoutingGroupStatus defines the observed state of TrinoRoutingGroup.
type TrinoRoutingGroupStatus struct {
	// activeMaintenance is set while a backend of this routing group is under maintenance.
	// It serialises maintenance operations within the group.
	// +optional
	ActiveMaintenance *ActiveMaintenance `json:"activeMaintenance,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Min-Available",type=integer,JSONPath=`.spec.minAvailable`
// +kubebuilder:printcolumn:name="Active-Maintenance",type=string,JSONPath=`.status.activeMaintenance.name`
// +kubebuilder:printcolumn:name="Maintenance-Cluster",type=string,JSONPath=`.status.activeMaintenance.clusterName`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// TrinoRoutingGroup is the availability policy for a set of TrinoClusters that share a Trino
// Gateway routing group. It is cluster-scoped because its backends may live in different namespaces.
type TrinoRoutingGroup struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of TrinoRoutingGroup
	// +required
	Spec TrinoRoutingGroupSpec `json:"spec"`

	// status defines the observed state of TrinoRoutingGroup
	// +optional
	Status TrinoRoutingGroupStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// TrinoRoutingGroupList contains a list of TrinoRoutingGroup
type TrinoRoutingGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []TrinoRoutingGroup `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &TrinoRoutingGroup{}, &TrinoRoutingGroupList{})
		return nil
	})
}
