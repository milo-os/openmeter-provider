// SPDX-License-Identifier: AGPL-3.0-only

// Command submission-consumer runs the standalone NATS JetStream usage-event
// ingestion consumer (internal/submission). It is a separate binary/Deployment
// from the controller-manager so the two workloads can scale and fail
// independently: the controller-manager reconciles CRDs, while this consumer
// is a long-running message-processing loop with no reconcile loop of its own.
//
// Mirrors amberflo-provider's cmd/submission-consumer/main.go: a plain
// func main() (not a cobra subcommand), leader election disabled (multiple
// replicas may run as independent JetStream pull-consumer workers sharing
// the same durable consumer), and a manager used only for its cache/metrics/
// healthz machinery plus mgr.Add(submissionConsumer) to run the consumer as
// a Runnable.
package main

import (
	"flag"
	"fmt"
	"os"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	natsgo "github.com/nats-io/nats.go"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"

	"go.miloapis.com/openmeter-provider/internal/config"
	"go.miloapis.com/openmeter-provider/internal/openmeter"
	"go.miloapis.com/openmeter-provider/internal/submission"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
	codecs   = serializer.NewCodecFactory(scheme, serializer.EnableStrict)

	// Build metadata, set via -ldflags at build time. See Dockerfile and
	// Taskfile.yaml.
	version      = "dev"
	gitCommit    = "unknown"
	gitTreeState = "unknown"
	buildDate    = "unknown"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(config.AddToScheme(scheme))
	utilruntime.Must(config.RegisterDefaults(scheme))

	utilruntime.Must(billingv1alpha1.AddToScheme(scheme))
}

func main() {
	var probeAddr string
	var serverConfigFile string

	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.StringVar(&serverConfigFile, "server-config", "", "Path to the server config file (YAML).")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	setupLog.Info("starting submission-consumer",
		"version", version,
		"gitCommit", gitCommit,
		"gitTreeState", gitTreeState,
		"buildDate", buildDate,
	)

	var serverConfig config.OpenMeterProviderOperator
	var configData []byte
	if len(serverConfigFile) > 0 {
		var err error
		configData, err = os.ReadFile(serverConfigFile)
		if err != nil {
			setupLog.Error(fmt.Errorf("unable to read server config from %q", serverConfigFile), "")
			os.Exit(1)
		}
	}

	if err := runtime.DecodeInto(codecs.UniversalDecoder(), configData, &serverConfig); err != nil {
		setupLog.Error(err, "unable to decode server config")
		os.Exit(1)
	}

	setupLog.Info("server config", "config", serverConfig)

	if serverConfig.OpenMeter == nil {
		setupLog.Error(fmt.Errorf("openMeter must be configured"), "")
		os.Exit(1)
	}
	if serverConfig.Nats.URL == "" {
		setupLog.Error(fmt.Errorf("nats.url must be configured"), "")
		os.Exit(1)
	}

	cfg, err := serverConfig.RestConfig()
	if err != nil {
		setupLog.Error(err, "unable to load rest config")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()

	bootstrapClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to create bootstrap client")
		os.Exit(1)
	}

	// The metrics endpoint's authn/authz filter must validate scrape
	// requests against the LOCAL cluster (where the scraper and this pod
	// live), not the Milo control plane that `cfg` may point at. Resolve
	// the local config via the standard in-cluster / KUBECONFIG resolution.
	metricsAuthConfig, err := ctrl.GetConfig()
	if err != nil {
		setupLog.Error(err, "unable to load local rest config for metrics auth")
		os.Exit(1)
	}

	metricsServerOptions := serverConfig.MetricsServer.Options(ctx, bootstrapClient, metricsAuthConfig)

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         false, // Independent JetStream pull-consumer workers; no single leader needed.
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	openMeterClient, err := openmeter.NewClient(
		serverConfig.OpenMeter.ServerURL,
		serverConfig.OpenMeter.APISecret,
	)
	if err != nil {
		setupLog.Error(err, "unable to construct OpenMeter client")
		os.Exit(1)
	}

	nc, natsErr := connectNATS(&serverConfig.Nats)
	if natsErr != nil {
		setupLog.Error(natsErr, "unable to connect to NATS",
			"url", serverConfig.Nats.URL)
		os.Exit(1)
	}
	defer nc.Drain() //nolint:errcheck

	meterCache, cacheErr := submission.NewMeterDefinitionCache(ctx, mgr.GetCache())
	if cacheErr != nil {
		setupLog.Error(cacheErr, "unable to create MeterDefinitionCache")
		os.Exit(1)
	}

	submissionConsumer := &submission.SubmissionConsumer{
		Cache:        mgr.GetCache(),
		NC:           nc,
		IngestClient: openMeterClient,
		MeterCache:   meterCache,
		Logger:       ctrl.Log.WithName("submission-consumer"),
		FetchBatch:   serverConfig.SubmissionBatchSize,
		RetryAfter:   serverConfig.SubmissionRetryAfter.Duration,
		AckWait:      serverConfig.SubmissionAckWait.Duration,
		FetchTimeout: serverConfig.SubmissionFetchTimeout.Duration,
	}
	if addErr := mgr.Add(submissionConsumer); addErr != nil {
		setupLog.Error(addErr, "unable to add submission consumer to manager")
		os.Exit(1)
	}
	setupLog.Info("submission consumer registered",
		"natsURL", serverConfig.Nats.URL,
		"openMeterServer", serverConfig.OpenMeter.ServerURL,
	)

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}
}

func connectNATS(cfg *config.NATSConfig) (*natsgo.Conn, error) {
	opts := []natsgo.Option{natsgo.Name("submission-consumer")}
	if cfg.CAFile != "" || cfg.CertFile != "" || cfg.KeyFile != "" {
		opts = append(opts, natsgo.RootCAs(cfg.CAFile))
		opts = append(opts, natsgo.ClientCert(cfg.CertFile, cfg.KeyFile))
	}
	return natsgo.Connect(cfg.URL, opts...)
}
