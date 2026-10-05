// SPDX-License-Identifier: AGPL-3.0-only

package openmeter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	om "github.com/openmeterio/openmeter/api/client/go"
)

// serveBillingProfiles handles /api/v1/billing/profiles[/{id}] and
// /api/v1/billing/customers/{customerKey} (the override endpoints). Returns
// false when the request isn't one of those routes, so ServeHTTP can fall
// through to other resources.
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
		id := fmt.Sprintf("profile_%s_%d", body.Name, len(f.billingProfiles))
		p := om.BillingProfile{
			Id:       id,
			Name:     body.Name,
			Default:  body.Default,
			Supplier: body.Supplier,
			Metadata: body.Metadata,
			Workflow: om.BillingWorkflow{
				Invoicing:  body.Workflow.Invoicing,
				Collection: normalizeCollectionLikeOpenMeter(body.Workflow.Collection),
			},
		}
		f.billingProfiles[id] = p
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(p)
		return true

	case r.Method == http.MethodGet && r.URL.Path == profilesPath:
		var items []om.BillingProfile
		for _, p := range f.billingProfiles {
			items = append(items, p)
		}
		// Deterministic order so pagination (page/pageSize below) is
		// actually meaningful across calls, not dependent on Go's
		// randomized map iteration order.
		sort.Slice(items, func(i, j int) bool { return items[i].Id < items[j].Id })

		page := 1
		if v := r.URL.Query().Get("page"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				page = n
			}
		}
		pageSize := 100
		if v := r.URL.Query().Get("pageSize"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				pageSize = n
			}
		}
		total := len(items)
		start := min((page-1)*pageSize, total)
		end := min(start+pageSize, total)

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
			p.Name = body.Name
			p.Default = body.Default
			p.Supplier = body.Supplier
			p.Metadata = body.Metadata
			f.billingProfileUpdates++
			p.Workflow = body.Workflow
			p.Workflow.Collection = normalizeCollectionLikeOpenMeter(body.Workflow.Collection)
			f.billingProfiles[id] = p
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(p)
			return true
		case http.MethodDelete:
			if _, ok := f.billingProfiles[id]; !ok {
				w.WriteHeader(http.StatusNotFound)
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

// normalizeCollectionLikeOpenMeter reproduces what a real OpenMeter does to
// a submitted collection workflow: whatever alignment you send, it stores
// and returns {type: "subscription"} with a PT1H interval. Verified live by
// POSTing {anchored, anchor 2024-01-15, interval P3M} and reading back
// {type: subscription, interval: PT1H}.
//
// The fake previously echoed the submitted workflow back verbatim, which is
// precisely why TestEnsureBillingProfile_NoopWhenUpToDate passed while
// production issued an UPDATE on every single reconcile forever: the drift
// check compared against values the real server never keeps. Any fake that
// is more permissive than the real API can hide a non-convergence bug like
// that, so it mimics the rewrite instead.
func normalizeCollectionLikeOpenMeter(_ *om.BillingWorkflowCollectionSettings) *om.BillingWorkflowCollectionSettings {
	var alignment om.BillingWorkflowCollectionAlignment
	if err := alignment.FromBillingWorkflowCollectionAlignmentSubscription(
		om.BillingWorkflowCollectionAlignmentSubscription{
			Type: om.BillingWorkflowCollectionAlignmentSubscriptionTypeSubscription,
		},
	); err != nil {
		panic(err) // static payload; a failure here is a broken test fixture
	}
	interval := "PT1H"
	return &om.BillingWorkflowCollectionSettings{Alignment: &alignment, Interval: &interval}
}

func seedDefaultBillingProfile(f *fakeServer) om.BillingProfile {
	appRefs := om.BillingProfileAppReferences{
		Invoicing: om.AppReference{Id: "app_invoicing"},
		Payment:   om.AppReference{Id: "app_payment"},
		Tax:       om.AppReference{Id: "app_tax"},
	}
	var apps om.BillingProfileAppsOrReference
	_ = apps.FromBillingProfileAppReferences(appRefs)
	supplierName := "Acme Platform Inc."
	p := om.BillingProfile{
		Id:      "profile_default",
		Name:    "Default",
		Default: true,
		Apps:    apps,
		Supplier: om.BillingParty{
			Name: &supplierName,
		},
	}
	f.billingProfiles[p.Id] = p
	return p
}

func TestEnsureBillingProfile_CreatesFromDefault(t *testing.T) {
	c, f := newTestClient(t)
	seedDefaultBillingProfile(f)

	desired := DesiredBillingProfile{
		AccountKey:        "account-1",
		Name:              "org-a/acme payment terms",
		NetDays:           45,
		InvoiceFrequency:  "Quarterly",
		InvoiceDayOfMonth: 15,
	}
	got, err := c.EnsureBillingProfile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("EnsureBillingProfile: %v", err)
	}
	if got.Default {
		t.Error("per-account profile must not be marked Default")
	}
	if got.Supplier.Name == nil || *got.Supplier.Name != "Acme Platform Inc." {
		t.Errorf("Supplier not cloned from default profile: %+v", got.Supplier)
	}
	if got.Workflow.Invoicing.DueAfter == nil || *got.Workflow.Invoicing.DueAfter != "P45D" {
		t.Errorf("DueAfter = %v, want P45D", got.Workflow.Invoicing.DueAfter)
	}
}

