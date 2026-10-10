// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
)

// fakeProfileEpoch is the createdAt of the first fake billing profile;
// each later one is created a second after the previous.
var fakeProfileEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// serveBillingProfiles handles /api/v1/billing/profiles[/{id}] and
// /api/v1/billing/customers/{customerKey} (the override endpoints). Returns
// false when the request isn't one of those routes, so ServeHTTP can fall
// through to other resources.
//
// Writes behave like OpenMeter's v1 HTTP driver: every workflow field the
// body omits is filled from billing.DefaultWorkflowConfig — on update too,
// since the PUT is a full replace (an omitted description is cleared as
// well) — and the collection alignment is always stored as
// {type: subscription}. A fake that kept omitted fields would hide exactly
// the bug where a partial update reverted the operator's settings.
func (f *fakeServer) serveBillingProfiles(w http.ResponseWriter, r *http.Request) bool {
	const profilesPath = "/api/v1/billing/profiles"
	const customersPath = "/api/v1/billing/customers/"

	switch {
	case r.Method == http.MethodPost && r.URL.Path == profilesPath:
		var body om.BillingProfileCreate
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return true
		}
		f.billingProfileSeq++
		id := fmt.Sprintf("profile_%s_%d", body.Name, f.billingProfileSeq)
		var apps om.BillingProfileAppsOrReference
		_ = apps.FromBillingProfileAppReferences(om.BillingProfileAppReferences{
			Invoicing: om.AppReference{Id: body.Apps.Invoicing},
			Payment:   om.AppReference{Id: body.Apps.Payment},
			Tax:       om.AppReference{Id: body.Apps.Tax},
		})
		created := fakeProfileEpoch.Add(time.Duration(f.billingProfileSeq) * time.Second)
		p := om.BillingProfile{
			Id:          id,
			Name:        body.Name,
			Description: body.Description,
			Default:     body.Default,
			Apps:        apps,
			Supplier:    body.Supplier,
			Metadata:    body.Metadata,
			CreatedAt:   created,
			UpdatedAt:   created,
			Workflow:    normalizeWorkflowLikeOpenMeter(om.BillingWorkflow(body.Workflow), string(om.BillingWorkflowInvoicingSubscriptionEndProrationModeBillActualPeriod)),
		}
		f.billingProfiles[id] = p
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(p)
		return true

	case r.Method == http.MethodGet && r.URL.Path == profilesPath:
		page, pageSize := pageParams(r)
		f.billingProfileListCalls++
		if f.billingProfileListHook != nil {
			f.billingProfileListHook(f, page)
		}
		var items []om.BillingProfile
		for _, p := range f.billingProfiles {
			if p.DeletedAt == nil {
				items = append(items, p)
			}
		}
		sortBillingProfiles(items, r.URL.Query().Get("orderBy"), r.URL.Query().Get("order"))
		total := len(items)
		start, end := pageBounds(page, pageSize, total)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(om.BillingProfilePaginatedResponse{
			Items: items[start:end], Page: page, PageSize: pageSize, TotalCount: total,
		})
		return true

	case strings.HasPrefix(r.URL.Path, profilesPath+"/"):
		id := strings.TrimPrefix(r.URL.Path, profilesPath+"/")
		switch r.Method {
		case http.MethodGet:
			p, ok := f.billingProfiles[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return true
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(p)
			return true
		case http.MethodPut:
			p, ok := f.billingProfiles[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return true
			}
			var body om.BillingProfileReplaceUpdateWithWorkflow
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return true
			}
			existingProration := ""
			if p.Workflow.Invoicing != nil && p.Workflow.Invoicing.SubscriptionEndProrationMode != nil {
				existingProration = string(*p.Workflow.Invoicing.SubscriptionEndProrationMode)
			}
			p.Name = body.Name
			p.Description = body.Description
			p.Default = body.Default
			p.Supplier = body.Supplier
			p.Metadata = body.Metadata
			f.billingProfileUpdates++
			p.Workflow = normalizeWorkflowLikeOpenMeter(body.Workflow, existingProration)
			f.billingProfiles[id] = p
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(p)
			return true
		case http.MethodDelete:
			p, ok := f.billingProfiles[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return true
			}
			if p.Default {
				writeProblem(w, http.StatusBadRequest, "default_profile_cannot_be_deleted", "default profile cannot be deleted")
				return true
			}
			delete(f.billingProfiles, id)
			w.WriteHeader(http.StatusNoContent)
			return true
		}
		return false

	case strings.HasPrefix(r.URL.Path, customersPath):
		key := strings.TrimPrefix(r.URL.Path, customersPath)
		switch r.Method {
		case http.MethodPut:
			var body om.BillingProfileCustomerOverrideCreate
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return true
			}
			f.billingProfileOverrides[key] = body
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(om.BillingProfileCustomerOverride{BillingProfileId: body.BillingProfileId})
			return true
		case http.MethodDelete:
			if _, ok := f.billingProfileOverrides[key]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return true
			}
			delete(f.billingProfileOverrides, key)
			w.WriteHeader(http.StatusNoContent)
			return true
		}
		return false
	}
	return false
}

// sortBillingProfiles orders items the way the list endpoint's orderBy and
// order params do (id as the tie-breaker); without orderBy, by id.
func sortBillingProfiles(items []om.BillingProfile, orderBy, order string) {
	desc := order == string(om.SortOrderDESC)
	less := func(i, j int) bool { return items[i].Id < items[j].Id }
	switch orderBy {
	case string(om.BillingProfileOrderByDefault):
		defaultFirst := desc
		less = func(i, j int) bool {
			if items[i].Default != items[j].Default {
				return items[i].Default == defaultFirst // ASC: false first; DESC: true first
			}
			return items[i].Id < items[j].Id
		}
		desc = false
	case string(om.BillingProfileOrderByCreatedAt):
		less = func(i, j int) bool {
			if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
				return items[i].CreatedAt.Before(items[j].CreatedAt)
			}
			return items[i].Id < items[j].Id
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if desc {
			return less(j, i)
		}
		return less(i, j)
	})
}

