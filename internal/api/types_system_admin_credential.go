// Copyright 2025 Microsoft Corporation
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

package api

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SystemAdminCredential represents a temporary system admin credential
// for an ARO HCP OpenShift cluster, tracked in Cosmos as a per-credential
// document nested under the cluster.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type SystemAdminCredential struct {
	CosmosMetadata `json:"cosmosMetadata"`

	Spec   SystemAdminCredentialSpec   `json:"spec"`
	Status SystemAdminCredentialStatus `json:"status"`
}

// SystemAdminCredentialSpec contains the desired state of the credential.
type SystemAdminCredentialSpec struct {
	// Username is the K8s username embedded in the cert CN. Defaulted at
	// create; the cluster's ACM cluster-admin role binding picks it up.
	Username string `json:"username,omitempty"`
	// ExpirationTimestamp is when the cert ceases to be valid. Server-set
	// at create (now + 24h) — we never let the customer pick.
	ExpirationTimestamp metav1.Time `json:"expirationTimestamp"`
	// OperationID is the ARM operation that created this credential. Used
	// to link the doc back to the customer-visible OperationResult.
	OperationID string `json:"operationID"`
	// PublicKeyPEM is the public half of the keypair generated at dispatch
	// time, PEM-encoded. The CSR carries the DER form of the same key;
	// we keep PEM here only as a convenience for diagnostics and for
	// golden-file fixtures.
	PublicKeyPEM string `json:"publicKeyPEM"`
	// PrivateKeyPEM is the private half of the keypair, PEM-encoded.
	// It is the input to OperationResult's kubeconfig assembly and
	// never leaves Cosmos. Treat as a secret in logs, dumps, and telemetry.
	PrivateKeyPEM string `json:"privateKeyPEM"`
}

// SystemAdminCredentialStatus contains the observed state of the credential.
type SystemAdminCredentialStatus struct {
	// Phase is the lifecycle state. Mirrors the cluster-service `status`
	// column we are replacing.
	Phase SystemAdminCredentialPhase `json:"phase"`
	// SignedCertificate is the base64-DER cert the management-cluster
	// signer produced. Populated when Phase moves to Issued.
	SignedCertificate string `json:"signedCertificate,omitempty"`
	// Conditions is the standard rolling-status array.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// RevokedAt is set when Phase transitions to Revoked. It is the
	// anchor used by the SystemAdminCredentialRevokedGC controller to
	// delete the doc 48h after revocation lands.
	RevokedAt *metav1.Time `json:"revokedAt,omitempty"`
	// OutstandingDesires names every per-credential kube-applier desire
	// that still exists in Cosmos for this credential. Controllers
	// append to it when they create a desire and remove from it when
	// they delete the doc.
	OutstandingDesires []SystemAdminCredentialDesireRef `json:"outstandingDesires,omitempty"`
}

// SystemAdminCredentialDesireRef points at a single kube-applier desire
// document scoped under the credential's parent cluster. Kind selects
// the container (ApplyDesires / ReadDesires / DeleteDesires); Name is
// the desire document's last-segment name within that container.
type SystemAdminCredentialDesireRef struct {
	Kind SystemAdminCredentialDesireKind `json:"kind"`
	Name string                          `json:"name"`
}

// SystemAdminCredentialDesireKind identifies the kube-applier desire container type.
type SystemAdminCredentialDesireKind string

const (
	SystemAdminCredentialDesireKindApply  SystemAdminCredentialDesireKind = "ApplyDesire"
	SystemAdminCredentialDesireKindRead   SystemAdminCredentialDesireKind = "ReadDesire"
	SystemAdminCredentialDesireKindDelete SystemAdminCredentialDesireKind = "DeleteDesire"
)

// SystemAdminCredentialPhase represents the lifecycle state of a credential.
type SystemAdminCredentialPhase string

const (
	SystemAdminCredentialPhaseRequested          SystemAdminCredentialPhase = "Requested"
	SystemAdminCredentialPhaseIssued             SystemAdminCredentialPhase = "Issued"
	SystemAdminCredentialPhaseAwaitingRevocation SystemAdminCredentialPhase = "AwaitingRevocation"
	SystemAdminCredentialPhaseRevoked            SystemAdminCredentialPhase = "Revoked"
	SystemAdminCredentialPhaseFailed             SystemAdminCredentialPhase = "Failed"
)
