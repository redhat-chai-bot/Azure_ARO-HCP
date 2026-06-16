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

// Package systemadmincredential holds pure helpers shared by the
// system-admin-credential controllers and the frontend. None of these
// functions perform I/O; they build Kubernetes objects or assemble a
// kubeconfig from already-fetched material.
package systemadmincredential

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"strings"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	hypershiftcertv1alpha1 "github.com/openshift/hypershift/api/certificates/v1alpha1"
	certificatesv1 "k8s.io/api/certificates/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientcmd "k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// OwnerAnnotationKey is stamped on every k8s object we land on a
	// management cluster via ApplyDesire. Its value is the lowercased
	// ARM resource ID of the ARO-HCP owner (the cluster, for everything
	// in this package).
	OwnerAnnotationKey = "aro-hcp.openshift.io/owner"

	// SignerClass is the HyperShift signer class for customer break-glass
	// certs. This string is HyperShift's contract with its
	// control-plane-pki-operator, not ours to rename.
	SignerClass = "customer-break-glass"

	// hypershiftCertAPIVersion is certificates.hypershift.openshift.io/v1alpha1.
	hypershiftCertAPIVersion = "certificates.hypershift.openshift.io/v1alpha1"

	rsaKeyBits = 4096
)

// signerNameForNamespace returns the per-cluster CSR signer name HyperShift's
// control-plane-pki-operator watches.
func signerNameForNamespace(hcpNamespace string) string {
	return fmt.Sprintf("hypershift.openshift.io/%s.%s", hcpNamespace, SignerClass)
}

// GenerateKeypair generates an RSA keypair and returns PEM-encoded public
// (PKIX) and private (PKCS#8) key bytes. Called by the dispatch controller.
func GenerateKeypair() (publicPEM, privatePEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, rsaKeyBits)
	if err != nil {
		return nil, nil, fmt.Errorf("generating RSA key: %w", err)
	}

	privBytes, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshaling private key: %w", err)
	}
	privatePEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privBytes})

	pubBytes, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, nil, fmt.Errorf("marshaling public key: %w", err)
	}
	publicPEM = pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes})

	return publicPEM, privatePEM, nil
}

// requireOwner panics if owner is nil. Every Build* helper calls this so a
// call site cannot forget to attribute the object it lands on the MC.
func requireOwner(owner *azcorearm.ResourceID) {
	if owner == nil {
		panic("systemadmincredential: owner resource ID must not be nil")
	}
}

func setOwnerAnnotation(meta *metav1.ObjectMeta, owner *azcorearm.ResourceID) {
	if meta.Annotations == nil {
		meta.Annotations = make(map[string]string)
	}
	meta.Annotations[OwnerAnnotationKey] = strings.ToLower(owner.String())
}

// parsePrivateKeyPEM decodes a PKCS#8 (falling back to PKCS#1) RSA private key.
func parsePrivateKeyPEM(privateKeyPEM []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(privateKeyPEM)
	if block == nil {
		return nil, fmt.Errorf("failed to decode private key PEM")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("private key is not RSA")
		}
		return rsaKey, nil
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

// BuildCSR creates a CertificateSigningRequest for a system admin credential.
//
// NOTE (deviation from PLAN.md): a valid PKCS#10 request must be SIGNED by the
// private key, so this helper takes privateKeyPEM rather than the public key.
// The dispatcher holds the private key in-process at create time, so this is
// the natural call site. The CN encodes the K8s username; the
// system:masters group grants cluster-admin.
func BuildCSR(owner *azcorearm.ResourceID, credName, username string, privateKeyPEM []byte, hcpNamespace string) (*certificatesv1.CertificateSigningRequest, error) {
	requireOwner(owner)

	key, err := parsePrivateKeyPEM(privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("parsing private key: %w", err)
	}

	template := &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:   username,
			Organization: []string{"system:masters"},
		},
	}
	derBytes, err := x509.CreateCertificateRequest(rand.Reader, template, key)
	if err != nil {
		return nil, fmt.Errorf("creating PKCS#10 request: %w", err)
	}
	requestPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: derBytes})

	csr := &certificatesv1.CertificateSigningRequest{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "certificates.k8s.io/v1",
			Kind:       "CertificateSigningRequest",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("system-admin-credential-%s", credName),
		},
		Spec: certificatesv1.CertificateSigningRequestSpec{
			Request:    requestPEM,
			SignerName: signerNameForNamespace(hcpNamespace),
			Usages:     []certificatesv1.KeyUsage{certificatesv1.UsageClientAuth},
		},
	}
	setOwnerAnnotation(&csr.ObjectMeta, owner)
	return csr, nil
}

// BuildCSRA creates the HyperShift CertificateSigningRequestApproval that
// authorizes the control-plane-pki-operator to sign the matching CSR. It is
// matched to the CSR by name.
func BuildCSRA(owner *azcorearm.ResourceID, credName, hcpNamespace string) *hypershiftcertv1alpha1.CertificateSigningRequestApproval {
	requireOwner(owner)
	csra := &hypershiftcertv1alpha1.CertificateSigningRequestApproval{
		TypeMeta: metav1.TypeMeta{
			APIVersion: hypershiftCertAPIVersion,
			Kind:       "CertificateSigningRequestApproval",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("system-admin-credential-%s", credName),
			Namespace: hcpNamespace,
		},
	}
	setOwnerAnnotation(&csra.ObjectMeta, owner)
	return csra
}