// normalizeWorkflowLikeOpenMeter reproduces what a real OpenMeter does to a
// submitted workflow (fromAPIBillingWorkflow in the v1 HTTP driver):
//
//   - whatever alignment you send, it stores {type: "subscription"} —
//     verified live by POSTing {anchored, anchor 2024-01-15, interval P3M}
//     and reading back {type: subscription};
//   - every omitted field takes billing.DefaultWorkflowConfig's value
//     (interval PT1H, autoAdvance true, draftPeriod P0D, dueAfter P30D,
//     progressiveBilling true, charge_automatically, tax enabled and not
//     enforced), except an omitted subscriptionEndProrationMode, which
//     keeps existingProration (bill_actual_period on create).
//
// The fake previously echoed the submitted workflow back verbatim, which is
// precisely why TestEnsureBillingProfile_NoopWhenUpToDate passed while
// production issued an UPDATE on every single reconcile forever, and why
// nothing noticed that a partial workflow reset the operator's settings.
func normalizeWorkflowLikeOpenMeter(in om.BillingWorkflow, existingProration string) om.BillingWorkflow {
	var alignment om.BillingWorkflowCollectionAlignment
	if err := alignment.FromBillingWorkflowCollectionAlignmentSubscription(
		om.BillingWorkflowCollectionAlignmentSubscription{
			Type: om.BillingWorkflowCollectionAlignmentSubscriptionTypeSubscription,
		},
	); err != nil {
		panic(err) // static payload; a failure here is a broken test fixture
	}
	str := func(p *string, def string) *string {
		if p != nil {
			v := *p
			return &v
		}
		return &def
	}
	boolean := func(p *bool, def bool) *bool {
		if p != nil {
			v := *p
			return &v
		}
		return &def
	}

	collection := in.Collection
	if collection == nil {
		collection = &om.BillingWorkflowCollectionSettings{}
	}
	invoicing := in.Invoicing
	if invoicing == nil {
		invoicing = &om.BillingWorkflowInvoicingSettings{}
	}
	payment := in.Payment
	if payment == nil {
		payment = &om.BillingWorkflowPaymentSettings{}
	}
	tax := in.Tax
	if tax == nil {
		tax = &om.BillingWorkflowTaxSettings{}
	}

	method := om.CollectionMethodChargeAutomatically
	if payment.CollectionMethod != nil {
		method = *payment.CollectionMethod
	}
	proration := om.BillingWorkflowInvoicingSubscriptionEndProrationMode(existingProration)
	if invoicing.SubscriptionEndProrationMode != nil {
		proration = *invoicing.SubscriptionEndProrationMode
	}

	return om.BillingWorkflow{
		Collection: &om.BillingWorkflowCollectionSettings{
			Alignment: &alignment,
			Interval:  str(collection.Interval, "PT1H"),
		},
		Invoicing: &om.BillingWorkflowInvoicingSettings{
			AutoAdvance:                  boolean(invoicing.AutoAdvance, true),
			DraftPeriod:                  str(invoicing.DraftPeriod, "P0D"),
			DueAfter:                     str(invoicing.DueAfter, "P30D"),
			ProgressiveBilling:           boolean(invoicing.ProgressiveBilling, true),
			SubscriptionEndProrationMode: &proration,
			DefaultTaxConfig:             invoicing.DefaultTaxConfig,
		},
		Payment: &om.BillingWorkflowPaymentSettings{CollectionMethod: &method},
		Tax: &om.BillingWorkflowTaxSettings{
			Enabled:  boolean(tax.Enabled, true),
			Enforced: boolean(tax.Enforced, false),
		},
	}
}

// serveCustomerList handles GET /api/v1/customers (the list route only):
// key is a case-insensitive partial match, as on the real server, and
// soft-deleted customers are listed only with includeDeleted.
func (f *fakeServer) serveCustomerList(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet || r.URL.Path != "/api/v1/customers" {
		return false
	}
	q := r.URL.Query()
	keyFilter := strings.ToLower(q.Get("key"))
	all := make([]om.Customer, 0, len(f.customers)+len(f.deletedCustomers))
	for _, cust := range f.customers {
		all = append(all, cust)
	}
	if q.Get("includeDeleted") == "true" {
		all = append(all, f.deletedCustomers...)
	}
	var items []om.Customer
	for _, cust := range all {
		if keyFilter != "" && (cust.Key == nil || !strings.Contains(strings.ToLower(*cust.Key), keyFilter)) {
			continue
		}
		items = append(items, cust)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Id < items[j].Id })
	page, pageSize := pageParams(r)
	start, end := pageBounds(page, pageSize, len(items))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(om.CustomerPaginatedResponse{
		Items: items[start:end], Page: page, PageSize: pageSize, TotalCount: len(items),
	})
	return true
}

