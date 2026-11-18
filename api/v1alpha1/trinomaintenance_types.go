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

// MaintenanceAction is the operation requested by a TrinoMaintenance.
// +kubebuilder:validation:Enum=Drain;Resume
type MaintenanceAction string

const (
	// ActionDrain deactivates the backend in Trino Gateway and waits until no
	// queries are running or queued on the coordinator.
	ActionDrain MaintenanceAction = "Drain"
	// ActionResume verifies the coordinator is healthy and activates the backend
	// in Trino Gateway again.
	ActionResume MaintenanceAction = "Resume"
)

// MaintenancePhase is the coarse-grained progress of a TrinoMaintenance.
// +kubebuilder:validation:Enum=Waiting;Draining;Drained;Resuming;Completed;Failed
type MaintenancePhase string

const (
	// PhaseWaiting: the operation cannot start yet (another backend of the routing
	// group is under maintenance, or availability policy forbids it).
	PhaseWaiting MaintenancePhase = "Waiting"
	// PhaseDraining: backend is deactivated in the gateway, queries are still finishing.
	PhaseDraining MaintenancePhase = "Draining"
	// PhaseDrained: backend is deactivated and no queries are running or queued. Terminal for Drain.
	PhaseDrained MaintenancePhase = "Drained"
	// PhaseResuming: waiting for the coordinator to become healthy.
	PhaseResuming MaintenancePhase = "Resuming"
	// PhaseCompleted: backend is active again. Terminal for Resume.
	PhaseCompleted MaintenancePhase = "Completed"
	// PhaseFailed: the operation failed and will not be retried by the controller. Terminal.
	PhaseFailed MaintenancePhase = "Failed"
)

// IsTerminal reports whether no further reconciliation happens in this phase.
func (p MaintenancePhase) IsTerminal() bool {
	return p == PhaseDrained || p == PhaseCompleted || p == PhaseFailed
}

// TrinoMaintenanceSpec defines the requested maintenance operation.
// The spec is immutable: create a new TrinoMaintenance for a new operation.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type TrinoMaintenanceSpec struct {
	// clusterName is the TrinoCluster (in the same namespace) to operate on.
	// +kubebuilder:validation:MinLength=1
	// +required
	ClusterName string `json:"clusterName"`

	// action to perform.
	// +required
	Action MaintenanceAction `json:"action"`
}

// TrinoMaintenanceStatus defines the observed state of TrinoMaintenance.
type TrinoMaintenanceStatus struct {
	// phase is the coarse-grained progress of the operation.
	// +optional
	Phase MaintenancePhase `json:"phase,omitempty"`

	// runningQueries last observed on the coordinator (Drain only).
	// +optional
	RunningQueries *int64 `json:"runningQueries,omitempty"`

	// queuedQueries last observed on the coordinator (Drain only).
	// +optional
	QueuedQueries *int64 `json:"queuedQueries,omitempty"`

	// unfinishedQueries includes queries in all nonterminal states.
	UnfinishedQueries *int64 `json:"unfinishedQueries,omitempty"`

	// activatedDrainUID is a Resume receipt: the backend was observed active
	// and healthy. Only releasing this particular lock remains after a restart.
	ActivatedDrainUID types.UID `json:"activatedDrainUID,omitempty"`

	// message is a human readable explanation of the current phase.
	// +optional
	Message string `json:"message,omitempty"`

	// observedGeneration is the generation of the spec the status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions represent detailed progress of the operation.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterName`
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=`.spec.action`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Running",type=integer,JSONPath=`.status.runningQueries`
// +kubebuilder:printcolumn:name="Queued",type=integer,JSONPath=`.status.queuedQueries`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.message`,priority=1

// TrinoMaintenance is a request to drain or resume one Trino backend. It is a short-lived
// resource: ArgoCD hooks (or humans) create it, the controller drives it to a terminal phase.
type TrinoMaintenance struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of TrinoMaintenance
	// +required
	Spec TrinoMaintenanceSpec `json:"spec"`

	// status defines the observed state of TrinoMaintenance
	// +optional
	Status TrinoMaintenanceStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// TrinoMaintenanceList contains a list of TrinoMaintenance
type TrinoMaintenanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []TrinoMaintenance `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &TrinoMaintenance{}, &TrinoMaintenanceList{})
		return nil
	})
}
