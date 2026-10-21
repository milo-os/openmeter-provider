// SPDX-License-Identifier: AGPL-3.0-only

package usagequery

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	usagev1alpha1 "go.miloapis.com/billing/api/usage/v1alpha1"
	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

var (
	start = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	end   = start.Add(2 * 24 * time.Hour)
)

// fakeOpenMeter answers GetCustomer from customers (keyed by account UID),
// QueryMeter with query, recording every query, and the subscription and
// plan rate card calls from subscriptions (keyed by account UID).
type fakeOpenMeter struct {
	openmeter.Client

	customers       map[string]string
	query           func(q openmeter.MeterQuery) ([]openmeter.MeterQueryRow, error)
	subscriptions   map[string][]fakeSubscription
	subscriptionErr error

	mu                sync.Mutex
	queries           []openmeter.MeterQuery
	customerCalls     int
	subscriptionCalls int
}

func (f *fakeOpenMeter) ListSubscriptions(_ context.Context, key openmeter.CustomerKey) ([]om.Subscription, error) {
	f.mu.Lock()
	f.subscriptionCalls++
	f.mu.Unlock()
	if f.subscriptionErr != nil {
		return nil, f.subscriptionErr
	}
	var out []om.Subscription
	for _, s := range f.subscriptions[string(key)] {
		out = append(out, s.sub)
	}
	return out, nil
}

func (f *fakeOpenMeter) PlanUsageRateCards(_ context.Context, planID string) ([]openmeter.UsageRateCard, error) {
	for _, subs := range f.subscriptions {
		for _, s := range subs {
			if s.sub.Plan != nil && s.sub.Plan.Id == planID {
				return s.cards, nil
			}
		}
	}
	return nil, fmt.Errorf("plan %s not found", planID)
}

func (f *fakeOpenMeter) GetCustomer(_ context.Context, key openmeter.CustomerKey) (om.Customer, error) {
	f.mu.Lock()
	f.customerCalls++
	f.mu.Unlock()
	id, ok := f.customers[string(key)]
	if !ok {
		return om.Customer{}, openmeter.ErrCustomerNotFound
	}
	return om.Customer{Id: id}, nil
}

func (f *fakeOpenMeter) QueryMeter(_ context.Context, q openmeter.MeterQuery) ([]openmeter.MeterQueryRow, error) {
	f.mu.Lock()
	f.queries = append(f.queries, q)
	f.mu.Unlock()
	if f.query == nil {
		return nil, nil
	}
	return f.query(q)
}

func (f *fakeOpenMeter) queryCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queries)
}

func account(namespace, name string) *billingv1alpha1.BillingAccount {
	return &billingv1alpha1.BillingAccount{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace,
		Name:      name,
		UID:       types.UID(namespace + "/" + name),
	}}
}

func project(name, org string) *unstructured.Unstructured {
	p := &unstructured.Unstructured{}
	p.SetGroupVersionKind(ProjectGVK)
	p.SetName(name)
	if org != "" {
		p.SetLabels(map[string]string{organizationNameLabel: org})
	}
	return p
}

func meterDefinition(name string, agg billingv1alpha1.MeterAggregation, phase billingv1alpha1.Phase, dims ...string) *billingv1alpha1.MeterDefinition {
	return &billingv1alpha1.MeterDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: billingv1alpha1.MeterDefinitionSpec{
			MeterName: "compute.miloapis.com/" + name,
			Phase:     phase,
			Measurement: billingv1alpha1.MeterMeasurement{
				Aggregation: agg,
				Unit:        "h",
				Dimensions:  dims,
			},
		},
	}
}

// world is two organizations: acme (two accounts, projects web and api) and
// globex (one account, project shop), and meters in every phase. egress-gb
// has a dimension whose OpenMeter group-by key differs from its name.
func world(t *testing.T) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := billingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(ProjectGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(ProjectGVK.GroupVersion().WithKind("ProjectList"), &unstructured.UnstructuredList{})

	legacy := project("legacy", "")
	if err := unstructured.SetNestedField(legacy.Object, "Organization", "spec", "ownerRef", "kind"); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(legacy.Object, "acme", "spec", "ownerRef", "name"); err != nil {
		t.Fatal(err)
	}

	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		account("organization-acme", "acme-main"),
		account("organization-acme", "acme-eu"),
		account("organization-globex", "globex-main"),
		project("web", "acme"),
		project("api", "acme"),
		project("shop", "globex"),
		legacy,
		meterDefinition("cpu-hours", billingv1alpha1.MeterAggregationSum, billingv1alpha1.PhasePublished, "region"),
		meterDefinition("peak-storage", billingv1alpha1.MeterAggregationMax, billingv1alpha1.PhasePublished),
		meterDefinition("egress-gb", billingv1alpha1.MeterAggregationSum, billingv1alpha1.PhasePublished, egressRegion),
		meterDefinition("old-meter", billingv1alpha1.MeterAggregationSum, billingv1alpha1.PhaseDeprecated),
		meterDefinition("draft-meter", billingv1alpha1.MeterAggregationSum, billingv1alpha1.PhaseDraft),
		meterDefinition("retired-meter", billingv1alpha1.MeterAggregationSum, billingv1alpha1.PhaseRetired),
	).Build()
}