func TestEnsureBillingProfile_NoDefaultIsPermanentError(t *testing.T) {
	c, _ := newTestClient(t)
	_, err := c.EnsureBillingProfile(context.Background(), "", DesiredBillingProfile{AccountKey: "account-1", Name: "x", NetDays: 30, InvoiceFrequency: "Monthly", InvoiceDayOfMonth: 1})
	if !IsPermanent(err) {
		t.Errorf("expected a PermanentError when no default profile is provisioned, got %T: %v", err, err)
	}
}

func TestEnsureBillingProfile_RequiresAccountKey(t *testing.T) {
	c, f := newTestClient(t)
	seedDefaultBillingProfile(f)
	_, err := c.EnsureBillingProfile(context.Background(), "", DesiredBillingProfile{Name: "x", NetDays: 30, InvoiceFrequency: "Monthly", InvoiceDayOfMonth: 1})
	if !IsPermanent(err) {
		t.Errorf("expected a PermanentError when AccountKey is empty, got %T: %v", err, err)
	}
}

func TestEnsureBillingProfile_NoopWhenUpToDate(t *testing.T) {
	c, f := newTestClient(t)
	seedDefaultBillingProfile(f)
	desired := DesiredBillingProfile{AccountKey: "account-1", Name: "x", NetDays: 30, InvoiceFrequency: "Monthly", InvoiceDayOfMonth: 1}

	first, err := c.EnsureBillingProfile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("first EnsureBillingProfile: %v", err)
	}
	second, err := c.EnsureBillingProfile(context.Background(), first.Id, desired)
	if err != nil {
		t.Fatalf("second EnsureBillingProfile: %v", err)
	}
	if second.Id != first.Id {
		t.Errorf("Id changed across a no-drift EnsureBillingProfile: %q -> %q", first.Id, second.Id)
	}
}

func TestEnsureBillingProfile_UpdatesOnDrift(t *testing.T) {
	c, f := newTestClient(t)
	seedDefaultBillingProfile(f)
	desired := DesiredBillingProfile{AccountKey: "account-1", Name: "x", NetDays: 30, InvoiceFrequency: "Monthly", InvoiceDayOfMonth: 1}
	created, err := c.EnsureBillingProfile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("first EnsureBillingProfile: %v", err)
	}

	desired.NetDays = 60
	updated, err := c.EnsureBillingProfile(context.Background(), created.Id, desired)
	if err != nil {
		t.Fatalf("second EnsureBillingProfile: %v", err)
	}
	if updated.Workflow.Invoicing.DueAfter == nil || *updated.Workflow.Invoicing.DueAfter != "P60D" {
		t.Errorf("DueAfter = %v, want P60D", updated.Workflow.Invoicing.DueAfter)
	}
}

func TestEnsureBillingProfile_UpdatePreservesAccountKeyMetadata(t *testing.T) {
	// updateBillingProfile PUTs a full replacement; if it forgot to carry
	// existing.Metadata forward, this tag would be silently wiped on the
	// very first update, breaking findBillingProfileByAccountKey for good.
	c, f := newTestClient(t)
	seedDefaultBillingProfile(f)
	desired := DesiredBillingProfile{AccountKey: "account-1", Name: "x", NetDays: 30, InvoiceFrequency: "Monthly", InvoiceDayOfMonth: 1}
	created, err := c.EnsureBillingProfile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("first EnsureBillingProfile: %v", err)
	}

	desired.NetDays = 60
	if _, err := c.EnsureBillingProfile(context.Background(), created.Id, desired); err != nil {
		t.Fatalf("second EnsureBillingProfile: %v", err)
	}

	stored := f.billingProfiles[created.Id]
	if stored.Metadata == nil || (*stored.Metadata)[billingProfileAccountKeyMetadataKey] != "account-1" {
		t.Errorf("Metadata[%q] lost after update: %+v", billingProfileAccountKeyMetadataKey, stored.Metadata)
	}
}

