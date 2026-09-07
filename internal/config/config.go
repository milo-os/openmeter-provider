// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:defaulter-gen=true

// OpenMeterProviderOperator is the configuration for the openmeter-provider operator.
type OpenMeterProviderOperator struct {
	metav1.TypeMeta

	MetricsServer MetricsServerConfig `json:"metricsServer"`

	// KubeconfigPath is the path to the kubeconfig file pointing at the Milo
	// control plane API server where resources are stored. When empty, the
	// controller falls back to in-cluster config / $KUBECONFIG via
	// ctrl.GetConfig(), which is useful for local development.
	KubeconfigPath string `json:"kubeconfigPath,omitempty"`

	// OpenMeter configures the OpenMeter metering backend the controller
	// reconciles against. When unset, the meter-definition controller is not
	// registered.
	OpenMeter *OpenMeterConfig `json:"openMeter,omitempty"`
}

// OpenMeterConfig configures the connection to the OpenMeter API.
type OpenMeterConfig struct {
	// ServerURL is the base URL of the OpenMeter API, e.g.
	// "http://openmeter-api.openmeter-system.svc.cluster.local". Required
	// when the meter-definition controller is enabled.
	ServerURL string `json:"serverUrl,omitempty"`

	// APISecret is the bearer token OpenMeter expects. When empty the client
	// is constructed without auth — acceptable for the dev install, which
	// runs with authentication disabled. In production a real token (from
	// external-secrets) should always be set.
	APISecret string `json:"apiSecret,omitempty"`
}

// RestConfig returns the *rest.Config used to connect to the Milo control plane.
// When KubeconfigPath is empty it falls back to the standard
// controller-runtime config resolution (in-cluster / $KUBECONFIG).
func (c *OpenMeterProviderOperator) RestConfig() (*rest.Config, error) {
	if c.KubeconfigPath == "" {
		return ctrl.GetConfig()
	}
	return clientcmd.BuildConfigFromFlags("", c.KubeconfigPath)
}

// +k8s:deepcopy-gen=true

// MetricsServerConfig configures the metrics server.
type MetricsServerConfig struct {
	// SecureServing enables serving metrics via https.
	SecureServing *bool `json:"secureServing,omitempty"`

	// BindAddress is the bind address for the metrics server.
	BindAddress string `json:"bindAddress"`

	// TLS is the TLS configuration for the metrics server.
	TLS TLSConfig `json:"tls"`
}

func SetDefaults_MetricsServerConfig(obj *MetricsServerConfig) {
	if obj.SecureServing == nil {
		obj.SecureServing = ptr.To(true)
	}

	if obj.BindAddress == "" {
		obj.BindAddress = "0"
	}

	if len(obj.TLS.CertDir) == 0 {
		obj.TLS.CertDir = filepath.Join(os.TempDir(), "k8s-metrics-server", "serving-certs")
	}
}

func (c *MetricsServerConfig) Options(ctx context.Context, secretsClient client.Client) metricsserver.Options {
	secureServing := c.SecureServing != nil && *c.SecureServing

	opts := metricsserver.Options{
		SecureServing: secureServing,
		BindAddress:   c.BindAddress,
		CertDir:       c.TLS.CertDir,
		CertName:      c.TLS.CertName,
		KeyName:       c.TLS.KeyName,
	}

	if secureServing {
		opts.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	if secretRef := c.TLS.SecretRef; secretRef != nil {
		opts.TLSOpts = c.TLS.Options(ctx, secretsClient)
	}

	return opts
}

// +k8s:deepcopy-gen=true

// TLSConfig configures TLS certificate management.
type TLSConfig struct {
	// SecretRef is a reference to a secret that contains the server key and certificate.
	SecretRef *corev1.ObjectReference `json:"secretRef,omitempty"`

	// CertDir is the directory that contains the server key and certificate.
	CertDir string `json:"certDir"`

	// CertName is the server certificate name. Defaults to tls.crt.
	CertName string `json:"certName"`

	// KeyName is the server key name. Defaults to tls.key.
	KeyName string `json:"keyName"`
}

func (c *TLSConfig) Options(ctx context.Context, secretsClient client.Client) []func(*tls.Config) {
	var tlsOpts []func(*tls.Config)

	if secretRef := c.SecretRef; secretRef != nil {
		tlsOpts = append(tlsOpts, func(c *tls.Config) {
			logger := ctrl.Log.WithName("tls-client")
			c.GetCertificate = func(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
				logger.Info("getting certificate")

				var secret corev1.Secret
				secretObjectKey := types.NamespacedName{
					Name:      secretRef.Name,
					Namespace: secretRef.Namespace,
				}
				if err := secretsClient.Get(ctx, secretObjectKey, &secret); err != nil {
					return nil, fmt.Errorf("failed to get secret: %w", err)
				}

				cert, err := tls.X509KeyPair(secret.Data["tls.crt"], secret.Data["tls.key"])
				if err != nil {
					return nil, fmt.Errorf("failed to parse certificate: %w", err)
				}

				return &cert, nil
			}
		})
	}

	return tlsOpts
}

func SetDefaults_TLSConfig(obj *TLSConfig) {
	if len(obj.CertName) == 0 {
		obj.CertName = "tls.crt"
	}

	if len(obj.KeyName) == 0 {
		obj.KeyName = "tls.key"
	}
}

// SetDefaults_OpenMeterProviderOperator sets defaults for OpenMeterProviderOperator.
// The generated SetObjectDefaults_OpenMeterProviderOperator handles calling nested
// defaults (MetricsServerConfig, TLSConfig), so this
// function only sets top-level defaults.
func SetDefaults_OpenMeterProviderOperator(obj *OpenMeterProviderOperator) {
	// Top-level defaults are handled by nested SetDefaults_* functions
	// which are called by the generated SetObjectDefaults_OpenMeterProviderOperator.
}

func init() {
	SchemeBuilder.Register(&OpenMeterProviderOperator{})
}
