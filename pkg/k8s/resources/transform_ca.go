package resources

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	caSecretVolumeName = "bridge-ca"
	caMountPath        = "/etc/bridge/tls"
)

// InjectCA returns a Transformer that generates a self-signed CA certificate
// and key, creates a Kubernetes Secret containing them, appends it to the
// bundle, and mounts it into the bridge proxy deployment's pod spec.
// Runs before SuffixNames — finds the deployment by tc.SourceName.
// SuffixNames will rename the Secret; RewriteRefs will update the volume reference.
func InjectCA(ns string) Transformer {
	return TransformFunc(func(tc *TransformContext, b *Bundle) error {
		certPEM, keyPEM, err := generateCA()
		if err != nil {
			return fmt.Errorf("generate CA: %w", err)
		}

		secretName := "bridge-ca"

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: ns,
			},
			Data: map[string][]byte{
				"ca.crt": certPEM,
				"ca.key": keyPEM,
			},
		}
		b.Resources = append(b.Resources, Resource{
			Object: secret,
			GVK:    corev1.SchemeGroupVersion.WithKind("Secret"),
		})

		// Mount the secret into the deployment. The volume references
		// secretName "bridge-ca" which SuffixNames + RewriteRefs will update.
		deploy, err := findApplicationDeployment(b, tc.SourceName)
		if err != nil {
			return &DeploymentNotFoundError{Name: tc.SourceName, Namespace: tc.SourceNamespace}
		}
		mountCASecret(deploy, secretName)

		return nil
	})
}

// mountCASecret adds the CA secret as a volume and mounts it into the first
// container at /etc/bridge/tls.
func mountCASecret(deploy *appsv1.Deployment, secretName string) {
	spec := &deploy.Spec.Template.Spec

	spec.Volumes = append(spec.Volumes, corev1.Volume{
		Name: caSecretVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: secretName,
			},
		},
	})

	if len(spec.Containers) > 0 {
		spec.Containers[0].VolumeMounts = append(spec.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name:      caSecretVolumeName,
			MountPath: caMountPath,
			ReadOnly:  true,
		})
	}
}

// generateCA creates a self-signed CA certificate and ECDSA private key,
// returning both as PEM-encoded bytes.
func generateCA() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"Bridge"},
			CommonName:   "Bridge Mock CA",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create certificate: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	return certPEM, keyPEM, nil
}