// seedDefaultBillingProfile seeds the operator-provisioned default profile
// with a workflow that differs from OpenMeter's built-in defaults in every
// field, so tests can tell "copied from the default profile" apart from
// "filled in by the server".
func seedDefaultBillingProfile(f *fakeServer) om.BillingProfile {
	appRefs := om.BillingProfileAppReferences{
		Invoicing: om.AppReference{Id: "app_invoicing"},
		Payment:   om.AppReference{Id: "app_payment"},
		Tax:       om.AppReference{Id: "app_tax"},
	}
	var apps om.BillingProfileAppsOrReference
	_ = apps.FromBillingProfileAppReferences(appRefs)
	supplierName := "Acme Platform Inc."
	country := "US"
	taxCode := "txcd_10000000"
	behavior := om.TaxBehaviorExclusive
	interval := "PT2H"
	draftPeriod := "P2D"
	dueAfter := "P14D"
	proration := om.BillingWorkflowInvoicingSubscriptionEndProrationModeBillFullPeriod
	method := om.CollectionMethodChargeAutomatically
	p := om.BillingProfile{
		Id:        "profile_default",
		Name:      "Default",
		Default:   true,
		Apps:      apps,
		CreatedAt: fakeProfileEpoch,
		Supplier: om.BillingParty{
			Name:      &supplierName,
			Addresses: &[]om.Address{{Country: &country}},
		},
		Workflow: om.BillingWorkflow{
			Collection: &om.BillingWorkflowCollectionSettings{Interval: &interval},
			Invoicing: &om.BillingWorkflowInvoicingSettings{
				AutoAdvance:                  ptr(false),
				DraftPeriod:                  &draftPeriod,
				DueAfter:                     &dueAfter,
				ProgressiveBilling:           ptr(false),
				SubscriptionEndProrationMode: &proration,
				DefaultTaxConfig:             &om.TaxConfig{Behavior: &behavior, Stripe: &om.StripeTaxConfig{Code: taxCode}},
			},
			Payment: &om.BillingWorkflowPaymentSettings{CollectionMethod: &method},
			Tax:     &om.BillingWorkflowTaxSettings{Enabled: ptr(false), Enforced: ptr(true)},
		},
	}
	p.Workflow = normalizeWorkflowLikeOpenMeter(p.Workflow, "")
	f.billingProfiles[p.Id] = p
	return p
}

func standardDesired(accountKey CustomerKey) DesiredBillingProfile {
	return DesiredBillingProfile{AccountKey: accountKey, Name: "x", NetDays: 30, InvoiceFrequency: "Monthly", InvoiceDayOfMonth: 1}
}

// assertInheritsDefaultWorkflow checks that every workflow setting the
// account does not control came from the default profile.
func assertInheritsDefaultWorkflow(t *testing.T, got om.BillingWorkflow, def om.BillingProfile) {
	t.Helper()
	gotInv, defInv := got.Invoicing, def.Workflow.Invoicing
	if gotInv == nil {
		t.Fatal("invoicing settings missing")
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"collection.interval", *got.Collection.Interval, *def.Workflow.Collection.Interval},
		{"invoicing.autoAdvance", *gotInv.AutoAdvance, *defInv.AutoAdvance},
		{"invoicing.draftPeriod", *gotInv.DraftPeriod, *defInv.DraftPeriod},
		{"invoicing.progressiveBilling", *gotInv.ProgressiveBilling, *defInv.ProgressiveBilling},
		{"invoicing.subscriptionEndProrationMode", *gotInv.SubscriptionEndProrationMode, *defInv.SubscriptionEndProrationMode},
		{"tax.enabled", *got.Tax.Enabled, *def.Workflow.Tax.Enabled},
		{"tax.enforced", *got.Tax.Enforced, *def.Workflow.Tax.Enforced},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want the default profile's %v", c.name, c.got, c.want)
		}
	}
	if gotInv.DefaultTaxConfig == nil || gotInv.DefaultTaxConfig.Stripe == nil || gotInv.DefaultTaxConfig.Stripe.Code != defInv.DefaultTaxConfig.Stripe.Code {
		t.Errorf("invoicing.defaultTaxConfig = %+v, want the default profile's", gotInv.DefaultTaxConfig)
	}
}

func TestEnsureBillingProfile_CreatesFromDefault(t *testing.T) {
	c, f := newTestClient(t)
	def := seedDefaultBillingProfile(f)

	desired := DesiredBillingProfile{
		AccountKey:        "account-1",
		Name:              "org-a/acme payment terms",
		NetDays:           45,
		InvoiceFrequency:  "Quarterly",
		InvoiceDayOfMonth: 15,
	}
	res, err := c.EnsureBillingProfile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("EnsureBillingProfile: %v", err)
	}
	got := res.Profile
	if got.Default {
		t.Error("per-account profile must not be marked Default")
	}
	if got.Supplier.Name == nil || *got.Supplier.Name != "Acme Platform Inc." {
		t.Errorf("Supplier not cloned from default profile: %+v", got.Supplier)
	}
	if got.Workflow.Invoicing.DueAfter == nil || *got.Workflow.Invoicing.DueAfter != "P45D" {
		t.Errorf("DueAfter = %v, want P45D", got.Workflow.Invoicing.DueAfter)
	}
	if m := got.Workflow.Payment.CollectionMethod; m == nil || *m != om.CollectionMethodChargeAutomatically {
		t.Errorf("CollectionMethod = %v, want the default profile's charge_automatically", m)
	}
	assertInheritsDefaultWorkflow(t, got.Workflow, def)
	if res.IgnoredExistingID != "" || res.AppsDrift != "" {
		t.Errorf("unexpected findings on a plain create: %+v", res)
	}
}

