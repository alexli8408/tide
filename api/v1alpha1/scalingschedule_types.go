/*
Copyright 2026 Alex Li.
Licensed under the MIT License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TargetKind is the kind of workload a ScalingSchedule can scale.
// +kubebuilder:validation:Enum=Deployment;StatefulSet
type TargetKind string

const (
	TargetKindDeployment  TargetKind = "Deployment"
	TargetKindStatefulSet TargetKind = "StatefulSet"
)

// TargetRef identifies the workload this schedule scales. Only workloads in
// the same namespace as the ScalingSchedule can be targeted, so a schedule
// never needs (or receives) cross-namespace privileges.
type TargetRef struct {
	// Kind of the target workload.
	// +kubebuilder:default=Deployment
	// +optional
	Kind TargetKind `json:"kind,omitempty"`

	// Name of the target workload.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// DayOfWeek is a three-letter English day-of-week name.
// +kubebuilder:validation:Enum=Mon;Tue;Wed;Thu;Fri;Sat;Sun
type DayOfWeek string

// ScalingWindow is a recurring weekly time window during which the target is
// held at a specific replica count.
type ScalingWindow struct {
	// Name identifies this window in status fields and events.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// Days of the week on which this window starts. A window that wraps past
	// midnight belongs to the day it starts on: a Fri 22:00–02:00 window ends
	// Saturday at 02:00.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=7
	Days []DayOfWeek `json:"days"`

	// Start of the window as 24-hour "HH:MM" wall-clock time in the
	// schedule's timezone.
	// +kubebuilder:validation:Pattern=`^([01][0-9]|2[0-3]):[0-5][0-9]$`
	Start string `json:"start"`

	// End of the window as 24-hour "HH:MM" wall-clock time. If End is not
	// later than Start, the window wraps past midnight into the next day.
	// +kubebuilder:validation:Pattern=`^([01][0-9]|2[0-3]):[0-5][0-9]$`
	End string `json:"end"`

	// Replicas the target is held at while this window is active.
	// +kubebuilder:validation:Minimum=0
	Replicas int32 `json:"replicas"`
}

// ScalingScheduleSpec declares the desired scaling behaviour for one workload.
type ScalingScheduleSpec struct {
	// TargetRef names the workload to scale, in the same namespace as this
	// ScalingSchedule.
	TargetRef TargetRef `json:"targetRef"`

	// TimeZone is an IANA timezone name (for example "America/Toronto") used
	// to interpret window start and end times. Daylight-saving transitions
	// are handled by the timezone database, not by the schedule author.
	// +kubebuilder:default=UTC
	// +optional
	TimeZone string `json:"timeZone,omitempty"`

	// DefaultReplicas is the replica count applied whenever no window is
	// active.
	// +kubebuilder:validation:Minimum=0
	DefaultReplicas int32 `json:"defaultReplicas"`

	// Windows are the recurring time windows that override DefaultReplicas.
	// When windows overlap, the highest replica count wins, so a schedule can
	// never accidentally scale a workload below what another active window
	// asked for.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	Windows []ScalingWindow `json:"windows,omitempty"`

	// Suspend stops the controller from scaling the target while true.
	// Status is still reported so a suspended schedule shows what it *would*
	// do.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// Condition types and reasons reported on ScalingSchedule status.
const (
	// ConditionReady means the schedule is valid, the target exists, and the
	// target's replica count matches the schedule.
	ConditionReady = "Ready"

	ReasonReconciled        = "Reconciled"
	ReasonSuspended         = "Suspended"
	ReasonTargetNotFound    = "TargetNotFound"
	ReasonInvalidSchedule   = "InvalidSchedule"
	ReasonScaleFailed       = "ScaleFailed"
	ReasonConflictingTarget = "ConflictingTarget"
)

// ScalingScheduleStatus is the observed state of a ScalingSchedule.
type ScalingScheduleStatus struct {
	// Conditions describe the current state of the schedule.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ActiveWindow is the name of the window currently deciding the replica
	// count, or empty when DefaultReplicas applies.
	// +optional
	ActiveWindow string `json:"activeWindow,omitempty"`

	// DesiredReplicas is the replica count the schedule currently calls for.
	// +optional
	DesiredReplicas *int32 `json:"desiredReplicas,omitempty"`

	// NextTransitionTime is when the desired replica count next changes; the
	// controller re-reconciles itself at this instant.
	// +optional
	NextTransitionTime *metav1.Time `json:"nextTransitionTime,omitempty"`

	// LastScaleTime is when the controller last changed the target's replica
	// count.
	// +optional
	LastScaleTime *metav1.Time `json:"lastScaleTime,omitempty"`

	// ObservedGeneration is the spec generation the status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ssc
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetRef.name`
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.status.desiredReplicas`
// +kubebuilder:printcolumn:name="Window",type=string,JSONPath=`.status.activeWindow`
// +kubebuilder:printcolumn:name="Next",type=string,JSONPath=`.status.nextTransitionTime`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ScalingSchedule scales one workload on a recurring weekly schedule.
type ScalingSchedule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ScalingScheduleSpec   `json:"spec,omitempty"`
	Status ScalingScheduleStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ScalingScheduleList contains a list of ScalingSchedule.
type ScalingScheduleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ScalingSchedule `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ScalingSchedule{}, &ScalingScheduleList{})
}
