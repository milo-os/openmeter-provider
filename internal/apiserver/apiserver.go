// SPDX-License-Identifier: AGPL-3.0-only

// Package apiserver is the usage.billing.miloapis.com aggregated API server.
// Milo proxies UsageQuery requests to it (via an APIService), after
// authenticating the caller and authorizing the request in its parent
// organization or project. It stores nothing: every UsageQuery is answered
// from OpenMeter, scoped with BillingAccounts and Projects read from Milo.
package apiserver

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	apiopenapi "k8s.io/apiserver/pkg/endpoints/openapi"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/klog/v2"
	openapicommon "k8s.io/kube-openapi/pkg/common"
	openapiutil "k8s.io/kube-openapi/pkg/util"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"go.miloapis.com/billing/api/usage/openapi"
	usagev1alpha1 "go.miloapis.com/billing/api/usage/v1alpha1"
	"go.miloapis.com/openmeter-provider/internal/openmeter"
	"go.miloapis.com/openmeter-provider/internal/registry/billingaccountbalance"
	"go.miloapis.com/openmeter-provider/internal/registry/usagequery"
)

var (
	// Scheme holds the types this server serves.
	Scheme = runtime.NewScheme()
	// Codecs serializes them.
	Codecs = serializer.NewCodecFactory(Scheme)
)

func init() {
	utilruntime.Must(usagev1alpha1.AddToScheme(Scheme))

	// The generic API server expects an internal version of every type it
	// serves; UsageQuery has a single version, so it doubles as internal.
	Scheme.AddKnownTypes(schema.GroupVersion{Group: usagev1alpha1.GroupName, Version: runtime.APIVersionInternal},
		&usagev1alpha1.UsageQuery{},
		&usagev1alpha1.UsageQueryList{},
		&usagev1alpha1.BillingAccountBalance{},
		&usagev1alpha1.BillingAccountBalanceList{},
	)

	metav1.AddToGroupVersion(Scheme, schema.GroupVersion{Version: "v1"})
	unversioned := schema.GroupVersion{Group: "", Version: "v1"}
	Scheme.AddUnversionedTypes(unversioned,
		&metav1.Status{},
		&metav1.APIVersions{},
		&metav1.APIGroupList{},
		&metav1.APIGroup{},
		&metav1.APIResourceList{},
	)
}

// ExtraConfig is what the usage server needs beyond the generic API server.
type ExtraConfig struct {
	// MiloCache reads BillingAccounts, MeterDefinitions and Projects from
	// Milo. It is started by the server.
	MiloCache cache.Cache
	// MiloObjects are the kinds MiloCache must have synced before the server
	// is ready.
	MiloObjects []client.Object
	// OpenMeter answers the usage queries.
	OpenMeter openmeter.Client
}

// Config combines the generic and usage-specific configuration.
type Config struct {
	GenericConfig *genericapiserver.RecommendedConfig
	ExtraConfig   ExtraConfig
}

type completedConfig struct {
	GenericConfig genericapiserver.CompletedConfig
	ExtraConfig   *ExtraConfig
}

// CompletedConfig can only be built by Complete.
type CompletedConfig struct {
	*completedConfig
}

// Complete fills in defaults.
func (cfg *Config) Complete() CompletedConfig {
	return CompletedConfig{&completedConfig{
		GenericConfig: cfg.GenericConfig.Complete(),
		ExtraConfig:   &cfg.ExtraConfig,
	}}
}

// UsageServer is the usage aggregated API server.
type UsageServer struct {
	GenericAPIServer *genericapiserver.GenericAPIServer
}