func TestEnsureBillingProfile_InvoiceArrangementSendsInvoices(t *testing.T) {
	c, f := newTestClient(t)
	seedDefaultBillingProfile(f)
	desired := standardDesired("account-1")

	created, err := c.EnsureBillingProfile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("first EnsureBillingProfile: %v", err)
	}
	f.billingProfileUpdates = 0

	desired.NetDays = 60
	desired.CollectionMethod = om.CollectionMethodSendInvoice
	res, err := c.EnsureBillingProfile(context.Background(), created.Profile.Id, desired)
	if err != nil {
		t.Fatalf("second EnsureBillingProfile: %v", err)
	}
	if f.billingProfileUpdates != 1 {
		t.Errorf("updates = %d, want 1", f.billingProfileUpdates)
	}
	wf := res.Profile.Workflow
	if m := wf.Payment.CollectionMethod; m == nil || *m != om.CollectionMethodSendInvoice {
		t.Errorf("CollectionMethod = %v, want send_invoice", m)
	}
	if *wf.Invoicing.DueAfter != "P60D" {
		t.Errorf("DueAfter = %s, want P60D", *wf.Invoicing.DueAfter)
	}

	// And it converges.
	f.billingProfileUpdates = 0
	if _, err := c.EnsureBillingProfile(context.Background(), created.Profile.Id, desired); err != nil {
		t.Fatalf("third EnsureBillingProfile: %v", err)
	}
	if f.billingProfileUpdates != 0 {
		t.Errorf("converged EnsureBillingProfile issued %d update(s), want 0", f.billingProfileUpdates)
	}
}

func TestEnsureBillingProfile_NoDefaultIsPermanentError(t *testing.T) {
	c, _ := newTestClient(t)
	_, err := c.EnsureBillingProfile(context.Background(), "", standardDesired("account-1"))
	if !IsPermanent(err) {
		t.Errorf("expected a PermanentError when no default profile is provisioned, got %T: %v", err, err)
	}
}

func TestEnsureBillingProfile_RequiresAccountKey(t *testing.T) {
	c, f := newTestClient(t)
	seedDefaultBillingProfile(f)
	_, err := c.EnsureBillingProfile(context.Background(), "", standardDesired(""))
	if !IsPermanent(err) {
		t.Errorf("expected a PermanentError when AccountKey is empty, got %T: %v", err, err)
	}
}

func TestEnsureBillingProfile_NoopWhenUpToDate(t *testing.T) {
	c, f := newTestClient(t)
	seedDefaultBillingProfile(f)
	desired := standardDesired("account-1")

	first, err := c.EnsureBillingProfile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("first EnsureBillingProfile: %v", err)
	}
	second, err := c.EnsureBillingProfile(context.Background(), first.Profile.Id, desired)
	if err != nil {
		t.Fatalf("second EnsureBillingProfile: %v", err)
	}
	if second.Profile.Id != first.Profile.Id {
		t.Errorf("Id changed across a no-drift EnsureBillingProfile: %q -> %q", first.Profile.Id, second.Profile.Id)
	}
}

func TestEnsureBillingProfile_UpdatePreservesAccountKeyMetadata(t *testing.T) {
	// updateBillingProfile PUTs a full replacement; if it forgot to carry
	// existing.Metadata forward, this tag would be silently wiped on the
	// very first update, breaking findBillingProfileByAccountKey for good.
	c, f := newTestClient(t)
	seedDefaultBillingProfile(f)
	desired := standardDesired("account-1")
	created, err := c.EnsureBillingProfile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("first EnsureBillingProfile: %v", err)
	}

	desired.NetDays = 60
	if _, err := c.EnsureBillingProfile(context.Background(), created.Profile.Id, desired); err != nil {
		t.Fatalf("second EnsureBillingProfile: %v", err)
	}

	stored := f.billingProfiles[created.Profile.Id]
	if stored.Metadata == nil || (*stored.Metadata)[billingProfileAccountKeyMetadataKey] != "account-1" {
		t.Errorf("Metadata[%q] lost after update: %+v", billingProfileAccountKeyMetadataKey, stored.Metadata)
	}
}

// TestEnsureBillingProfile_UpdateKeepsDefaultSettingsAndDescription is the
// M1 regression: OpenMeter's update is a full replace, so an update that
// sent only dueAfter and the alignment reset every other workflow setting
// to OpenMeter's built-in defaults (progressive billing back on, draft
// period zero, ...) and dropped the description.
func TestEnsureBillingProfile_UpdateKeepsDefaultSettingsAndDescription(t *testing.T) {
	c, f := newTestClient(t)
	def := seedDefaultBillingProfile(f)
	desired := standardDesired("account-1")
	created, err := c.EnsureBillingProfile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("first EnsureBillingProfile: %v", err)
	}
	stored := f.billingProfiles[created.Profile.Id]
	note := "set by finance"
	stored.Description = &note
	f.billingProfiles[created.Profile.Id] = stored

	desired.NetDays = 60
	res, err := c.EnsureBillingProfile(context.Background(), created.Profile.Id, desired)
	if err != nil {
		t.Fatalf("second EnsureBillingProfile: %v", err)
	}
	assertInheritsDefaultWorkflow(t, res.Profile.Workflow, def)
	if res.Profile.Description == nil || *res.Profile.Description != note {
		t.Errorf("Description = %v, want %q preserved", res.Profile.Description, note)
	}
}

// TestEnsureBillingProfile_FollowsDefaultProfileChanges: settings the
// account does not control track the default profile, including the
// supplier.
func TestEnsureBillingProfile_FollowsDefaultProfileChanges(t *testing.T) {
	c, f := newTestClient(t)
	def := seedDefaultBillingProfile(f)
	desired := standardDesired("account-1")
	created, err := c.EnsureBillingProfile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("first EnsureBillingProfile: %v", err)
	}

	newName := "Acme Platform LLC"
	def.Supplier.Name = &newName
	def.Workflow.Invoicing.ProgressiveBilling = ptr(true)
	f.billingProfiles[def.Id] = def
	f.billingProfileUpdates = 0

	res, err := c.EnsureBillingProfile(context.Background(), created.Profile.Id, desired)
	if err != nil {
		t.Fatalf("second EnsureBillingProfile: %v", err)
	}
	if f.billingProfileUpdates != 1 {
		t.Errorf("updates = %d, want 1", f.billingProfileUpdates)
	}
	if res.Profile.Supplier.Name == nil || *res.Profile.Supplier.Name != newName {
		t.Errorf("Supplier.Name = %v, want %q", res.Profile.Supplier.Name, newName)
	}
	if !*res.Profile.Workflow.Invoicing.ProgressiveBilling {
		t.Error("progressiveBilling did not follow the default profile")
	}
}