// everyCustomer gives every account in world an OpenMeter customer.
func everyCustomer() map[string]string {
	return map[string]string{
		"organization-acme/acme-main":     "01ACMEMAIN",
		"organization-acme/acme-eu":       "01ACMEEU",
		"organization-globex/globex-main": "01GLOBEX",
	}
}

func inParent(kind, name string) context.Context {
	extra := map[string][]string{}
	if kind != "" {
		extra = map[string][]string{
			usagev1alpha1.ParentAPIGroupExtraKey: {"resourcemanager.miloapis.com"},
			usagev1alpha1.ParentTypeExtraKey:     {kind},
			usagev1alpha1.ParentNameExtraKey:     {name},
		}
	}
	return request.WithUser(context.Background(), &user.DefaultInfo{Name: "alice@example.com", Extra: extra})
}

func baseQuery() *usagev1alpha1.UsageQuery {
	return &usagev1alpha1.UsageQuery{Spec: usagev1alpha1.UsageQuerySpec{
		StartTime:   metav1.NewTime(start),
		EndTime:     metav1.NewTime(end),
		Granularity: usagev1alpha1.UsageGranularityDay,
	}}
}

func create(t *testing.T, ctx context.Context, r *REST, q *usagev1alpha1.UsageQuery) (*usagev1alpha1.UsageQuery, error) {
	t.Helper()
	obj, err := r.Create(ctx, q, nil, nil)
	if err != nil {
		return nil, err
	}
	return obj.(*usagev1alpha1.UsageQuery), nil
}

func accountNames(refs []usagev1alpha1.BillingAccountReference) []string {
	var out []string
	for _, ref := range refs {
		out = append(out, ref.Namespace+"/"+ref.Name)
	}
	return out
}

func customersQueried(f *fakeOpenMeter) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[string]bool{}
	for _, q := range f.queries {
		for _, id := range q.CustomerIDs {
			seen[string(id)] = true
		}
	}
	return slices.Sorted(func(yield func(string) bool) {
		for id := range seen {
			if !yield(id) {
				return
			}
		}
	})
}

func projectFilters(f *fakeOpenMeter) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, q := range f.queries {
		out = append(out, q.FilterGroupBy["project_name"])
	}
	return out
}