func TestEnsureBillingProfile_RecreatesWhenExistingIDGone(t *testing.T) {
	c, f := newTestClient(t)
	seedDefaultBillingProfile(f)
	desired := DesiredBillingProfile{AccountKey: "account-1", Name: "x", NetDays: 30, InvoiceFrequency: "Monthly", InvoiceDayOfMonth: 1}

	got, err := c.EnsureBillingProfile(context.Background(), "profile-that-no-longer-exists", desired)
	if err != nil {
		t.Fatalf("EnsureBillingProfile: %v", err)
	}
	if got.Id == "" {
		t.Error("expected a freshly created profile")
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
	desired := DesiredBillingProfile{AccountKey: "account-1", Name: "x", NetDays: 30, InvoiceFrequency: "Monthly", InvoiceDayOfMonth: 1}

	first, err := c.EnsureBillingProfile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("first EnsureBillingProfile (simulated pre-crash create): %v", err)
	}

	countBefore := len(f.billingProfiles)
	second, err := c.EnsureBillingProfile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("second EnsureBillingProfile (simulated retry after lost annotation): %v", err)
	}

	if second.Id != first.Id {
		t.Errorf("retry created a new profile instead of adopting the existing one: first=%q second=%q", first.Id, second.Id)
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

	a, err := c.EnsureBillingProfile(context.Background(), "", DesiredBillingProfile{AccountKey: "account-1", Name: "a", NetDays: 30, InvoiceFrequency: "Monthly", InvoiceDayOfMonth: 1})
	if err != nil {
		t.Fatalf("EnsureBillingProfile(account-1): %v", err)
	}
	b, err := c.EnsureBillingProfile(context.Background(), "", DesiredBillingProfile{AccountKey: "account-2", Name: "b", NetDays: 30, InvoiceFrequency: "Monthly", InvoiceDayOfMonth: 1})
	if err != nil {
		t.Fatalf("EnsureBillingProfile(account-2): %v", err)
	}
	if a.Id == b.Id {
		t.Errorf("two different accounts were assigned the same profile %q", a.Id)
	}
}

func TestFindBillingProfileByAccountKey_WalksBeyondFirstPage(t *testing.T) {
	// One BillingProfile is created per BillingAccount, so an org can have
	// more than billingProfileListPageSize profiles. Seed enough filler
	// profiles (sorting before the target alphabetically) to push the
	// target's tagged profile onto page 2, and confirm the lookup still
	// finds it instead of stopping after page 1.
	c, f := newTestClient(t)
	seedDefaultBillingProfile(f)

	for i := range billingProfileListPageSize {
		id := fmt.Sprintf("filler-%03d", i)
		f.billingProfiles[id] = om.BillingProfile{Id: id, Name: "filler"}
	}
	metadata := om.Metadata{billingProfileAccountKeyMetadataKey: "account-on-page-2"}
	f.billingProfiles["zzz-target"] = om.BillingProfile{
		Id:       "zzz-target",
		Name:     "target",
		Metadata: &metadata,
	}

	found, err := c.(*client).findBillingProfileByAccountKey(context.Background(), "account-on-page-2")
	if err != nil {
		t.Fatalf("findBillingProfileByAccountKey: %v", err)
	}
	if found == nil {
		t.Fatal("expected to find the profile tagged account-on-page-2, got nil (did the walk stop after page 1?)")
	}
	if found.Id != "zzz-target" {
		t.Errorf("found.Id = %q, want %q", found.Id, "zzz-target")
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
// normalizeCollectionLikeOpenMeter), so this asserts the thing that
// actually matters: a second Ensure with unchanged input writes nothing.
func TestEnsureBillingProfile_ConvergesWhenServerNormalizesAlignment(t *testing.T) {
	c, f := newTestClient(t)
	seedDefaultBillingProfile(f)
	desired := DesiredBillingProfile{
		AccountKey: "account-1", Name: "x",
		NetDays: 30, InvoiceFrequency: "Monthly", InvoiceDayOfMonth: 1,
	}

	created, err := c.EnsureBillingProfile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("first EnsureBillingProfile: %v", err)
	}
	f.billingProfileUpdates = 0

	for i := range 3 {
		if _, err := c.EnsureBillingProfile(context.Background(), created.Id, desired); err != nil {
			t.Fatalf("EnsureBillingProfile #%d: %v", i+2, err)
		}
	}
	if f.billingProfileUpdates != 0 {
		t.Errorf("converged EnsureBillingProfile issued %d update(s) across 3 no-op calls, want 0 — the drift check is comparing against something the server never stores", f.billingProfileUpdates)
	}
}

// TestEnsureBillingProfile_StillUpdatesOnRealDrift guards the other
// direction: the convergence fix must not have made the drift check inert.
// NetDays is the one paymentTerms field OpenMeter actually round-trips.
func TestEnsureBillingProfile_StillUpdatesOnRealDrift(t *testing.T) {
	c, f := newTestClient(t)
	seedDefaultBillingProfile(f)
	desired := DesiredBillingProfile{
		AccountKey: "account-1", Name: "x",
		NetDays: 30, InvoiceFrequency: "Monthly", InvoiceDayOfMonth: 1,
	}
	created, err := c.EnsureBillingProfile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("first EnsureBillingProfile: %v", err)
	}
	f.billingProfileUpdates = 0

	desired.NetDays = 60
	updated, err := c.EnsureBillingProfile(context.Background(), created.Id, desired)
	if err != nil {
		t.Fatalf("second EnsureBillingProfile: %v", err)
	}
	if f.billingProfileUpdates != 1 {
		t.Errorf("netDays drift produced %d update(s), want exactly 1", f.billingProfileUpdates)
	}
	if updated.Workflow.Invoicing == nil || updated.Workflow.Invoicing.DueAfter == nil || *updated.Workflow.Invoicing.DueAfter != "P60D" {
		t.Errorf("DueAfter not updated: %+v", updated.Workflow.Invoicing)
	}
}