func TestEnsureBillingProfile_ReportsAppsDrift(t *testing.T) {
	c, f := newTestClient(t)
	def := seedDefaultBillingProfile(f)
	desired := standardDesired("account-1")
	created, err := c.EnsureBillingProfile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("first EnsureBillingProfile: %v", err)
	}

	var apps om.BillingProfileAppsOrReference
	_ = apps.FromBillingProfileAppReferences(om.BillingProfileAppReferences{
		Invoicing: om.AppReference{Id: "app_invoicing_v2"},
		Payment:   om.AppReference{Id: "app_payment"},
		Tax:       om.AppReference{Id: "app_tax"},
	})
	def.Apps = apps
	f.billingProfiles[def.Id] = def
	f.billingProfileUpdates = 0

	res, err := c.EnsureBillingProfile(context.Background(), created.Profile.Id, desired)
	if err != nil {
		t.Fatalf("second EnsureBillingProfile: %v", err)
	}
	if !strings.Contains(res.AppsDrift, "app_invoicing_v2") {
		t.Errorf("AppsDrift = %q, want it to describe the invoicing app difference", res.AppsDrift)
	}
	if f.billingProfileUpdates != 0 || res.Profile.Id != created.Profile.Id {
		t.Errorf("apps drift must be reported, not acted on (updates=%d, id %q -> %q)",
			f.billingProfileUpdates, created.Profile.Id, res.Profile.Id)
	}
}

// TestEnsureBillingProfile_RefusesForeignAnnotatedProfile is the M2
// regression: BillingProfileIDAnnotation is writable by the account's own
// users, and EnsureBillingProfile used to adopt — and rewrite — whatever
// profile it named.
func TestEnsureBillingProfile_RefusesForeignAnnotatedProfile(t *testing.T) {
	for _, tt := range []struct {
		name  string
		setup func(t *testing.T, c Client, f *fakeServer) string
	}{
		{"another account's profile", func(t *testing.T, c Client, _ *fakeServer) string {
			other, err := c.EnsureBillingProfile(context.Background(), "", standardDesired("victim-account"))
			if err != nil {
				t.Fatalf("EnsureBillingProfile(victim): %v", err)
			}
			return other.Profile.Id
		}},
		{"the default profile", func(*testing.T, Client, *fakeServer) string { return "profile_default" }},
		{"an untagged profile", func(_ *testing.T, _ Client, f *fakeServer) string {
			f.billingProfiles["untagged"] = om.BillingProfile{Id: "untagged", Name: "manual"}
			return "untagged"
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, f := newTestClient(t)
			seedDefaultBillingProfile(f)
			target := tt.setup(t, c, f)
			before := f.billingProfiles[target]
			f.billingProfileUpdates = 0

			desired := standardDesired("attacker-account")
			desired.NetDays = 90
			res, err := c.EnsureBillingProfile(context.Background(), target, desired)
			if err != nil {
				t.Fatalf("EnsureBillingProfile: %v", err)
			}
			if res.IgnoredExistingID != target || res.IgnoredExistingReason == "" {
				t.Errorf("result = %+v, want %q reported as ignored with a reason", res, target)
			}
			if res.Profile.Id == target {
				t.Errorf("adopted the foreign profile %q", target)
			}
			if mismatch := BillingProfileOwnershipMismatch(res.Profile, "attacker-account"); mismatch != "" {
				t.Errorf("resulting profile is not the account's own: %s", mismatch)
			}
			if f.billingProfileUpdates != 0 {
				t.Errorf("updates = %d, want 0 (the foreign profile must not be touched)", f.billingProfileUpdates)
			}
			after := f.billingProfiles[target]
			if before.Workflow.Invoicing != nil && after.Workflow.Invoicing != nil &&
				*after.Workflow.Invoicing.DueAfter != *before.Workflow.Invoicing.DueAfter {
				t.Errorf("foreign profile's dueAfter changed: %s -> %s", *before.Workflow.Invoicing.DueAfter, *after.Workflow.Invoicing.DueAfter)
			}
		})
	}
}

func TestEnsureBillingProfile_RecreatesWhenExistingIDGone(t *testing.T) {
	c, f := newTestClient(t)
	seedDefaultBillingProfile(f)

	got, err := c.EnsureBillingProfile(context.Background(), "profile-that-no-longer-exists", standardDesired("account-1"))
	if err != nil {
		t.Fatalf("EnsureBillingProfile: %v", err)
	}
	if got.Profile.Id == "" {
		t.Error("expected a freshly created profile")
	}
	if got.IgnoredExistingID != "" {
		t.Errorf("a missing profile is not a refused one: %+v", got)
	}
}

func TestEnsureBillingProfile_RetryWithLostAnnotationAdoptsExistingProfile(t *testing.T) {
	// Simulates the exact race this fix closes: EnsureBillingProfile
	// succeeds and creates a profile, but the caller crashes/fails before
	// persisting the returned id anywhere (BillingProfileIDAnnotation never
	// lands). The next reconcile calls in with existingID == "" again, same
	// as the very first call — it must adopt the already-created profile by
	// AccountKey instead of creating a second, orphaned one.
	c, f := newTestClient(t)
	seedDefaultBillingProfile(f)
	desired := standardDesired("account-1")

	first, err := c.EnsureBillingProfile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("first EnsureBillingProfile (simulated pre-crash create): %v", err)
	}

	countBefore := len(f.billingProfiles)
	second, err := c.EnsureBillingProfile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("second EnsureBillingProfile (simulated retry after lost annotation): %v", err)
	}

	if second.Profile.Id != first.Profile.Id {
		t.Errorf("retry created a new profile instead of adopting the existing one: first=%q second=%q", first.Profile.Id, second.Profile.Id)
	}
	if len(f.billingProfiles) != countBefore {
		t.Errorf("retry created an orphaned duplicate profile: had %d, now %d", countBefore, len(f.billingProfiles))
	}
}