// New builds the server and installs the usage.billing.miloapis.com group.
func (c completedConfig) New() (*UsageServer, error) {
	genericServer, err := c.GenericConfig.New("openmeter-usage-apiserver", genericapiserver.NewEmptyDelegate())
	if err != nil {
		return nil, err
	}
	s := &UsageServer{GenericAPIServer: genericServer}

	apiGroupInfo := genericapiserver.NewDefaultAPIGroupInfo(usagev1alpha1.GroupName, Scheme, metav1.ParameterCodec, Codecs)
	apiGroupInfo.VersionedResourcesStorageMap[usagev1alpha1.SchemeGroupVersion.Version] = map[string]rest.Storage{
		"usagequeries":           usagequery.NewREST(c.ExtraConfig.MiloCache, c.ExtraConfig.OpenMeter),
		"billingaccountbalances": billingaccountbalance.NewREST(c.ExtraConfig.MiloCache, c.ExtraConfig.OpenMeter),
	}
	if err := s.GenericAPIServer.InstallAPIGroup(&apiGroupInfo); err != nil {
		return nil, err
	}

	// Serve only once the Milo cache has synced: before that, every query
	// would see no billing accounts and answer with no usage. Post-start
	// hooks gate /readyz.
	s.GenericAPIServer.AddPostStartHookOrDie("milo-cache", func(ctx genericapiserver.PostStartHookContext) error {
		go func() {
			if err := c.ExtraConfig.MiloCache.Start(ctx.Context); err != nil {
				klog.ErrorS(err, "Milo cache stopped")
			}
		}()
		// Informers start on first use; open them now and wait for each.
		for _, obj := range c.ExtraConfig.MiloObjects {
			if _, err := c.ExtraConfig.MiloCache.GetInformer(ctx.Context, obj); err != nil {
				return fmt.Errorf("watching %T in Milo: %w", obj, err)
			}
		}
		if !c.ExtraConfig.MiloCache.WaitForCacheSync(ctx.Context) {
			return fmt.Errorf("waiting for the Milo cache to sync")
		}
		klog.Info("Milo cache synced")
		return nil
	})

	return s, nil
}

// ConfigureOpenAPI serves OpenAPI v2 and v3 for the usage types, from the
// definitions billing generates.
//
// Definition names must be the REST-friendly form the scheme's namer knows,
// or server-side apply's type converter cannot find UsageQuery and every
// create logs "failed to update managedFields". The generated definitions
// are keyed by Go package path, so names are converted before the namer
// looks them up, and the definitions are rebuilt so their $refs use the
// same names. This follows the search API server.
func ConfigureOpenAPI(genericConfig *genericapiserver.RecommendedConfig, version string) {
	namer := apiopenapi.NewDefinitionNamer(Scheme)
	getDefinitionName := func(name string) (string, spec.Extensions) {
		if strings.Contains(name, "/") {
			name = openapiutil.ToRESTFriendlyName(name)
		}
		return namer.GetDefinitionName(name)
	}

	genericConfig.OpenAPIV3Config = genericapiserver.DefaultOpenAPIV3Config(openapi.GetOpenAPIDefinitions, namer)
	genericConfig.OpenAPIV3Config.Info.Title = "Usage"
	genericConfig.OpenAPIV3Config.Info.Version = version
	genericConfig.OpenAPIV3Config.GetDefinitionName = getDefinitionName
	genericConfig.OpenAPIV3Config.Definitions = openapi.GetOpenAPIDefinitions(func(name string) spec.Ref {
		defName, _ := getDefinitionName(name)
		return spec.MustCreateRef("#/components/schemas/" + openapicommon.EscapeJsonPointer(defName))
	})

	genericConfig.OpenAPIConfig = genericapiserver.DefaultOpenAPIConfig(openapi.GetOpenAPIDefinitions, namer)
	genericConfig.OpenAPIConfig.Info.Title = "Usage"
	genericConfig.OpenAPIConfig.Info.Version = version
	genericConfig.OpenAPIConfig.GetDefinitionName = getDefinitionName
	genericConfig.OpenAPIConfig.Definitions = openapi.GetOpenAPIDefinitions(func(name string) spec.Ref {
		defName, _ := getDefinitionName(name)
		return spec.MustCreateRef("#/definitions/" + openapicommon.EscapeJsonPointer(defName))
	})
}

// Run serves until ctx is done.
func (s *UsageServer) Run(ctx context.Context) error {
	return s.GenericAPIServer.PrepareRun().RunWithContext(ctx)
}
