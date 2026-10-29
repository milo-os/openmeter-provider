// SPDX-License-Identifier: AGPL-3.0-only

package apiserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
	omv3 "github.com/openmeterio/openmeter/api/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/rest"
	basecompatibility "k8s.io/component-base/compatibility"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	usagev1alpha1 "go.miloapis.com/billing/api/usage/v1alpha1"
	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

// fakeCache is a Milo cache backed by a fake client.
type fakeCache struct {
	client.Reader
	*informertest.FakeInformers
}

func (c fakeCache) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return c.Reader.Get(ctx, key, obj, opts...)
}

func (c fakeCache) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.Reader.List(ctx, list, opts...)
}

type fakeOpenMeter struct {
	openmeter.Client
}

func (fakeOpenMeter) GetCustomer(context.Context, openmeter.CustomerKey) (om.Customer, error) {
	return om.Customer{Id: "01ACME"}, nil
}

func (fakeOpenMeter) GetCreditBalance(context.Context, openmeter.CustomerID) (string, error) {
	return "100.00", nil
}

func (fakeOpenMeter) ListInvoices(context.Context, openmeter.CustomerID) ([]om.Invoice, error) {
	return nil, nil
}

func (fakeOpenMeter) ListCreditGrants(context.Context, openmeter.CustomerID) ([]omv3.BillingCreditGrant, error) {
	return nil, nil
}

func (fakeOpenMeter) ListSubscriptions(context.Context, openmeter.CustomerKey) ([]om.Subscription, error) {
	return nil, nil
}

func (fakeOpenMeter) QueryMeter(_ context.Context, q openmeter.MeterQuery) ([]openmeter.MeterQueryRow, error) {
	return []openmeter.MeterQueryRow{{
		WindowStart: q.From,
		WindowEnd:   q.From.Add(24 * time.Hour),
		CustomerID:  "01ACME",
		GroupBy:     map[string]string{},
		Value:       18.2,
	}}, nil
}

// newHandler builds the server without authentication or authorization
// (those are Milo's job, and the generic server skips them when unset) and
// returns its handler, which runs requests as a user in organization acme.
func newHandler(t *testing.T) http.Handler {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := billingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	milo := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&billingv1alpha1.BillingAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "organization-acme", Name: "acme-main", UID: types.UID("uid-1")}},
		&billingv1alpha1.MeterDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: "cpu-hours"},
			Spec: billingv1alpha1.MeterDefinitionSpec{
				MeterName:   "compute.miloapis.com/cpu-hours",
				Phase:       billingv1alpha1.PhasePublished,
				Measurement: billingv1alpha1.MeterMeasurement{Aggregation: billingv1alpha1.MeterAggregationSum, Unit: "h"},
			},
		},
	).Build()

	genericConfig := genericapiserver.NewRecommendedConfig(Codecs)
	genericConfig.EffectiveVersion = basecompatibility.NewEffectiveVersionFromString("1.35", "", "")
	genericConfig.ExternalAddress = "127.0.0.1:443"
	genericConfig.LoopbackClientConfig = &rest.Config{Host: "https://127.0.0.1:443"}
	ConfigureOpenAPI(genericConfig, "test")

	cfg := &Config{
		GenericConfig: genericConfig,
		ExtraConfig: ExtraConfig{
			MiloCache: fakeCache{Reader: milo, FakeInformers: &informertest.FakeInformers{}},
			OpenMeter: fakeOpenMeter{},
		},
	}
	server, err := cfg.Complete().New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// PrepareRun installs the OpenAPI endpoints.
	handler := server.GenericAPIServer.PrepareRun().Handler
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(request.WithUser(r.Context(), &user.DefaultInfo{
			Name: "alice@example.com",
			Extra: map[string][]string{
				usagev1alpha1.ParentAPIGroupExtraKey: {"resourcemanager.miloapis.com"},
				usagev1alpha1.ParentTypeExtraKey:     {"Organization"},
				usagev1alpha1.ParentNameExtraKey:     {"acme"},
			},
		}))
		handler.ServeHTTP(w, r)
	})
}

func do(t *testing.T, h http.Handler, method, path string, body []byte) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out, _ := io.ReadAll(rec.Body)
	return rec.Code, out
}