func TestEnsureBillingProfile_DifferentAccountsGetDifferentProfiles(t *testing.T) {
	// Guards against findBillingProfileByAccountKey being too loose (e.g.
	// matching on empty/zero-value Metadata) and cross-linking two
	// unrelated accounts onto the same profile.
	c, f := newTestClient(t)
	seedDefaultBillingProfile(f)

	a, err := c.EnsureBillingProfile(context.Background(), "", standardDesired("account-1"))
	if err != nil {
		t.Fatalf("EnsureBillingProfile(account-1): %v", err)
	}
	b, err := c.EnsureBillingProfile(context.Background(), "", standardDesired("account-2"))
	if err != nil {
		t.Fatalf("EnsureBillingProfile(account-2): %v", err)
	}
	if a.Profile.Id == b.Profile.Id {
		t.Errorf("two different accounts were assigned the same profile %q", a.Profile.Id)
	}
}

func TestFindBillingProfileByAccountKey_WalksBeyondFirstPage(t *testing.T) {
	// One BillingProfile is created per BillingAccount, so an org can have
	// more than billingProfileListPageSize profiles. Seed enough filler
	// profiles (created before the target) to push the target's tagged
	// profile onto page 2, and confirm the lookup still finds it instead of
	// stopping after page 1.
	c, f := newTestClient(t)
	seedDefaultBillingProfile(f)

	for i := range billingProfileListPageSize {
		id := fmt.Sprintf("filler-%03d", i)
		f.billingProfiles[id] = om.BillingProfile{Id: id, Name: "filler", CreatedAt: fakeProfileEpoch.Add(time.Duration(i) * time.Second)}
	}
	metadata := om.Metadata{billingProfileAccountKeyMetadataKey: "account-on-page-2"}
	f.billingProfiles["target"] = om.BillingProfile{
		Id:        "target",
		Name:      "target",
		Metadata:  &metadata,
		CreatedAt: fakeProfileEpoch.Add(time.Hour),
	}

	found, err := c.(*client).findBillingProfileByAccountKey(context.Background(), "account-on-page-2")
	if err != nil {
		t.Fatalf("findBillingProfileByAccountKey: %v", err)
	}
	if found == nil {
		t.Fatal("expected to find the profile tagged account-on-page-2, got nil (did the walk stop after page 1?)")
	}
	if found.Id != "target" {
		t.Errorf("found.Id = %q, want %q", found.Id, "target")
	}
}

// TestFindBillingProfiles_RestartsWhenADeletionShiftsPages: a profile
// deleted from page 1 while page 2 is being fetched shifts the first
// profile of page 2 onto page 1, which was already read. The walk must
// notice (TotalCount shrank) and start over rather than miss it.
func TestFindBillingProfiles_RestartsWhenADeletionShiftsPages(t *testing.T) {
	c, f := newTestClient(t)
	for i := range billingProfileListPageSize {
		id := fmt.Sprintf("filler-%03d", i)
		f.billingProfiles[id] = om.BillingProfile{Id: id, Name: "filler", CreatedAt: fakeProfileEpoch.Add(time.Duration(i) * time.Second)}
	}
	metadata := om.Metadata{billingProfileAccountKeyMetadataKey: "account-1"}
	f.billingProfiles["target"] = om.BillingProfile{
		Id: "target", Name: "target", Metadata: &metadata,
		CreatedAt: fakeProfileEpoch.Add(time.Hour),
	}
	deleted := false
	f.billingProfileListHook = func(f *fakeServer, page int) {
		if page == 2 && !deleted {
			delete(f.billingProfiles, "filler-000")
			deleted = true
		}
	}

	found, err := c.FindBillingProfilesByAccountKey(context.Background(), "account-1")
	if err != nil {
		t.Fatalf("FindBillingProfilesByAccountKey: %v", err)
	}
	if len(found) != 1 || found[0].Id != "target" {
		t.Errorf("found %v, want [target]", found)
	}
}

func TestFindBillingProfiles_WalksOldestFirst(t *testing.T) {
	c, f := newTestClient(t)
	seedDefaultBillingProfile(f)
	for _, key := range []CustomerKey{"account-1", "account-1"} {
		// Two profiles for one account: a duplicate left by a lost create
		// response. Both are returned, oldest first.
		if _, err := c.(*client).createBillingProfile(context.Background(), f.billingProfiles["profile_default"], standardDesired(key)); err != nil {
			t.Fatalf("createBillingProfile: %v", err)
		}
	}
	if _, err := c.EnsureBillingProfile(context.Background(), "", standardDesired("account-2")); err != nil {
		t.Fatalf("EnsureBillingProfile(account-2): %v", err)
	}

	found, err := c.FindBillingProfilesByAccountKey(context.Background(), "account-1")
	if err != nil {
		t.Fatalf("FindBillingProfilesByAccountKey: %v", err)
	}
	seen := make([]string, 0, len(found))
	for _, p := range found {
		seen = append(seen, p.Id)
	}
	if len(seen) != 2 || !found[0].CreatedAt.Before(found[1].CreatedAt) {
		t.Errorf("found %v, want both of account-1's profiles, oldest first", seen)
	}
	for _, p := range found {
		if p.Default {
			t.Error("the default profile must never be returned")
		}
	}
}