func TestCreate_Scope(t *testing.T) {
	tests := []struct {
		name   string
		ctx    context.Context
		mutate func(q *usagev1alpha1.UsageQuerySpec)

		wantErr      func(error) bool
		wantAccounts []string
		// wantCustomers are the OpenMeter customers queried.
		wantCustomers []string
		wantProjects  []string
	}{
		{
			name:          "organization reads all of its accounts",
			ctx:           inParent("Organization", "acme"),
			wantAccounts:  []string{"organization-acme/acme-eu", "organization-acme/acme-main"},
			wantCustomers: []string{"01ACMEEU", "01ACMEMAIN"},
		},
		{
			name: "organization narrows to one account",
			ctx:  inParent("Organization", "acme"),
			mutate: func(s *usagev1alpha1.UsageQuerySpec) {
				s.BillingAccounts = []usagev1alpha1.BillingAccountReference{{Name: "acme-eu"}}
			},
			wantAccounts:  []string{"organization-acme/acme-eu"},
			wantCustomers: []string{"01ACMEEU"},
		},
		{
			name:          "organization narrows to one of its projects",
			ctx:           inParent("Organization", "acme"),
			mutate:        func(s *usagev1alpha1.UsageQuerySpec) { s.Projects = []string{"web"} },
			wantAccounts:  []string{"organization-acme/acme-eu", "organization-acme/acme-main"},
			wantCustomers: []string{"01ACMEEU", "01ACMEMAIN"},
			wantProjects:  []string{"web"},
		},
		{
			name:          "organization owns a project by its ownerRef when the label is missing",
			ctx:           inParent("Organization", "acme"),
			mutate:        func(s *usagev1alpha1.UsageQuerySpec) { s.Projects = []string{"legacy"} },
			wantAccounts:  []string{"organization-acme/acme-eu", "organization-acme/acme-main"},
			wantCustomers: []string{"01ACMEEU", "01ACMEMAIN"},
			wantProjects:  []string{"legacy"},
		},
		{
			name: "organization cannot read another organization's account",
			ctx:  inParent("Organization", "acme"),
			mutate: func(s *usagev1alpha1.UsageQuerySpec) {
				s.BillingAccounts = []usagev1alpha1.BillingAccountReference{{Namespace: "organization-globex", Name: "globex-main"}}
			},
			wantErr: apierrors.IsForbidden,
		},
		{
			name:    "organization cannot read another organization's project",
			ctx:     inParent("Organization", "acme"),
			mutate:  func(s *usagev1alpha1.UsageQuerySpec) { s.Projects = []string{"shop"} },
			wantErr: apierrors.IsForbidden,
		},
		{
			name:    "an unknown project looks like another organization's",
			ctx:     inParent("Organization", "acme"),
			mutate:  func(s *usagev1alpha1.UsageQuerySpec) { s.Projects = []string{"nope"} },
			wantErr: apierrors.IsForbidden,
		},
		{
			name: "organization naming a missing account of its own",
			ctx:  inParent("Organization", "acme"),
			mutate: func(s *usagev1alpha1.UsageQuerySpec) {
				s.BillingAccounts = []usagev1alpha1.BillingAccountReference{{Name: "gone"}}
			},
			wantErr: apierrors.IsNotFound,
		},
		{
			name:          "project reads its usage on every account of its organization",
			ctx:           inParent("Project", "web"),
			wantCustomers: []string{"01ACMEEU", "01ACMEMAIN"},
			wantProjects:  []string{"web"},
		},
		{
			name:          "project may repeat its own name",
			ctx:           inParent("Project", "web"),
			mutate:        func(s *usagev1alpha1.UsageQuerySpec) { s.Projects = []string{"web"} },
			wantCustomers: []string{"01ACMEEU", "01ACMEMAIN"},
			wantProjects:  []string{"web"},
		},
		{
			name:    "project cannot read another project",
			ctx:     inParent("Project", "web"),
			mutate:  func(s *usagev1alpha1.UsageQuerySpec) { s.Projects = []string{"api"} },
			wantErr: apierrors.IsForbidden,
		},
		{
			name: "project cannot name billing accounts",
			ctx:  inParent("Project", "web"),
			mutate: func(s *usagev1alpha1.UsageQuerySpec) {
				s.BillingAccounts = []usagev1alpha1.BillingAccountReference{{Name: "acme-main"}}
			},
			wantErr: apierrors.IsForbidden,
		},
		{
			name:    "platform must narrow the query",
			ctx:     inParent("", ""),
			wantErr: apierrors.IsBadRequest,
		},
		{
			name: "platform must give account namespaces",
			ctx:  inParent("", ""),
			mutate: func(s *usagev1alpha1.UsageQuerySpec) {
				s.BillingAccounts = []usagev1alpha1.BillingAccountReference{{Name: "acme-main"}}
			},
			wantErr: apierrors.IsBadRequest,
		},
		{
			name: "platform reads any named account",
			ctx:  inParent("", ""),
			mutate: func(s *usagev1alpha1.UsageQuerySpec) {
				s.BillingAccounts = []usagev1alpha1.BillingAccountReference{{Namespace: "organization-globex", Name: "globex-main"}}
			},
			wantAccounts:  []string{"organization-globex/globex-main"},
			wantCustomers: []string{"01GLOBEX"},
		},
		{
			name:          "platform reads a project's usage on its organization's accounts",
			ctx:           inParent("", ""),
			mutate:        func(s *usagev1alpha1.UsageQuerySpec) { s.Projects = []string{"shop", "web"} },
			wantAccounts:  []string{"organization-acme/acme-eu", "organization-acme/acme-main", "organization-globex/globex-main"},
			wantCustomers: []string{"01ACMEEU", "01ACMEMAIN", "01GLOBEX"},
			wantProjects:  []string{"shop", "web"},
		},
		{
			name:    "a user parent is rejected",
			ctx:     request.WithUser(context.Background(), &user.DefaultInfo{Name: "bob", Extra: map[string][]string{usagev1alpha1.ParentAPIGroupExtraKey: {"iam.miloapis.com"}, usagev1alpha1.ParentTypeExtraKey: {"User"}, usagev1alpha1.ParentNameExtraKey: {"bob"}}}),
			wantErr: apierrors.IsForbidden,
		},
		{
			name:    "no user is unauthorized",
			ctx:     context.Background(),
			wantErr: apierrors.IsUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			omc := &fakeOpenMeter{customers: everyCustomer()}
			r := NewREST(world(t), omc)

			q := baseQuery()
			q.Spec.Meters = []string{"cpu-hours"}
			if tt.mutate != nil {
				tt.mutate(&q.Spec)
			}

			got, err := create(t, tt.ctx, r, q)
			if tt.wantErr != nil {
				if !tt.wantErr(err) {
					t.Fatalf("err = %v, wrong kind", err)
				}
				if omc.queryCount() != 0 {
					t.Errorf("a rejected query still reached OpenMeter %d times", omc.queryCount())
				}
				return
			}
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			if !slices.Equal(accountNames(got.Status.BillingAccounts), tt.wantAccounts) {
				t.Errorf("status.billingAccounts = %v, want %v", accountNames(got.Status.BillingAccounts), tt.wantAccounts)
			}
			if c := customersQueried(omc); !slices.Equal(c, tt.wantCustomers) {
				t.Errorf("customers queried = %v, want %v", c, tt.wantCustomers)
			}
			for _, filter := range projectFilters(omc) {
				if !slices.Equal(filter, tt.wantProjects) {
					t.Errorf("project filter = %v, want %v", filter, tt.wantProjects)
				}
			}
		})
	}
}

