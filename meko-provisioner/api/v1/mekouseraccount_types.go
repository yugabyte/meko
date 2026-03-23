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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type AccountTier string

const (
	TierFree       AccountTier = "free"
	TierPro        AccountTier = "pro"
	TierEnterprise AccountTier = "enterprise"
)

// MekoUserAccountSpec defines the desired state of MekoUserAccount.
type MekoUserAccountSpec struct {
	// +kubebuilder:validation:MinLength=1
	AccountID string `json:"accountId"`
	// +kubebuilder:validation:Enum=free;pro;enterprise
	AccountTier   AccountTier `json:"accountTier"`
	MCPImage      string      `json:"mcpImage"`
	RAGImage      string      `json:"ragImage"`
	LangfuseImage string      `json:"langfuseImage"`
	// +kubebuilder:default=1
	// +optional
	MCPReplicas int32 `json:"mcpReplicas,omitempty"`
	// +kubebuilder:default=1
	// +optional
	RAGReplicas int32 `json:"ragReplicas,omitempty"`
}

// MekoUserAccountStatus defines the observed state of MekoUserAccount.
type MekoUserAccountStatus struct {
	Phase         string `json:"phase,omitempty"`
	NamespaceName string `json:"namespaceName,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// MekoUserAccount is the Schema for the mekouseraccounts API
type MekoUserAccount struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of MekoUserAccount
	// +required
	Spec MekoUserAccountSpec `json:"spec"`

	// status defines the observed state of MekoUserAccount
	// +optional
	Status MekoUserAccountStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// MekoUserAccountList contains a list of MekoUserAccount
type MekoUserAccountList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []MekoUserAccount `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MekoUserAccount{}, &MekoUserAccountList{})
}
