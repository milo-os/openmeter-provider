// SPDX-License-Identifier: AGPL-3.0-only

// Package apiserver is the openmeter-provider binary's apiserver subcommand:
// the usage.billing.miloapis.com aggregated API server (internal/apiserver).
//
// It reuses the controller-manager's --server-config file for the Milo
// kubeconfig and the OpenMeter connection, and takes the standard generic
// API server flags for serving, authentication and authorization (delegated
// to Milo, as for any aggregated API server). It stores nothing, so it runs
// without etcd and without admission plugins.
package apiserver

import (
	"fmt"
	"os"
	// The image (distroless) has no time zone database, and UsageQuery
	// accepts any IANA spec.timeZone.
	_ "time/tzdata"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/options"
	basecompatibility "k8s.io/component-base/compatibility"
	"k8s.io/component-base/logs"
	logsapi "k8s.io/component-base/logs/api/v1"
	// Registers --logging-format=json, which the deployment uses.
	_ "k8s.io/component-base/logs/json/register"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	"go.miloapis.com/instrumentation/buildinfo"
	"go.miloapis.com/instrumentation/logging"
	usageapiserver "go.miloapis.com/openmeter-provider/internal/apiserver"
	"go.miloapis.com/openmeter-provider/internal/config"
	"go.miloapis.com/openmeter-provider/internal/openmeter"
	"go.miloapis.com/openmeter-provider/internal/registry/usagequery"
)

var (
	configScheme = runtime.NewScheme()
	configCodecs = serializer.NewCodecFactory(configScheme, serializer.EnableStrict)
)

func init() {
	utilruntime.Must(config.AddToScheme(configScheme))
	utilruntime.Must(config.RegisterDefaults(configScheme))
}

// Options are the apiserver subcommand's flags.
type Options struct {
	RecommendedOptions *options.RecommendedOptions
	Logs               *logsapi.LoggingConfiguration
	// ServerConfigFile is the controller-manager's config file, for the
	// Milo kubeconfig and the OpenMeter connection.
	ServerConfigFile string
	Version          string
}

// NewOptions returns the defaults.
func NewOptions(version string) *Options {
	o := &Options{
		RecommendedOptions: options.NewRecommendedOptions(
			"/registry/usage.billing.miloapis.com",
			usageapiserver.Codecs.LegacyCodec(usageapiserver.Scheme.PrioritizedVersionsAllGroups()...),
		),
		Logs:    logsapi.NewLoggingConfiguration(),
		Version: version,
	}
	// UsageQuery is never stored, and nothing needs admission or the core
	// Kubernetes API, so none of them are configured.
	o.RecommendedOptions.Etcd = nil
	o.RecommendedOptions.Admission = nil
	o.RecommendedOptions.CoreAPI = nil
	// Priority and fairness needs a core Kubernetes client.
	o.RecommendedOptions.Features.EnablePriorityAndFairness = false
	return o
}

// NewCommand returns the apiserver subcommand.
func NewCommand(version string) *cobra.Command {
	o := NewOptions(version)
	cmd := &cobra.Command{
		Use:   "apiserver",
		Short: "Run the usage.billing.miloapis.com aggregated API server (UsageQuery, answered from OpenMeter)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.Run(cmd)
		},
	}
	o.RecommendedOptions.AddFlags(cmd.Flags())
	logsapi.AddFlags(o.Logs, cmd.Flags())
	cmd.Flags().StringVar(&o.ServerConfigFile, "server-config", "", "Path to the openmeter-provider config file (Milo kubeconfig and OpenMeter connection).")
	return cmd
}

// Run starts the server and blocks until it stops.
func (o *Options) Run(cmd *cobra.Command) error {
	if err := logsapi.ValidateAndApply(o.Logs, nil); err != nil {
		return fmt.Errorf("applying logging configuration: %w", err)
	}
	defer logs.FlushLogs()
	buildinfo.SetVersion(o.Version)
	ctrl.SetLogger(logging.New(nil))

	serverConfig, err := o.serverConfig()
	if err != nil {
		return err
	}
	if serverConfig.OpenMeter == nil || serverConfig.OpenMeter.ServerURL == "" {
		return fmt.Errorf("the server config has no openMeter.serverUrl")
	}

	cfg, err := o.Config(serverConfig)
	if err != nil {
		return err
	}
	server, err := cfg.Complete().New()
	if err != nil {
		return err
	}

	klog.InfoS("Starting usage API server", "version", o.Version, "openMeter", serverConfig.OpenMeter.ServerURL)
	return server.Run(cmd.Context())
}

func (o *Options) serverConfig() (*config.OpenMeterProviderOperator, error) {
	var data []byte
	if o.ServerConfigFile != "" {
		var err error
		if data, err = os.ReadFile(o.ServerConfigFile); err != nil {
			return nil, fmt.Errorf("reading server config from %q: %w", o.ServerConfigFile, err)
		}
	}
	var serverConfig config.OpenMeterProviderOperator
	if err := runtime.DecodeInto(configCodecs.UniversalDecoder(), data, &serverConfig); err != nil {
		return nil, fmt.Errorf("decoding server config: %w", err)
	}
	return &serverConfig, nil
}

// Config builds the server configuration.
func (o *Options) Config(serverConfig *config.OpenMeterProviderOperator) (*usageapiserver.Config, error) {
	if err := o.RecommendedOptions.SecureServing.MaybeDefaultWithSelfSignedCerts("localhost", nil, nil); err != nil {
		return nil, fmt.Errorf("creating self-signed certificates: %w", err)
	}

	genericConfig := genericapiserver.NewRecommendedConfig(usageapiserver.Codecs)
	genericConfig.EffectiveVersion = basecompatibility.NewEffectiveVersionFromString("1.35", "", "")
	usageapiserver.ConfigureOpenAPI(genericConfig, o.Version)

	if err := o.RecommendedOptions.ApplyTo(genericConfig); err != nil {
		return nil, fmt.Errorf("applying generic API server options: %w", err)
	}

	miloConfig, err := serverConfig.RestConfig()
	if err != nil {
		return nil, fmt.Errorf("loading the Milo kubeconfig: %w", err)
	}
	miloScheme := runtime.NewScheme()
	if err := billingv1alpha1.AddToScheme(miloScheme); err != nil {
		return nil, err
	}
	miloCache, err := cache.New(miloConfig, cache.Options{Scheme: miloScheme})
	if err != nil {
		return nil, fmt.Errorf("creating the Milo cache: %w", err)
	}
	project := &unstructured.Unstructured{}
	project.SetGroupVersionKind(usagequery.ProjectGVK)

	openMeterClient, err := openmeter.NewClient(serverConfig.OpenMeter.ServerURL, serverConfig.OpenMeter.APISecret)
	if err != nil {
		return nil, fmt.Errorf("creating the OpenMeter client: %w", err)
	}

	return &usageapiserver.Config{
		GenericConfig: genericConfig,
		ExtraConfig: usageapiserver.ExtraConfig{
			MiloCache: miloCache,
			MiloObjects: []client.Object{
				&billingv1alpha1.BillingAccount{},
				&billingv1alpha1.MeterDefinition{},
				project,
			},
			OpenMeter: openMeterClient,
		},
	}, nil
}