func TestCreate_ReportsScope(t *testing.T) {
	r := NewREST(world(t), &fakeOpenMeter{customers: everyCustomer()})
	got, err := create(t, inParent("Project", "web"), r, baseQuery())
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Scope != (usagev1alpha1.UsageScope{Type: usagev1alpha1.UsageScopeProject, Name: "web"}) {
		t.Errorf("scope = %+v", got.Status.Scope)
	}
}

func TestCreate_InvalidSpec(t *testing.T) {
	r := NewREST(world(t), &fakeOpenMeter{customers: everyCustomer()})
	q := baseQuery()
	q.Spec.EndTime = q.Spec.StartTime
	_, err := create(t, inParent("Organization", "acme"), r, q)
	if !apierrors.IsInvalid(err) {
		t.Fatalf("err = %v, want Invalid", err)
	}
}

func TestCreate_Meters(t *testing.T) {
	// Retired meters accept no more usage and draft ones none yet, so they
	// are left out unless asked for (next case).
	t.Run("every published and deprecated meter, by name", func(t *testing.T) {
		r := NewREST(world(t), &fakeOpenMeter{customers: everyCustomer()})
		got, err := create(t, inParent("Organization", "acme"), r, baseQuery())
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, m := range got.Status.Meters {
			names = append(names, m.MeterDefinition)
		}
		if want := []string{"cpu-hours", "egress-gb", "old-meter", "peak-storage"}; !slices.Equal(names, want) {
			t.Errorf("meters = %v, want %v", names, want)
		}
	})

	t.Run("a retired or draft meter is read only when named", func(t *testing.T) {
		r := NewREST(world(t), &fakeOpenMeter{customers: everyCustomer()})
		q := baseQuery()
		q.Spec.Meters = []string{"retired-meter", "draft-meter"}
		got, err := create(t, inParent("Organization", "acme"), r, q)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, m := range got.Status.Meters {
			names = append(names, m.MeterDefinition)
		}
		if want := []string{"retired-meter", "draft-meter"}; !slices.Equal(names, want) {
			t.Errorf("meters = %v, want %v", names, want)
		}
	})

	t.Run("an unknown meter is a bad request", func(t *testing.T) {
		r := NewREST(world(t), &fakeOpenMeter{customers: everyCustomer()})
		q := baseQuery()
		q.Spec.Meters = []string{"nope"}
		if _, err := create(t, inParent("Organization", "acme"), r, q); !apierrors.IsBadRequest(err) {
			t.Fatalf("err = %v, want BadRequest", err)
		}
	})

	t.Run("a dimension the named meter lacks is a bad request", func(t *testing.T) {
		r := NewREST(world(t), &fakeOpenMeter{customers: everyCustomer()})
		q := baseQuery()
		q.Spec.Meters = []string{"peak-storage"}
		q.Spec.Groupings = []usagev1alpha1.UsageGrouping{{GroupBy: []string{"region"}}}
		if _, err := create(t, inParent("Organization", "acme"), r, q); !apierrors.IsBadRequest(err) {
			t.Fatalf("err = %v, want BadRequest", err)
		}
	})

	t.Run("reading every meter, a dimension some lack leaves their breakdown empty", func(t *testing.T) {
		omc := &fakeOpenMeter{customers: everyCustomer(), query: func(q openmeter.MeterQuery) ([]openmeter.MeterQueryRow, error) {
			return []openmeter.MeterQueryRow{{
				WindowStart: start, WindowEnd: start.Add(24 * time.Hour),
				CustomerID: "01ACMEMAIN", GroupBy: map[string]string{"region": "us-east4"}, Value: 1,
			}}, nil
		}}
		r := NewREST(world(t), omc)
		q := baseQuery()
		q.Spec.Groupings = []usagev1alpha1.UsageGrouping{{}, {GroupBy: []string{"region"}}}
		got, err := create(t, inParent("Organization", "acme"), r, q)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range got.Status.Meters {
			if m.Error != "" {
				t.Errorf("%s: unexpected error %q", m.MeterDefinition, m.Error)
			}
			if len(m.Groupings) != 2 || len(m.Groupings[0].Series) != 1 {
				t.Errorf("%s: the total should still be there: %+v", m.MeterDefinition, m.Groupings)
				continue
			}
			wantRegion := 0
			if m.MeterDefinition == "cpu-hours" {
				wantRegion = 1
			}
			if len(m.Groupings[1].Series) != wantRegion {
				t.Errorf("%s: region series = %d, want %d", m.MeterDefinition, len(m.Groupings[1].Series), wantRegion)
			}
		}
	})

	t.Run("project_name is a system dimension every meter has", func(t *testing.T) {
		r := NewREST(world(t), &fakeOpenMeter{customers: everyCustomer()})
		q := baseQuery()
		q.Spec.Meters = []string{"peak-storage"}
		q.Spec.Groupings = []usagev1alpha1.UsageGrouping{{GroupBy: []string{"project_name"}}}
		if _, err := create(t, inParent("Organization", "acme"), r, q); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCreate_MeterErrors(t *testing.T) {
	t.Run("a meter OpenMeter does not know gets its own error", func(t *testing.T) {
		omc := &fakeOpenMeter{customers: everyCustomer(), query: func(q openmeter.MeterQuery) ([]openmeter.MeterQueryRow, error) {
			if q.Slug == openmeter.MeterSlug("compute.miloapis.com/peak-storage") {
				return nil, fmt.Errorf("%w: %s", openmeter.ErrMeterNotFound, q.Slug)
			}
			return nil, nil
		}}
		r := NewREST(world(t), omc)
		q := baseQuery()
		q.Spec.Meters = []string{"cpu-hours", "peak-storage"}
		got, err := create(t, inParent("Organization", "acme"), r, q)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status.Meters[0].Error != "" {
			t.Errorf("cpu-hours: error %q", got.Status.Meters[0].Error)
		}
		if got.Status.Meters[1].Error == "" || got.Status.Meters[1].Groupings != nil {
			t.Errorf("peak-storage: %+v, want an error and no results", got.Status.Meters[1])
		}
	})

	t.Run("a meter OpenMeter rejects the query for gets its own error", func(t *testing.T) {
		omc := &fakeOpenMeter{customers: everyCustomer(), query: func(q openmeter.MeterQuery) ([]openmeter.MeterQueryRow, error) {
			if q.Slug == openmeter.MeterSlug("compute.miloapis.com/peak-storage") {
				return nil, &openmeter.PermanentError{Err: fmt.Errorf("invalid group by"), StatusCode: 400}
			}
			return nil, nil
		}}
		r := NewREST(world(t), omc)
		q := baseQuery()
		q.Spec.Meters = []string{"cpu-hours", "peak-storage"}
		got, err := create(t, inParent("Organization", "acme"), r, q)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status.Meters[0].Error != "" || got.Status.Meters[0].Groupings == nil {
			t.Errorf("cpu-hours: %+v, want results", got.Status.Meters[0])
		}
		if e := got.Status.Meters[1].Error; !strings.Contains(e, "OpenMeter rejected the query") || got.Status.Meters[1].Groupings != nil {
			t.Errorf("peak-storage: %+v, want a rejected-query error and no results", got.Status.Meters[1])
		}
	})

	t.Run("an OpenMeter outage fails the query", func(t *testing.T) {
		omc := &fakeOpenMeter{customers: everyCustomer(), query: func(openmeter.MeterQuery) ([]openmeter.MeterQueryRow, error) {
			return nil, &openmeter.TransientError{Err: fmt.Errorf("503")}
		}}
		r := NewREST(world(t), omc)
		if _, err := create(t, inParent("Organization", "acme"), r, baseQuery()); !apierrors.IsServiceUnavailable(err) {
			t.Fatalf("err = %v, want ServiceUnavailable", err)
		}
	})
}

func TestCreate_NoCustomersSkipsOpenMeter(t *testing.T) {
	// An empty customer filter would read every customer's usage.
	omc := &fakeOpenMeter{customers: map[string]string{}}
	r := NewREST(world(t), omc)
	got, err := create(t, inParent("Organization", "acme"), r, baseQuery())
	if err != nil {
		t.Fatal(err)
	}
	if omc.queryCount() != 0 {
		t.Errorf("OpenMeter was queried %d times", omc.queryCount())
	}
	for _, m := range got.Status.Meters {
		for _, g := range m.Groupings {
			if len(g.Series) != 0 {
				t.Errorf("%s has series without customers", m.MeterDefinition)
			}
		}
	}
}

func TestCreate_CustomerIDsAreCached(t *testing.T) {
	omc := &fakeOpenMeter{customers: everyCustomer()}
	r := NewREST(world(t), omc)
	for range 2 {
		if _, err := create(t, inParent("Organization", "acme"), r, baseQuery()); err != nil {
			t.Fatal(err)
		}
	}
	if omc.customerCalls != 2 {
		t.Errorf("GetCustomer called %d times for 2 accounts over 2 queries, want 2", omc.customerCalls)
	}
}

func TestCreate_Results(t *testing.T) {
	day := 24 * time.Hour
	rows := func(q openmeter.MeterQuery) ([]openmeter.MeterQueryRow, error) {
		if q.WindowSize == nil {
			// The un-windowed totals query, only made for peak-storage (Max).
			return []openmeter.MeterQueryRow{
				{CustomerID: "01ACMEMAIN", GroupBy: map[string]string{}, Value: 40, WindowStart: start, WindowEnd: end},
				{CustomerID: "01ACMEEU", GroupBy: map[string]string{}, Value: 70, WindowStart: start, WindowEnd: end},
			}, nil
		}
		return []openmeter.MeterQueryRow{
			// Two accounts on day one, one on day two.
			{WindowStart: start, WindowEnd: start.Add(day), CustomerID: "01ACMEMAIN", GroupBy: map[string]string{}, Value: 10},
			{WindowStart: start, WindowEnd: start.Add(day), CustomerID: "01ACMEEU", GroupBy: map[string]string{}, Value: 30},
			{WindowStart: start.Add(day), WindowEnd: start.Add(2 * day), CustomerID: "01ACMEEU", GroupBy: map[string]string{}, Value: 5},
		}, nil
	}

	omc := &fakeOpenMeter{customers: everyCustomer(), query: rows}
	r := NewREST(world(t), omc)
	q := baseQuery()
	q.Spec.Meters = []string{"cpu-hours", "peak-storage"}
	got, err := create(t, inParent("Organization", "acme"), r, q)
	if err != nil {
		t.Fatal(err)
	}

	sum := got.Status.Meters[0]
	if sum.Aggregation != "Sum" || sum.Unit != "h" || sum.MeterName != "compute.miloapis.com/cpu-hours" {
		t.Errorf("cpu-hours metadata = %+v", sum)
	}
	series := sum.Groupings[0].Series
	if len(series) != 1 {
		t.Fatalf("cpu-hours series = %+v", series)
	}
	if series[0].Total != "45" {
		t.Errorf("Sum total = %s, want 45 (the sum of its points)", series[0].Total)
	}
	if len(series[0].Points) != 2 || series[0].Points[0].Value != "40" || series[0].Points[1].Value != "5" {
		t.Errorf("Sum points = %+v, want 40 then 5", series[0].Points)
	}

	max := got.Status.Meters[1].Groupings[0].Series[0]
	if max.Points[0].Value != "30" {
		t.Errorf("Max day one = %s, want 30 (the larger account)", max.Points[0].Value)
	}
	if max.Total != "70" {
		t.Errorf("Max total = %s, want 70 (from the totals query)", max.Total)
	}

	// Sum needs one query per grouping; Max needs a second for its total.
	if n := omc.queryCount(); n != 3 {
		t.Errorf("OpenMeter queries = %d, want 3", n)
	}
}

func TestCreate_TotalsOnly(t *testing.T) {
	omc := &fakeOpenMeter{customers: everyCustomer(), query: func(q openmeter.MeterQuery) ([]openmeter.MeterQueryRow, error) {
		if q.WindowSize != nil {
			t.Errorf("a totals-only query asked for windows")
		}
		return []openmeter.MeterQueryRow{
			{CustomerID: "01ACMEMAIN", GroupBy: map[string]string{"region": "us-east4"}, Value: 1.5},
			{CustomerID: "01ACMEEU", GroupBy: map[string]string{"region": "us-east4"}, Value: 2},
			{CustomerID: "01ACMEEU", GroupBy: map[string]string{"region": ""}, Value: 7},
		}, nil
	}}
	r := NewREST(world(t), omc)
	q := baseQuery()
	q.Spec.Granularity = ""
	q.Spec.Meters = []string{"cpu-hours"}
	q.Spec.Groupings = []usagev1alpha1.UsageGrouping{{GroupBy: []string{"region"}}}
	got, err := create(t, inParent("Organization", "acme"), r, q)
	if err != nil {
		t.Fatal(err)
	}

	series := got.Status.Meters[0].Groupings[0].Series
	if len(series) != 2 {
		t.Fatalf("series = %+v", series)
	}
	// Sorted by group value: the unset region ("") first.
	if series[0].Group["region"] != "" || series[0].Total != "7" || series[0].Points != nil {
		t.Errorf("series 0 = %+v", series[0])
	}
	if series[1].Group["region"] != "us-east4" || series[1].Total != "3.5" {
		t.Errorf("series 1 = %+v", series[1])
	}
}

func TestCreate_ClampsBucketsToTheRange(t *testing.T) {
	from := start.Add(15 * time.Hour)
	to := from.Add(20 * time.Hour)
	omc := &fakeOpenMeter{customers: everyCustomer(), query: func(openmeter.MeterQuery) ([]openmeter.MeterQueryRow, error) {
		return []openmeter.MeterQueryRow{
			{WindowStart: start, WindowEnd: start.Add(24 * time.Hour), CustomerID: "01ACMEMAIN", GroupBy: map[string]string{}, Value: 1},
			{WindowStart: start.Add(24 * time.Hour), WindowEnd: start.Add(48 * time.Hour), CustomerID: "01ACMEMAIN", GroupBy: map[string]string{}, Value: 1},
		}, nil
	}}
	r := NewREST(world(t), omc)
	q := baseQuery()
	q.Spec.StartTime = metav1.NewTime(from)
	q.Spec.EndTime = metav1.NewTime(to)
	q.Spec.Meters = []string{"cpu-hours"}
	got, err := create(t, inParent("Organization", "acme"), r, q)
	if err != nil {
		t.Fatal(err)
	}
	points := got.Status.Meters[0].Groupings[0].Series[0].Points
	if !points[0].Start.Time.Equal(from) || !points[1].End.Time.Equal(to) {
		t.Errorf("points = %+v, want them clamped to [%s, %s)", points, from, to)
	}
}

func TestCombine(t *testing.T) {
	values := []float64{4, 1, 7}
	tests := map[billingv1alpha1.MeterAggregation]float64{
		billingv1alpha1.MeterAggregationSum:         12,
		billingv1alpha1.MeterAggregationCount:       12,
		billingv1alpha1.MeterAggregationUniqueCount: 12,
		billingv1alpha1.MeterAggregationLatest:      12,
		billingv1alpha1.MeterAggregationMax:         7,
		billingv1alpha1.MeterAggregationMin:         1,
		billingv1alpha1.MeterAggregationAverage:     4,
	}
	for agg, want := range tests {
		if got := combine(agg, values); got != want {
			t.Errorf("%s: got %v, want %v", agg, got, want)
		}
	}
	if got := combine(billingv1alpha1.MeterAggregationMax, nil); got != 0 {
		t.Errorf("no values: got %v, want 0", got)
	}
}

// egressRegion is a dimension that is not a valid OpenMeter group-by key, so
// it is queried as egressRegionKey and must be mapped back in the results.
const (
	egressRegion    = "networking.miloapis.com/region"
	egressRegionKey = "networking_miloapis_com_region"
)

// usageEvent is one event's contribution to a meter, as OpenMeter stores it.
type usageEvent struct {
	customer string
	day      int
	project  string
	region   string
	value    float64
}

// answerFrom answers meter queries the way OpenMeter does for a Sum meter:
// one row per customer, window and value of the requested group-by keys.
// Events carry project_name and the egress region.
func answerFrom(events []usageEvent) func(openmeter.MeterQuery) ([]openmeter.MeterQueryRow, error) {
	return func(q openmeter.MeterQuery) ([]openmeter.MeterQueryRow, error) {
		type rowKey struct {
			customer string
			window   time.Time
			group    string
		}
		rows := map[rowKey]*openmeter.MeterQueryRow{}
		var order []rowKey
		for _, e := range events {
			if !slices.Contains(q.CustomerIDs, openmeter.CustomerID(e.customer)) {
				continue
			}
			dims := map[string]string{"project_name": e.project, egressRegionKey: e.region}
			groupBy := map[string]string{}
			for _, key := range q.GroupBy {
				groupBy[key] = dims[key]
			}
			windowStart, windowEnd := q.From, q.To
			if q.WindowSize != nil {
				windowStart = start.Add(time.Duration(e.day) * 24 * time.Hour)
				windowEnd = windowStart.Add(24 * time.Hour)
			}
			k := rowKey{e.customer, windowStart, fmt.Sprint(groupBy)}
			row, ok := rows[k]
			if !ok {
				row = &openmeter.MeterQueryRow{WindowStart: windowStart, WindowEnd: windowEnd, CustomerID: openmeter.CustomerID(e.customer), GroupBy: groupBy}
				rows[k] = row
				order = append(order, k)
			}
			row.Value += e.value
		}
		var out []openmeter.MeterQueryRow
		for _, k := range order {
			out = append(out, *rows[k])
		}
		return out, nil
	}
}

// summarize renders a grouping's series as "group: total [points]", with the
// group's values in groupBy order.
func summarize(g usagev1alpha1.UsageGroupingResult) []string {
	var out []string
	for _, s := range g.Series {
		var group []string
		for _, dim := range g.GroupBy {
			group = append(group, dim+"="+s.Group[dim])
		}
		var points []string
		for _, p := range s.Points {
			points = append(points, fmt.Sprintf("%s=%s", p.Start.Format("01-02"), p.Value))
		}
		out = append(out, fmt.Sprintf("%s: %s %v", strings.Join(group, ","), s.Total, points))
	}
	return out
}

func TestCreate_Groupings(t *testing.T) {
	// Two accounts, two days, two projects, two regions.
	omc := &fakeOpenMeter{customers: everyCustomer(), query: answerFrom([]usageEvent{
		{customer: "01ACMEMAIN", day: 0, project: "web", region: "us-east4", value: 10},
		{customer: "01ACMEEU", day: 0, project: "web", region: "us-east4", value: 5},
		{customer: "01ACMEEU", day: 0, project: "web", region: "eu-west1", value: 1},
		{customer: "01ACMEMAIN", day: 1, project: "api", region: "eu-west1", value: 7},
		{customer: "01GLOBEX", day: 0, project: "shop", region: "us-east4", value: 100},
	})}
	r := NewREST(world(t), omc)
	q := baseQuery()
	q.Spec.Meters = []string{"egress-gb"}
	q.Spec.Groupings = []usagev1alpha1.UsageGrouping{
		{},
		{GroupBy: []string{egressRegion}},
		{GroupBy: []string{"project_name", egressRegion}},
	}
	got, err := create(t, inParent("Organization", "acme"), r, q)
	if err != nil {
		t.Fatal(err)
	}

	// Each grouping is one query (Sum totals come from the points), by the
	// dimensions' OpenMeter keys.
	var sent [][]string
	for _, query := range omc.queries {
		sent = append(sent, query.GroupBy)
	}
	slices.SortFunc(sent, func(a, b []string) int { return cmp.Compare(len(a), len(b)) })
	wantSent := [][]string{nil, {egressRegionKey}, {"project_name", egressRegionKey}}
	if len(sent) != len(wantSent) {
		t.Fatalf("queries sent group by %v, want %v", sent, wantSent)
	}
	for i := range wantSent {
		if !slices.Equal(sent[i], wantSent[i]) {
			t.Errorf("queries sent group by %v, want %v", sent, wantSent)
		}
	}

	// Results are keyed by dimension name, not OpenMeter key; the two
	// accounts' usage on the same day and group is added together; globex's
	// usage never appears.
	m := got.Status.Meters[0]
	if m.Error != "" {
		t.Fatalf("error: %s", m.Error)
	}
	want := [][]string{
		{": 23 [10-05=16 10-06=7]"},
		{
			egressRegion + "=eu-west1: 8 [10-05=1 10-06=7]",
			egressRegion + "=us-east4: 15 [10-05=15]",
		},
		{
			"project_name=api," + egressRegion + "=eu-west1: 7 [10-06=7]",
			"project_name=web," + egressRegion + "=eu-west1: 1 [10-05=1]",
			"project_name=web," + egressRegion + "=us-east4: 15 [10-05=15]",
		},
	}
	for i, w := range want {
		if !slices.Equal(m.Groupings[i].GroupBy, q.Spec.Groupings[i].GroupBy) {
			t.Errorf("grouping %d groupBy = %v, want %v", i, m.Groupings[i].GroupBy, q.Spec.Groupings[i].GroupBy)
		}
		if s := summarize(m.Groupings[i]); !slices.Equal(s, w) {
			t.Errorf("grouping %d:\n got %q\nwant %q", i, s, w)
		}
	}
}

func TestCreate_SendsTheRangeAndBuckets(t *testing.T) {
	window := func(w om.WindowSize) *om.WindowSize { return &w }
	tests := []struct {
		granularity usagev1alpha1.UsageGranularity
		want        *om.WindowSize
	}{
		{usagev1alpha1.UsageGranularityHour, window(om.WindowSizeHour)},
		{usagev1alpha1.UsageGranularityDay, window(om.WindowSizeDay)},
		{usagev1alpha1.UsageGranularityMonth, window(om.WindowSizeMonth)},
		{"", nil},
	}
	for _, tt := range tests {
		t.Run(string(tt.granularity), func(t *testing.T) {
			omc := &fakeOpenMeter{customers: everyCustomer()}
			r := NewREST(world(t), omc)
			q := baseQuery()
			q.Spec.Granularity = tt.granularity
			q.Spec.TimeZone = "Europe/Madrid"
			q.Spec.Meters = []string{"cpu-hours"}
			if _, err := create(t, inParent("Organization", "acme"), r, q); err != nil {
				t.Fatal(err)
			}
			if omc.queryCount() != 1 {
				t.Fatalf("queries = %d, want 1", omc.queryCount())
			}
			sent := omc.queries[0]
			if !sent.From.Equal(start) || !sent.To.Equal(end) {
				t.Errorf("range = [%s, %s), want [%s, %s)", sent.From, sent.To, start, end)
			}
			if sent.TimeZone != "Europe/Madrid" {
				t.Errorf("timeZone = %q", sent.TimeZone)
			}
			if (sent.WindowSize == nil) != (tt.want == nil) || (tt.want != nil && *sent.WindowSize != *tt.want) {
				t.Errorf("windowSize = %v, want %v", sent.WindowSize, tt.want)
			}
		})
	}
}
