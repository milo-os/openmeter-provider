// SPDX-License-Identifier: AGPL-3.0-only

package controllermanager

import (
	"flag"
	"fmt"
	"os"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	"go.miloapis.com/openmeter-provider/internal/config"
	"go.miloapis.com/openmeter-provider/internal/controller"
	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

var (
	scheme = runtime.NewScheme()
	codecs = serializer.NewCodecFactory(scheme, serializer.EnableStrict)
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(config.AddToScheme(scheme))
	utilruntime.Must(config.RegisterDefaults(scheme))
	utilruntime.Must(billingv1alpha1.AddToScheme(scheme))
}

func newControllerManagerCommand(info BuildInfo) *cobra.Command {
	var (
		enableLeaderElection    bool
		leaderElectionNamespace string
		probeAddr               string
		serverConfigFile        string
	)

	opts := zap.Options{
		Development: true,
	}

	cmd := &cobra.Command{
		Use:   "controller-manager",
		Short: "Run the openmeter-provider controller manager (controller-runtime manager)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

			setupLog := ctrl.Log.WithName("setup")
			setupLog.Info("starting openmeter-provider operator",
				"version", info.Version,
				"gitCommit", info.GitCommit,
				"gitTreeState", info.GitTreeState,
				"buildDate", info.BuildDate,
			)

			var serverConfig config.OpenMeterProviderOperator
			var configData []byte
			if len(serverConfigFile) > 0 {
				var err error
				configData, err = os.ReadFile(serverConfigFile)
				if err != nil {
					return fmt.Errorf("reading server config from %q: %w", serverConfigFile, err)
				}
			}

			if err := runtime.DecodeInto(codecs.UniversalDecoder(), configData, &serverConfig); err != nil {
				return fmt.Errorf("decoding server config: %w", err)
			}

			setupLog.Info("server config loaded", "kubeconfigPath", serverConfig.KubeconfigPath)

			cfg, err := serverConfig.RestConfig()
			if err != nil {
				return fmt.Errorf("loading rest config: %w", err)
			}

			ctx := ctrl.SetupSignalHandler()

			bootstrapClient, err := client.New(cfg, client.Options{Scheme: scheme})
			if err != nil {
				return fmt.Errorf("creating bootstrap client: %w", err)
			}

			metricsServerOptions := serverConfig.MetricsServer.Options(ctx, bootstrapClient)

			mgr, err := ctrl.NewManager(cfg, ctrl.Options{
				Scheme:                  scheme,
				Metrics:                 metricsServerOptions,
				HealthProbeBindAddress:  probeAddr,
				LeaderElection:          enableLeaderElection,
				LeaderElectionID:        "openmeter-provider.miloapis.com",
				LeaderElectionNamespace: leaderElectionNamespace,
			})
			if err != nil {
				return fmt.Errorf("starting manager: %w", err)
			}

			if serverConfig.OpenMeter != nil {
				openMeterClient, err := openmeter.NewClient(
					serverConfig.OpenMeter.ServerURL,
					serverConfig.OpenMeter.APISecret,
				)
				if err != nil {
					return fmt.Errorf("creating OpenMeter client: %w", err)
				}
				if err = (&controller.MeterDefinitionReconciler{
					OpenMeterClient: openMeterClient,
				}).SetupWithManager(mgr); err != nil {
					return fmt.Errorf("creating MeterDefinition controller: %w", err)
				}
				setupLog.Info("OpenMeter meter-definition controller registered",
					"server", serverConfig.OpenMeter.ServerURL)
			} else {
				setupLog.Info("openMeter not configured; meter-definition controller disabled")
			}

			if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
				return fmt.Errorf("setting up health check: %w", err)
			}
			if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
				return fmt.Errorf("setting up ready check: %w", err)
			}

			setupLog.Info("starting manager")
			if err := mgr.Start(ctx); err != nil {
				return fmt.Errorf("running manager: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	cmd.Flags().BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	cmd.Flags().StringVar(&leaderElectionNamespace, "leader-elect-namespace", "", "The namespace to use for leader election.")
	cmd.Flags().StringVar(&serverConfigFile, "server-config", "", "Path to the server config file.")

	// zap.Options.BindFlags accepts *flag.FlagSet (stdlib). Bridge via pflag's
	// AddGoFlagSet so the zap flags are surfaced on the cobra command.
	zapFlags := flag.NewFlagSet("zap", flag.ContinueOnError)
	opts.BindFlags(zapFlags)
	cmd.Flags().AddGoFlagSet(zapFlags)

	return cmd
}