// TestBillingProfileNeedsUpdate_ToleratesAbsentWorkflowSections guards the
// nil-deref found in review: Workflow.Invoicing and Workflow.Collection are
// both optional pointers in the SDK, but only their inner fields were
// nil-checked. A profile returned without either section panicked the
// reconciler (recovered by controller-runtime into an endless retry).
func TestBillingProfileNeedsUpdate_ToleratesAbsentWorkflowSections(t *testing.T) {
	desired := DesiredBillingProfile{
		AccountKey: "k", Name: "n",
		NetDays: 30, InvoiceFrequency: "Monthly", InvoiceDayOfMonth: 1,
	}
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
			// Must not panic; the value itself is unimportant.
			_ = billingProfileNeedsUpdate(tt.profile, desired)
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
	desired := DesiredBillingProfile{
		AccountKey: "account-1", Name: "x",
		NetDays: 30, InvoiceFrequency: "Monthly", InvoiceDayOfMonth: 1,
	}
	created, err := c.EnsureBillingProfile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("EnsureBillingProfile: %v", err)
	}

	// Soft-delete it the way the real server does: still present, but with
	// deletedAt set.
	deleted := f.billingProfiles[created.Id]
	at := time.Date(2026, 9, 9, 18, 28, 54, 0, time.UTC)
	deleted.DeletedAt = &at
	f.billingProfiles[created.Id] = deleted

	if _, err := c.GetBillingProfile(context.Background(), created.Id); !errors.Is(err, ErrBillingProfileNotFound) {
		t.Errorf("GetBillingProfile on a soft-deleted profile returned %v, want ErrBillingProfileNotFound", err)
	}
}

// TestEnsureBillingProfile_RecreatesWhenExistingWasSoftDeleted is the
// end-to-end consequence: an account whose profile was deleted out of band
// must get a fresh one rather than getting stuck updating a dead record.
func TestEnsureBillingProfile_RecreatesWhenExistingWasSoftDeleted(t *testing.T) {
	c, f := newTestClient(t)
	seedDefaultBillingProfile(f)
	desired := DesiredBillingProfile{
		AccountKey: "account-1", Name: "x",
		NetDays: 30, InvoiceFrequency: "Monthly", InvoiceDayOfMonth: 1,
	}
	created, err := c.EnsureBillingProfile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("first EnsureBillingProfile: %v", err)
	}

	deleted := f.billingProfiles[created.Id]
	at := time.Date(2026, 9, 9, 18, 28, 54, 0, time.UTC)
	deleted.DeletedAt = &at
	// Drop the account-key tag too: the list endpoint excludes soft-deleted
	// profiles, so findBillingProfileByAccountKey would not see it either.
	deleted.Metadata = nil
	f.billingProfiles[created.Id] = deleted

	replacement, err := c.EnsureBillingProfile(context.Background(), created.Id, desired)
	if err != nil {
		t.Fatalf("EnsureBillingProfile after soft delete: %v", err)
	}
	if replacement.Id == created.Id {
		t.Errorf("reused the soft-deleted profile %q instead of creating a replacement", created.Id)
	}
	if replacement.DeletedAt != nil {
		t.Errorf("replacement profile %q is itself soft-deleted", replacement.Id)
	}
}