// BuildRevocationRequest creates the HyperShift CertificateRevocationRequest
// that revokes every customer-signer cert for the cluster. revokeOpSuffix is
// the 16-char form of the revoke operation's ID.
func BuildRevocationRequest(owner *azcorearm.ResourceID, revokeOpSuffix, hcpNamespace string) *hypershiftcertv1alpha1.CertificateRevocationRequest {
	requireOwner(owner)
	crr := &hypershiftcertv1alpha1.CertificateRevocationRequest{
		TypeMeta: metav1.TypeMeta{
			APIVersion: hypershiftCertAPIVersion,
			Kind:       "CertificateRevocationRequest",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("system-admin-credential-revocation-%s", revokeOpSuffix),
			Namespace: hcpNamespace,
		},
		Spec: hypershiftcertv1alpha1.CertificateRevocationRequestSpec{
			SignerClass: SignerClass,
		},
	}
	setOwnerAnnotation(&crr.ObjectMeta, owner)
	return crr
}

// klusterletSubject is the subject the RBAC bundles bind. The klusterlet agent
// service account on the MC enacts the desires.
// NOTE: confirm exact SA name/namespace against cluster-service's shipped
// bundles during review.
func klusterletSubject() rbacv1.Subject {
	return rbacv1.Subject{
		Kind:     "Group",
		APIGroup: "rbac.authorization.k8s.io",
		Name:     "system:serviceaccounts:open-cluster-management-agent-addon",
	}
}

// BuildRBACGiveCSRPerm returns a ClusterRole + ClusterRoleBinding granting CSR
// management, named system-admin-credential-give-csr-perm-<credName>.
func BuildRBACGiveCSRPerm(owner *azcorearm.ResourceID, credName string) []client.Object {
	requireOwner(owner)
	name := fmt.Sprintf("system-admin-credential-give-csr-perm-%s", credName)

	cr := &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{"certificates.k8s.io"},
			Resources: []string{"certificatesigningrequests"},
			Verbs:     []string{"get", "list", "watch", "create", "update", "patch"},
		}},
	}
	setOwnerAnnotation(&cr.ObjectMeta, owner)

	crb := &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: name},
		Subjects:   []rbacv1.Subject{klusterletSubject()},
	}
	setOwnerAnnotation(&crb.ObjectMeta, owner)

	return []client.Object{cr, crb}
}

// BuildRBACCSRA returns a Role + RoleBinding granting CSRA management in the
// HCP namespace, named system-admin-credential-csra-perm-<credName>.
func BuildRBACCSRA(owner *azcorearm.ResourceID, credName, hcpNamespace string) []client.Object {
	requireOwner(owner)
	name := fmt.Sprintf("system-admin-credential-csra-perm-%s", credName)

	role := &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: hcpNamespace},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{"certificates.hypershift.openshift.io"},
			Resources: []string{"certificatesigningrequestapprovals"},
			Verbs:     []string{"get", "list", "watch", "create", "update", "patch"},
		}},
	}
	setOwnerAnnotation(&role.ObjectMeta, owner)

	rb := &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: hcpNamespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: name},
		Subjects:   []rbacv1.Subject{klusterletSubject()},
	}
	setOwnerAnnotation(&rb.ObjectMeta, owner)

	return []client.Object{role, rb}
}

// BuildRBACRevocation returns a Role + RoleBinding granting CRR management in
// the HCP namespace, named system-admin-credential-revocation-perm-<credName>.
func BuildRBACRevocation(owner *azcorearm.ResourceID, credName, hcpNamespace string) []client.Object {
	requireOwner(owner)
	name := fmt.Sprintf("system-admin-credential-revocation-perm-%s", credName)

	role := &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: hcpNamespace},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{"certificates.hypershift.openshift.io"},
			Resources: []string{"certificaterevocationrequests"},
			Verbs:     []string{"get", "list", "watch", "create", "update", "patch"},
		}},
	}
	setOwnerAnnotation(&role.ObjectMeta, owner)

	rb := &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: hcpNamespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: name},
		Subjects:   []rbacv1.Subject{klusterletSubject()},
	}
	setOwnerAnnotation(&rb.ObjectMeta, owner)

	return []client.Object{role, rb}
}

// BuildKubeconfig assembles a kubeconfig from a signed cert, private key, CA
// bundle, and API URL. Pure function, no I/O. signedCertPEM is the PEM-encoded
// signed client certificate.
func BuildKubeconfig(clusterName, apiURL, caBundlePEM, signedCertPEM, privateKeyPEM string) ([]byte, error) {
	const user = "system:admin"
	config := clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{
			clusterName: {
				Server:                   apiURL,
				CertificateAuthorityData: []byte(caBundlePEM),
			},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			user: {
				ClientCertificateData: []byte(signedCertPEM),
				ClientKeyData:         []byte(privateKeyPEM),
			},
		},
		Contexts: map[string]*clientcmdapi.Context{
			clusterName: {
				Cluster:  clusterName,
				AuthInfo: user,
			},
		},
		CurrentContext: clusterName,
	}
	return clientcmd.Write(config)
}

// CredName generates a 16-character hex credential name from a UUID string.
func CredName(uuidStr string) string {
	return strings.ReplaceAll(uuidStr, "-", "")[:16]
}

// RevokeOpSuffix generates the 16-char suffix used for revoke-scoped object
// names from a revoke operation's ID.
func RevokeOpSuffix(operationID string) string {
	return strings.ReplaceAll(operationID, "-", "")[:16]
}
