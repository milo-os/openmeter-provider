// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"

	om "github.com/openmeterio/openmeter/api/client/go"
	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

func baseOffer() *billingv1alpha1.Offer {
	return &billingv1alpha1.Offer{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-offer",
			UID:  types.UID("test-uid"),
		},
		Spec: billingv1alpha1.OfferSpec{
			LaunchStage: billingv1alpha1.OfferLaunchStageGA,
		},
	}
}

// TestDesiredPlan_NamePrecedence verifies that the display name annotation
// is used if present, otherwise falling back to metadata.name.
func TestDesiredPlan_NamePrecedence(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		wantName    string
	}{
		{
			name:        "annotation present",
			annotations: map[string]string{billingv1alpha1.DisplayNameAnnotation: "Custom Display Name"},
			wantName:    "Custom Display Name",
		},
		{
			name:        "no annotation falls back to metadata name",
			annotations: nil,
			wantName:    "test-offer",
		},
		{
			name:        "empty annotation falls back to metadata name",
			annotations: map[string]string{billingv1alpha1.DisplayNameAnnotation: ""},
			wantName:    "test-offer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := runtime.NewScheme()
			if err := clientgoscheme.AddToScheme(s); err != nil {
				t.Fatalf("failed to add to scheme: %v", err)
			}
			if err := billingv1alpha1.AddToScheme(s); err != nil {
				t.Fatalf("failed to add billing API to scheme: %v", err)
			}

			// Fake client is needed because desiredPlan calls List on MeterDefinitions.
			// It will return an empty list, which is fine for this test.
			fakeClient := fake.NewClientBuilder().WithScheme(s).Build()

			r := &OfferReconciler{
				Client: fakeClient,
			}

			offer := baseOffer()
			offer.Annotations = tt.annotations

			ctx := context.Background()
			desired, err := r.desiredPlan(ctx, offer)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if desired.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", desired.Name, tt.wantName)
			}
			if desired.Key != "test_uid" {
				t.Errorf("Key = %q, want %q", desired.Key, "test_uid")
			}
			if desired.Description != "test-offer" {
				t.Errorf("Description = %q, want %q", desired.Description, "test-offer")
			}
		})
	}
}

type fakeOpenMeterClient struct {
	openmeter.Client
}

func (f *fakeOpenMeterClient) EnsureFeature(ctx context.Context, desired openmeter.DesiredFeature) (om.Feature, error) {
	return om.Feature{
		Key: desired.Key,
	}, nil
}

// TestDesiredPlan_Mapping tests the core mapping logic from ServicePricings
// to OpenMeter desired plans, rate cards, and metadata.
func TestDesiredPlan_Mapping(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("failed to add to scheme: %v", err)
	}
	if err := billingv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("failed to add billing API to scheme: %v", err)
	}

	meterDef := &billingv1alpha1.MeterDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-meter", // The slug
		},
		Spec: billingv1alpha1.MeterDefinitionSpec{
			MeterName: "compute.miloapis.com/cpu",
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(meterDef).Build()

	r := &OfferReconciler{
		Client:          fakeClient,
		OpenMeterClient: &fakeOpenMeterClient{},
	}

	offer := baseOffer()
	offer.Spec.ServicePricings = []billingv1alpha1.ServicePricingSnapshot{
		{
			Name: "usage-item",
			Spec: billingv1alpha1.ServicePricingSpec{
				DisplayName: "CPU Usage",
				ChargeType:  billingv1alpha1.ChargeTypeUsage,
				ServiceRef:  "compute.miloapis.com",
				Metric:      "compute.miloapis.com/cpu",
				PricingUnit: "vcpu",
				Rates: []billingv1alpha1.PricingRate{
					{
						Flat: "0.05",
					},
				},
			},
		},
		{
			Name: "recurring-item",
			Spec: billingv1alpha1.ServicePricingSpec{
				DisplayName: "Base Fee",
				ChargeType:  billingv1alpha1.ChargeTypeRecurring,
				ServiceRef:  "compute.miloapis.com",
				Amount:      "10.00",
			},
		},
	}

	ctx := context.Background()
	desired, err := r.desiredPlan(ctx, offer)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(desired.Phases) != 1 {
		t.Fatalf("expected 1 phase, got %d", len(desired.Phases))
	}
	if len(desired.Phases[0].RateCards) != 2 {
		t.Fatalf("expected 2 rate cards, got %d", len(desired.Phases[0].RateCards))
	}

	// First rate card should be usage based
	rc0, err := desired.Phases[0].RateCards[0].AsRateCardUsageBased()
	if err != nil {
		t.Fatalf("expected usage based rate card: %v", err)
	}
	if rc0.Name != "CPU Usage" {
		t.Errorf("expected name 'CPU Usage', got %q", rc0.Name)
	}
	if *rc0.FeatureKey != "test_meter_default" {
		t.Errorf("expected feature key 'test_meter_default', got %q", *rc0.FeatureKey)
	}
	if rc0.Metadata == nil || (*rc0.Metadata)["miloapis.com/pricing-unit"] != "vcpu" {
		t.Errorf("expected pricing unit 'vcpu', got %+v", rc0.Metadata)
	}

	// Second rate card should be flat fee
	rc1, err := desired.Phases[0].RateCards[1].AsRateCardFlatFee()
	if err != nil {
		t.Fatalf("expected flat fee rate card: %v", err)
	}
	if rc1.Name != "Base Fee" {
		t.Errorf("expected name 'Base Fee', got %q", rc1.Name)
	}
	if rc1.BillingCadence == nil || *rc1.BillingCadence != "P1M" {
		t.Errorf("expected billing cadence 'P1M', got %v", rc1.BillingCadence)
	}
	if rc1.Metadata == nil || (*rc1.Metadata)["miloapis.com/service-ref"] != "compute.miloapis.com" {
		t.Errorf("expected service ref 'compute.miloapis.com', got %+v", rc1.Metadata)
	}
}