func TestServer_CreateUsageQuery(t *testing.T) {
	h := newHandler(t)

	body := []byte(`{
		"apiVersion": "usage.billing.miloapis.com/v1alpha1",
		"kind": "UsageQuery",
		"metadata": {"name": "q"},
		"spec": {
			"startTime": "2026-10-05T00:00:00Z",
			"endTime": "2026-10-06T00:00:00Z",
			"granularity": "Day"
		}
	}`)
	code, out := do(t, h, http.MethodPost, "/apis/usage.billing.miloapis.com/v1alpha1/usagequeries", body)
	if code != http.StatusCreated {
		t.Fatalf("status = %d: %s", code, out)
	}

	var got usagev1alpha1.UsageQuery
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	if got.Kind != "UsageQuery" || got.APIVersion != "usage.billing.miloapis.com/v1alpha1" {
		t.Errorf("type = %s/%s", got.APIVersion, got.Kind)
	}
	if got.Spec.TimeZone != "UTC" {
		t.Errorf("defaults not applied: timeZone = %q", got.Spec.TimeZone)
	}
	if got.Status.Scope.Type != usagev1alpha1.UsageScopeOrganization || got.Status.Scope.Name != "acme" {
		t.Errorf("scope = %+v", got.Status.Scope)
	}
	if len(got.Status.Meters) != 1 || got.Status.Meters[0].Groupings[0].Series[0].Total != "18.2" {
		t.Errorf("meters = %+v", got.Status.Meters)
	}
}

func TestServer_InvalidQueryIs422(t *testing.T) {
	h := newHandler(t)
	body := []byte(`{"apiVersion": "usage.billing.miloapis.com/v1alpha1", "kind": "UsageQuery", "spec": {"startTime": "2026-10-05T00:00:00Z", "endTime": "2026-10-05T00:00:00Z"}}`)
	code, out := do(t, h, http.MethodPost, "/apis/usage.billing.miloapis.com/v1alpha1/usagequeries", body)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d: %s", code, out)
	}
}

func TestServer_OnlyCreateIsServed(t *testing.T) {
	h := newHandler(t)
	for _, method := range []string{http.MethodGet, http.MethodDelete, http.MethodPut} {
		path := "/apis/usage.billing.miloapis.com/v1alpha1/usagequeries"
		if method != http.MethodGet {
			path += "/q"
		}
		if code, _ := do(t, h, method, path, nil); code != http.StatusMethodNotAllowed && code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 405 or 404", method, path, code)
		}
	}
}

func TestServer_Discovery(t *testing.T) {
	h := newHandler(t)
	code, out := do(t, h, http.MethodGet, "/apis/usage.billing.miloapis.com/v1alpha1", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, out)
	}
	var list metav1.APIResourceList
	if err := json.Unmarshal(out, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.APIResources) != 2 {
		t.Fatalf("resources = %+v", list.APIResources)
	}
	resources := map[string]metav1.APIResource{}
	for _, res := range list.APIResources {
		resources[res.Name] = res
	}
	uq, ok := resources["usagequeries"]
	if !ok || uq.Kind != "UsageQuery" || uq.Namespaced || strings.Join(uq.Verbs, ",") != "create" {
		t.Errorf("usagequeries = %+v, want cluster-scoped usagequeries with only create", uq)
	}
	bab, ok := resources["billingaccountbalances"]
	if !ok || bab.Kind != "BillingAccountBalance" || !bab.Namespaced || strings.Join(bab.Verbs, ",") != "get,list" {
		t.Errorf("billingaccountbalances = %+v, want namespaced billingaccountbalances with get,list", bab)
	}
}

func TestServer_OpenAPI(t *testing.T) {
	h := newHandler(t)
	code, out := do(t, h, http.MethodGet, "/openapi/v3/apis/usage.billing.miloapis.com/v1alpha1", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, out)
	}
	if !strings.Contains(string(out), "UsageQuerySpec") {
		t.Errorf("OpenAPI v3 does not describe UsageQuerySpec")
	}
}

func TestServer_GetBillingAccountBalance(t *testing.T) {
	h := newHandler(t)
	code, out := do(t, h, http.MethodGet, "/apis/usage.billing.miloapis.com/v1alpha1/namespaces/organization-acme/billingaccountbalances/acme-main", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, out)
	}
	var bal usagev1alpha1.BillingAccountBalance
	if err := json.Unmarshal(out, &bal); err != nil {
		t.Fatal(err)
	}
	if bal.Name != "acme-main" || bal.Namespace != "organization-acme" {
		t.Errorf("name/namespace = %s/%s, want acme-main/organization-acme", bal.Name, bal.Namespace)
	}
	if bal.Status.Balance.CreditRemaining != "100.00" {
		t.Errorf("creditRemaining = %s, want 100.00", bal.Status.Balance.CreditRemaining)
	}
}

