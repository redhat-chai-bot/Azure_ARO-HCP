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
	certificatesv1alpha1 "github.com/openshift/hypershift/api/certificates/v1alpha1"
	certificatesv1 "k8s.io/api/certificates/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	clientcmd "k8s.io/client-go/tools/clientcmd"
)

const (
	ownerAnnotationKey = "aro-hcp.openshift.io/owner"
	signerNamePrefix   = "hypershift.openshift.io/"
	signerNameSuffix   = ".customer-break-glass"
	signerClass        = "customer-break-glass"
)

func requireOwner(owner *azcorearm.ResourceID) {
	if owner == nil {
		panic("owner ResourceID must not be nil")
	}
}

func ownerAnnotation(owner *azcorearm.ResourceID) map[string]string {
	return map[string]string{
		ownerAnnotationKey: strings.ToLower(owner.String()),
	}
}

// GenerateKeypair generates an RSA 2048-bit keypair and returns PEM-encoded public and private keys.
func GenerateKeypair() (publicPEM, privatePEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("generating RSA key: %w", err)
	}

	privDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshaling private key: %w", err)
	}
	privatePEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})

	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, nil, fmt.Errorf("marshaling public key: %w", err)
	}
	publicPEM = pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})

	return publicPEM, privatePEM, nil
}

// BuildCSR builds a CertificateSigningRequest for a system admin credential.
// It takes the private key PEM to generate the PKCS#10 CSR, which is then
// embedded in the Kubernetes CSR object.
func BuildCSR(owner *azcorearm.ResourceID, hcpNamespace, credName, username string, privateKeyPEM []byte) (*certificatesv1.CertificateSigningRequest, error) {
	requireOwner(owner)

	block, _ := pem.Decode(privateKeyPEM)
	if block == nil {
		return nil, fmt.Errorf("failed to decode private key PEM")
	}
	privateKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing private key: %w", err)
	}

	csrTemplate := &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:   username,
			Organization: []string{"system:masters"},
		},
	}

	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTemplate, privateKey)
	if err != nil {
		return nil, fmt.Errorf("creating certificate request: %w", err)
	}

	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	expirationSeconds := int32(86400) // 24 hours
	return &certificatesv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:        credName,
			Annotations: ownerAnnotation(owner),
		},
		Spec: certificatesv1.CertificateSigningRequestSpec{
			Request:           csrPEM,
			SignerName:        signerNamePrefix + hcpNamespace + signerNameSuffix,
			Usages:            []certificatesv1.KeyUsage{certificatesv1.UsageClientAuth},
			ExpirationSeconds: &expirationSeconds,
		},
	}, nil
}

// BuildKubeconfig assembles a kubeconfig from the signed certificate, private key, CA bundle, and API URL.
func BuildKubeconfig(apiURL string, caBundle, signedCert, privateKeyPEM []byte) ([]byte, error) {
	config := clientcmdapi.NewConfig()
	config.Clusters["cluster"] = &clientcmdapi.Cluster{
		Server:                   apiURL,
		CertificateAuthorityData: caBundle,
	}
	config.AuthInfos["admin"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: signedCert,
		ClientKeyData:         privateKeyPEM,
	}
	config.Contexts["default"] = &clientcmdapi.Context{
		Cluster:  "cluster",
		AuthInfo: "admin",
	}
	config.CurrentContext = "default"

	return clientcmd.Write(*config)
}

// BuildCSRA builds a CertificateSigningRequestApproval for a system admin credential.
func BuildCSRA(owner *azcorearm.ResourceID, hcpNamespace, credName string) *certificatesv1alpha1.CertificateSigningRequestApproval {
	requireOwner(owner)
	return &certificatesv1alpha1.CertificateSigningRequestApproval{
		ObjectMeta: metav1.ObjectMeta{
			Name:        credName,
			Namespace:   hcpNamespace,
			Annotations: ownerAnnotation(owner),
		},
	}
}