func TestGetDefaultBillingProfile_SingleRequestAmongManyProfiles(t *testing.T) {
	c, f := newTestClient(t)
	for i := range billingProfileListPageSize + 10 {
		id := fmt.Sprintf("filler-%03d", i)
		f.billingProfiles[id] = om.BillingProfile{Id: id, Name: "filler", CreatedAt: fakeProfileEpoch.Add(time.Duration(i) * time.Second)}
	}
	def := seedDefaultBillingProfile(f)
	def.CreatedAt = fakeProfileEpoch.Add(24 * time.Hour) // newest, last in any createdAt walk
	f.billingProfiles[def.Id] = def

	got, err := c.(*client).GetDefaultBillingProfile(context.Background())
	if err != nil {
		t.Fatalf("GetDefaultBillingProfile: %v", err)
	}
	if got.Id != def.Id {
		t.Errorf("got %q, want %q", got.Id, def.Id)
	}
	if f.billingProfileListCalls != 1 {
		t.Errorf("list calls = %d, want 1 (ordered by default, first item)", f.billingProfileListCalls)
	}
}

func TestGetDefaultBillingProfile_NotFound(t *testing.T) {
	c, f := newTestClient(t)
	f.billingProfiles["p"] = om.BillingProfile{Id: "p"}
	if _, err := c.(*client).GetDefaultBillingProfile(context.Background()); !errors.Is(err, ErrBillingProfileNotFound) {
		t.Errorf("err = %v, want ErrBillingProfileNotFound", err)
	}
}

