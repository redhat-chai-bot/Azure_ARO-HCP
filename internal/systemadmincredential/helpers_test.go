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

package systemadmincredential

import (
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	clientcmd "k8s.io/client-go/tools/clientcmd"
)

func TestGenerateKeypairAndBuildCSR(t *testing.T) {
	// Generate a keypair.
	pubPEM, privPEM, err := GenerateKeypair()
	require.NoError(t, err, "GenerateKeypair should succeed")
	require.NotEmpty(t, pubPEM, "public key PEM should not be empty")
	require.NotEmpty(t, privPEM, "private key PEM should not be empty")

	// Parse a dummy ARM resource ID to use as owner.
	owner, err := azcorearm.ParseResourceID(
		"/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.RedHatOpenShift/hcpOpenShiftClusters/c1",
	)
	require.NoError(t, err, "ParseResourceID should succeed")

	// Build a CSR.
	csr, err := BuildCSR(owner, "abc123", "system:admin", privPEM, "hcp-ns")
	require.NoError(t, err, "BuildCSR should succeed")

	// Spec.Request should be non-empty PEM that decodes to a valid PKCS#10.
	require.NotEmpty(t, csr.Spec.Request, "CSR request bytes should not be empty")

	block, _ := pem.Decode(csr.Spec.Request)
	require.NotNil(t, block, "CSR request should PEM-decode")

	parsedCSR, err := x509.ParseCertificateRequest(block.Bytes)
	require.NoError(t, err, "CSR DER should parse as x509.CertificateRequest")
	assert.Equal(t, "system:admin", parsedCSR.Subject.CommonName,
		"CSR CommonName should be the requested username")

	// Owner annotation should be set and lowercased.
	ownerVal, ok := csr.Annotations[OwnerAnnotationKey]
	require.True(t, ok, "owner annotation should be present")
	assert.Equal(t, strings.ToLower(owner.String()), ownerVal,
		"owner annotation should be the lowercased ARM resource ID")

	// SignerName should contain customer-break-glass.
	assert.Contains(t, csr.Spec.SignerName, SignerClass,
		"SignerName should contain the customer-break-glass signer class")
}

func TestBuildKubeconfig(t *testing.T) {
	kubeconfigBytes, err := BuildKubeconfig("c1", "https://api.example:6443", "CA", "CERT", "KEY")
	require.NoError(t, err, "BuildKubeconfig should succeed")

	cfg, err := clientcmd.Load(kubeconfigBytes)
	require.NoError(t, err, "kubeconfig should parse via clientcmd.Load")

	assert.Equal(t, "c1", cfg.CurrentContext,
		"CurrentContext should be the cluster name")

	cluster, ok := cfg.Clusters["c1"]
	require.True(t, ok, "cluster entry 'c1' should exist")
	assert.Equal(t, "https://api.example:6443", cluster.Server,
		"cluster server URL should match")
}

func TestRequireOwnerPanics(t *testing.T) {
	assert.Panics(t, func() {
		BuildCSRA(nil, "x", "ns")
	}, "BuildCSRA with nil owner should panic")
}
