/*
Copyright 2026 Alex Li.
Licensed under the MIT License.
*/

// Package v1alpha1 contains the v1alpha1 API group for Tide.
//
// The group name is rooted at alexli8408.github.io — a domain the project
// author controls — following the Kubernetes convention that API groups are
// DNS names owned by whoever defines them.
// +kubebuilder:object:generate=true
// +groupName=tide.alexli8408.github.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "tide.alexli8408.github.io", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
