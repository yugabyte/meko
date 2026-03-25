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
	"fmt"

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
	// +kubebuilder:validation:MinLength=1
	YugabyteDBHost string `json:"yugabyteDBHost"`
	// +kubebuilder:default=5433
	// +optional
	YugabyteDBPort int32 `json:"yugabyteDBPort,omitempty"`
	// +kubebuilder:default="yugabyte"
	// +optional
	YugabyteDBUser string `json:"yugabyteDBUser,omitempty"`
	// +kubebuilder:validation:MinLength=1
	YugabyteDBPassword string `json:"yugabyteDBPassword"`
	// +kubebuilder:validation:MinLength=1
	YugabyteDBName string `json:"yugabyteDBName"`
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
	// +optional
	MCPEndpoint string `json:"mcpEndpoint,omitempty"`
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

// GetYugabyteDBPort returns the port with a default of 5433.
func (s *MekoUserAccountSpec) GetYugabyteDBPort() int32 {
	if s.YugabyteDBPort == 0 {
		return 5433
	}
	return s.YugabyteDBPort
}

// GetYugabyteDBUser returns the user with a default of "yugabyte".
func (s *MekoUserAccountSpec) GetYugabyteDBUser() string {
	if s.YugabyteDBUser == "" {
		return "yugabyte"
	}
	return s.YugabyteDBUser
}

// YugabyteDBConnectionStringURI returns a PostgreSQL URI connection string.
func (s *MekoUserAccountSpec) YugabyteDBConnectionStringURI() string {
	return fmt.Sprintf("postgresql://%s:%s@%s:%d/%s",
		s.GetYugabyteDBUser(), s.YugabyteDBPassword,
		s.YugabyteDBHost, s.GetYugabyteDBPort(), s.YugabyteDBName)
}

// YugabyteDBConnectionStringLibpq returns a libpq-format connection string.
func (s *MekoUserAccountSpec) YugabyteDBConnectionStringLibpq() string {
	return fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s",
		s.YugabyteDBHost, s.GetYugabyteDBPort(), s.YugabyteDBName,
		s.GetYugabyteDBUser(), s.YugabyteDBPassword)
}

func init() {
	SchemeBuilder.Register(&MekoUserAccount{}, &MekoUserAccountList{})
}