func TestCustomerIDsByKey_IncludesDeletedAndMatchesExactly(t *testing.T) {
	c, f := newTestClient(t)
	key := "uid-1"
	longer := "uid-10"
	other := "uid-2"
	f.customers[key] = om.Customer{Id: "01LIVE", Key: &key}
	f.customers[longer] = om.Customer{Id: "01PARTIAL", Key: &longer}
	f.customers[other] = om.Customer{Id: "01OTHER", Key: &other}
	deletedAt := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	f.deletedCustomers = []om.Customer{{Id: "01DELETED", Key: &key, DeletedAt: &deletedAt}}

	ids, err := c.CustomerIDsByKey(context.Background(), CustomerKey(key))
	if err != nil {
		t.Fatalf("CustomerIDsByKey: %v", err)
	}
	got := make([]string, 0, len(ids))
	for _, id := range ids {
		got = append(got, string(id))
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "01DELETED,01LIVE" {
		t.Errorf("ids = %v, want [01DELETED 01LIVE] (exact key match, deleted included)", got)
	}
}

func TestBillingProfileOwnershipMismatch(t *testing.T) {
	tagged := func(key string) *om.Metadata { return &om.Metadata{billingProfileAccountKeyMetadataKey: key} }
	for _, tt := range []struct {
		name    string
		profile om.BillingProfile
		want    bool
	}{
		{"own", om.BillingProfile{Id: "p", Metadata: tagged("a")}, false},
		{"other account", om.BillingProfile{Id: "p", Metadata: tagged("b")}, true},
		{"untagged", om.BillingProfile{Id: "p"}, true},
		{"default even if tagged", om.BillingProfile{Id: "p", Default: true, Metadata: tagged("a")}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := BillingProfileOwnershipMismatch(tt.profile, "a") != ""; got != tt.want {
				t.Errorf("mismatch = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUpsertAndDeleteBillingProfileCustomerOverride(t *testing.T) {
	c, f := newTestClient(t)

	if err := c.UpsertBillingProfileCustomerOverride(context.Background(), "cust-1", "profile_x"); err != nil {
		t.Fatalf("UpsertBillingProfileCustomerOverride: %v", err)
	}
	stored, ok := f.billingProfileOverrides["cust-1"]
	if !ok || stored.BillingProfileId == nil || *stored.BillingProfileId != "profile_x" {
		t.Errorf("override not stored correctly: %+v", stored)
	}

	if err := c.DeleteBillingProfileCustomerOverride(context.Background(), "cust-1"); err != nil {
		t.Fatalf("DeleteBillingProfileCustomerOverride: %v", err)
	}
	if _, ok := f.billingProfileOverrides["cust-1"]; ok {
		t.Error("override still present after delete")
	}
}

func TestDeleteBillingProfileCustomerOverride_NotFoundIsSuccess(t *testing.T) {
	c, _ := newTestClient(t)
	if err := c.DeleteBillingProfileCustomerOverride(context.Background(), "cust-missing"); err != nil {
		t.Errorf("DeleteBillingProfileCustomerOverride on missing override: %v", err)
	}
}

func TestDeleteBillingProfile_NotFoundIsSuccess(t *testing.T) {
	c, _ := newTestClient(t)
	if err := c.DeleteBillingProfile(context.Background(), "missing"); err != nil {
		t.Errorf("DeleteBillingProfile on missing id: %v", err)
	}
}

func TestGetBillingProfile_NotFound(t *testing.T) {
	c, _ := newTestClient(t)
	_, err := c.GetBillingProfile(context.Background(), "missing")
	if !errors.Is(err, ErrBillingProfileNotFound) {
		t.Errorf("GetBillingProfile error = %v, want ErrBillingProfileNotFound", err)
	}
}

// TestEnsureBillingProfile_ConvergesWhenServerNormalizesAlignment is the
// regression test for the highest-impact bug found in review: OpenMeter
// accepts the anchored collection alignment EnsureBillingProfile sends but
// stores {type: "subscription"} instead, so a drift check that compares
// against what was SENT can never be satisfied — every reconcile of every
// BillingAccount issued an UPDATE, forever.
//
// It went unnoticed because the fake used to echo the submitted workflow
// back verbatim; it now mimics the real rewrite (see
// normalizeWorkflowLikeOpenMeter), so this asserts the thing that
// actually matters: a second Ensure with unchanged input writes nothing.
func TestEnsureBillingProfile_ConvergesWhenServerNormalizesAlignment(t *testing.T) {
	c, f := newTestClient(t)
	seedDefaultBillingProfile(f)
	desired := standardDesired("account-1")

	created, err := c.EnsureBillingProfile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("first EnsureBillingProfile: %v", err)
	}
	f.billingProfileUpdates = 0

	for i := range 3 {
		if _, err := c.EnsureBillingProfile(context.Background(), created.Profile.Id, desired); err != nil {
			t.Fatalf("EnsureBillingProfile #%d: %v", i+2, err)
		}
	}
	if f.billingProfileUpdates != 0 {
		t.Errorf("converged EnsureBillingProfile issued %d update(s) across 3 no-op calls, want 0 — the drift check is comparing against something the server never stores", f.billingProfileUpdates)
	}
}

// TestEnsureBillingProfile_StillUpdatesOnRealDrift guards the other
// direction: the convergence fix must not have made the drift check inert.
func TestEnsureBillingProfile_StillUpdatesOnRealDrift(t *testing.T) {
	c, f := newTestClient(t)
	seedDefaultBillingProfile(f)
	desired := standardDesired("account-1")
	created, err := c.EnsureBillingProfile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("first EnsureBillingProfile: %v", err)
	}
	f.billingProfileUpdates = 0

	desired.NetDays = 60
	updated, err := c.EnsureBillingProfile(context.Background(), created.Profile.Id, desired)
	if err != nil {
		t.Fatalf("second EnsureBillingProfile: %v", err)
	}
	if f.billingProfileUpdates != 1 {
		t.Errorf("netDays drift produced %d update(s), want exactly 1", f.billingProfileUpdates)
	}
	inv := updated.Profile.Workflow.Invoicing
	if inv == nil || inv.DueAfter == nil || *inv.DueAfter != "P60D" {
		t.Errorf("DueAfter not updated: %+v", inv)
	}
}

// TestBillingProfileNeedsUpdate_ToleratesAbsentWorkflowSections guards the
// nil-deref found in review: Workflow.Invoicing and Workflow.Collection are
// both optional pointers in the SDK, but only their inner fields were
// nil-checked. A profile returned without either section panicked the
// reconciler (recovered by controller-runtime into an endless retry).
func TestBillingProfileNeedsUpdate_ToleratesAbsentWorkflowSections(t *testing.T) {
	desired := standardDesired("k")
	for _, tt := range []struct {
		name    string
		profile om.BillingProfile
	}{
		{"entirely empty workflow", om.BillingProfile{}},
		{"invoicing present, collection absent", om.BillingProfile{Workflow: om.BillingWorkflow{
			Invoicing: &om.BillingWorkflowInvoicingSettings{},
		}}},
		{"collection present, invoicing absent", om.BillingProfile{Workflow: om.BillingWorkflow{
			Collection: &om.BillingWorkflowCollectionSettings{},
		}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// Must not panic; the value itself is unimportant. Exercise
			// both the profile and the default profile being sparse.
			_ = billingProfileNeedsUpdate(tt.profile, om.BillingProfile{}, desired)
			_ = billingProfileNeedsUpdate(tt.profile, tt.profile, desired)
		})
	}
}

// TestGetBillingProfile_SoftDeletedReadsAsNotFound covers OpenMeter's
// soft-delete semantics: a deleted profile keeps answering GET with 200 and
// a populated deletedAt instead of 404 (verified live). Reporting that as
// live would make EnsureBillingProfile try to UPDATE a deleted record rather
// than create a replacement, wedging the account.
func TestGetBillingProfile_SoftDeletedReadsAsNotFound(t *testing.T) {
	c, f := newTestClient(t)
	seedDefaultBillingProfile(f)
	created, err := c.EnsureBillingProfile(context.Background(), "", standardDesired("account-1"))
	if err != nil {
		t.Fatalf("EnsureBillingProfile: %v", err)
	}

	// Soft-delete it the way the real server does: still present, but with
	// deletedAt set.
	deleted := f.billingProfiles[created.Profile.Id]
	at := time.Date(2026, 9, 9, 18, 28, 54, 0, time.UTC)
	deleted.DeletedAt = &at
	f.billingProfiles[created.Profile.Id] = deleted

	if _, err := c.GetBillingProfile(context.Background(), created.Profile.Id); !errors.Is(err, ErrBillingProfileNotFound) {
		t.Errorf("GetBillingProfile on a soft-deleted profile returned %v, want ErrBillingProfileNotFound", err)
	}
}

// TestEnsureBillingProfile_RecreatesWhenExistingWasSoftDeleted is the
// end-to-end consequence: an account whose profile was deleted out of band
// must get a fresh one rather than getting stuck updating a dead record.
func TestEnsureBillingProfile_RecreatesWhenExistingWasSoftDeleted(t *testing.T) {
	c, f := newTestClient(t)
	seedDefaultBillingProfile(f)
	desired := standardDesired("account-1")
	created, err := c.EnsureBillingProfile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("first EnsureBillingProfile: %v", err)
	}

	deleted := f.billingProfiles[created.Profile.Id]
	at := time.Date(2026, 9, 9, 18, 28, 54, 0, time.UTC)
	deleted.DeletedAt = &at
	f.billingProfiles[created.Profile.Id] = deleted

	replacement, err := c.EnsureBillingProfile(context.Background(), created.Profile.Id, desired)
	if err != nil {
		t.Fatalf("EnsureBillingProfile after soft delete: %v", err)
	}
	if replacement.Profile.Id == created.Profile.Id {
		t.Errorf("reused the soft-deleted profile %q instead of creating a replacement", created.Profile.Id)
	}
	if replacement.Profile.DeletedAt != nil {
		t.Errorf("replacement profile %q is itself soft-deleted", replacement.Profile.Id)
	}
}