// BuildRevocationRequest builds a CertificateRevocationRequest to revoke all customer-signer certs.
func BuildRevocationRequest(owner *azcorearm.ResourceID, hcpNamespace, revokeOpSuffix string) *certificatesv1alpha1.CertificateRevocationRequest {
	requireOwner(owner)
	return &certificatesv1alpha1.CertificateRevocationRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "customer-break-glass-revocation-" + revokeOpSuffix,
			Namespace:   hcpNamespace,
			Annotations: ownerAnnotation(owner),
		},
		Spec: certificatesv1alpha1.CertificateRevocationRequestSpec{
			SignerClass: signerClass,
		},
	}
}

// BuildRBACGiveCSRPerm returns a ClusterRole + ClusterRoleBinding that grants permission to manage CSRs.
func BuildRBACGiveCSRPerm(owner *azcorearm.ResourceID, credName string) []runtime.Object {
	requireOwner(owner)
	annotations := ownerAnnotation(owner)
	name := "system-admin-credential-give-csr-perm-" + credName
	return []runtime.Object{
		&rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Annotations: annotations,
			},
			Rules: []rbacv1.PolicyRule{
				{
					APIGroups: []string{"certificates.k8s.io"},
					Resources: []string{"certificatesigningrequests"},
					Verbs:     []string{"get", "list", "watch", "create", "update", "delete"},
				},
			},
		},
		&rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Annotations: annotations,
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "ClusterRole",
				Name:     name,
			},
			Subjects: []rbacv1.Subject{
				{
					Kind:      "Group",
					APIGroup:  "rbac.authorization.k8s.io",
					Name:      "system:serviceaccounts:klusterlet-agent",
				},
			},
		},
	}
}

// BuildRBACCSRA returns a Role + RoleBinding granting CSRA permissions in the HCP namespace.
func BuildRBACCSRA(owner *azcorearm.ResourceID, hcpNamespace, credName string) []runtime.Object {
	requireOwner(owner)
	annotations := ownerAnnotation(owner)
	name := "system-admin-credential-csra-perm-" + credName
	return []runtime.Object{
		&rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Namespace:   hcpNamespace,
				Annotations: annotations,
			},
			Rules: []rbacv1.PolicyRule{
				{
					APIGroups: []string{"certificates.hypershift.openshift.io"},
					Resources: []string{"certificatesigningrequestapprovals"},
					Verbs:     []string{"get", "list", "watch", "create", "update", "delete"},
				},
			},
		},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Namespace:   hcpNamespace,
				Annotations: annotations,
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "Role",
				Name:     name,
			},
			Subjects: []rbacv1.Subject{
				{
					Kind:      "Group",
					APIGroup:  "rbac.authorization.k8s.io",
					Name:      "system:serviceaccounts:klusterlet-agent",
				},
			},
		},
	}
}

// BuildRBACRevocation returns a Role + RoleBinding granting CRR permissions in the HCP namespace.
func BuildRBACRevocation(owner *azcorearm.ResourceID, hcpNamespace, credName string) []runtime.Object {
	requireOwner(owner)
	annotations := ownerAnnotation(owner)
	name := "system-admin-credential-revocation-perm-" + credName
	return []runtime.Object{
		&rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Namespace:   hcpNamespace,
				Annotations: annotations,
			},
			Rules: []rbacv1.PolicyRule{
				{
					APIGroups: []string{"certificates.hypershift.openshift.io"},
					Resources: []string{"certificaterevocationrequests"},
					Verbs:     []string{"get", "list", "watch", "create", "update", "delete"},
				},
			},
		},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Namespace:   hcpNamespace,
				Annotations: annotations,
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "Role",
				Name:     name,
			},
			Subjects: []rbacv1.Subject{
				{
					Kind:      "Group",
					APIGroup:  "rbac.authorization.k8s.io",
					Name:      "system:serviceaccounts:klusterlet-agent",
				},
			},
		},
	}
}
