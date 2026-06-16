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

// SystemAdminCredential is a Cosmos document that tracks a single
// short-lived admin credential (break-glass replacement) scoped to a
// cluster. The frontend's requestadmincredential action creates it;
// revoke marks every non-terminal one for deletion.
type SystemAdminCredential struct {
	CosmosMetadata `json:"cosmosMetadata"`

	Spec   SystemAdminCredentialSpec   `json:"spec"`
	Status SystemAdminCredentialStatus `json:"status"`
}

// SystemAdminCredentialSpec carries the immutable inputs set at
// dispatch time.
type SystemAdminCredentialSpec struct {
	// Username is the K8s username embedded in the cert CN.
	Username string `json:"username,omitempty"`
	// ExpirationTimestamp is when the cert ceases to be valid (now + 24h).
	ExpirationTimestamp metav1.Time `json:"expirationTimestamp"`
	// OperationID is the ARM operation that created this credential.
	OperationID string `json:"operationID"`
	// PublicKeyPEM is the PEM-encoded public half of the keypair.
	PublicKeyPEM string `json:"publicKeyPEM"`
	// PrivateKeyPEM is the PEM-encoded private half of the keypair.
	// Treat as a secret in logs, dumps, and telemetry.
	PrivateKeyPEM string `json:"privateKeyPEM"`
}

// SystemAdminCredentialStatus carries the mutable lifecycle state.
type SystemAdminCredentialStatus struct {
	// Phase is the lifecycle state.
	Phase SystemAdminCredentialPhase `json:"phase"`
	// SignedCertificate is the base64-DER cert produced by the MC signer.
	SignedCertificate string `json:"signedCertificate,omitempty"`
	// Conditions is the standard rolling-status array.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// RevokedAt is set when Phase transitions to Revoked.
	RevokedAt *metav1.Time `json:"revokedAt,omitempty"`
	// OutstandingDesires names every per-credential kube-applier desire
	// that still exists in Cosmos for this credential.
	OutstandingDesires []SystemAdminCredentialDesireRef `json:"outstandingDesires,omitempty"`
}

// SystemAdminCredentialDesireRef points at a single kube-applier desire
// document scoped under the credential's parent cluster.
type SystemAdminCredentialDesireRef struct {
	Kind SystemAdminCredentialDesireKind `json:"kind"`
	Name string                          `json:"name"`
}

// SystemAdminCredentialDesireKind selects the desire container.
type SystemAdminCredentialDesireKind string

const (
	SystemAdminCredentialDesireKindApply  SystemAdminCredentialDesireKind = "ApplyDesire"
	SystemAdminCredentialDesireKindRead   SystemAdminCredentialDesireKind = "ReadDesire"
	SystemAdminCredentialDesireKindDelete SystemAdminCredentialDesireKind = "DeleteDesire"
)

// SystemAdminCredentialPhase is the lifecycle state of a credential.
type SystemAdminCredentialPhase string

const (
	SystemAdminCredentialPhaseRequested          SystemAdminCredentialPhase = "Requested"
	SystemAdminCredentialPhaseIssued             SystemAdminCredentialPhase = "Issued"
	SystemAdminCredentialPhaseAwaitingRevocation SystemAdminCredentialPhase = "AwaitingRevocation"
	SystemAdminCredentialPhaseRevoked            SystemAdminCredentialPhase = "Revoked"
	SystemAdminCredentialPhaseFailed             SystemAdminCredentialPhase = "Failed"
)
