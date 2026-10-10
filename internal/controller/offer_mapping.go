// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	om "github.com/openmeterio/openmeter/api/client/go"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"

	"go.miloapis.com/openmeter-provider/internal/openmeter"
)

// This file is the pure translation of a GA Offer snapshot into the OpenMeter
// objects that bill it: one plan with a single phase, plus the usage features
// its rate cards point at. It performs no I/O, so every validation failure is
// reported before anything is written to OpenMeter.

const (
	// openMeterKeyMaxLen is OpenMeter's limit for feature and rate card keys.
	openMeterKeyMaxLen = 64
	// keyHashLen is the number of hex characters of the definition hash
	// appended to generated keys.
	keyHashLen = 10

	planPhaseKey       = "default_phase"
	planPhaseName      = "Default Phase"
	usageBillingPeriod = "P1M"

	metadataOfferName      = "miloapis.com/offer-name"
	metadataServiceRef     = "miloapis.com/service-ref"
	metadataServicePricing = "miloapis.com/service-pricing"
	metadataPricingUnit    = "miloapis.com/pricing-unit"
	metadataTrigger        = "miloapis.com/trigger"
	metadataMeterName      = "miloapis.com/meter-name"
)

// errMeterNotFound marks a usage pricing whose metric has no MeterDefinition
// yet. It is the only mapping failure that resolves on its own (the meter is
// usually created alongside the Offer), so callers retry it quickly.
var errMeterNotFound = errors.New("meter definition not found")

// invalidOfferError marks an Offer snapshot that cannot be represented in
// OpenMeter. GA Offers are immutable, so it never resolves without a new
// Offer (or a provider upgrade); callers back off.
type invalidOfferError struct{ msg string }

func (e *invalidOfferError) Error() string { return e.msg }

func invalidOffer(format string, args ...any) error {
	return &invalidOfferError{msg: fmt.Sprintf(format, args...)}
}

// meterInfo is what the mapping needs to know about a MeterDefinition.
type meterInfo struct {
	// Name is the canonical spec.meterName.
	Name string
	// Slug is the OpenMeter meter slug derived from Name.
	Slug string
	// Dimensions are the meter's group-by keys; filters on anything else
	// are rejected by OpenMeter.
	Dimensions []string
}

// offerMapping is the full OpenMeter projection of a GA Offer.
type offerMapping struct {
	Plan openmeter.DesiredPlan
	// Features are the usage features referenced by Plan, deduplicated, in
	// rate card order.
	Features []openmeter.DesiredFeature
	// MeterSlugs are the distinct meters the features are bound to.
	MeterSlugs []string
}

// planKeyForOffer is the OpenMeter plan key of an Offer. The UID is stable for
// the Offer's lifetime and never reused, and OpenMeter keys forbid hyphens.
func planKeyForOffer(offer *billingv1alpha1.Offer) string {
	return strings.ReplaceAll(string(offer.UID), "-", "_")
}

// buildOfferMapping translates offer into its OpenMeter plan and features.
// meters is keyed by spec.meterName. It returns an error wrapping
// errMeterNotFound when a metric has no MeterDefinition, and an
// *invalidOfferError when the snapshot cannot be represented.
func buildOfferMapping(offer *billingv1alpha1.Offer, meters map[string]meterInfo) (offerMapping, error) {
	if len(offer.Spec.ServicePricings) == 0 {
		return offerMapping{}, invalidOffer("offer %q has no servicePricings snapshot", offer.Name)
	}

	b := &mappingBuilder{
		offer:         offer,
		meters:        meters,
		rateCardKeys:  map[string]string{},
		featureByKey:  map[string]int{},
		pricedMetrics: map[string]string{},
	}
	for _, sp := range offer.Spec.ServicePricings {
		var err error
		switch sp.Spec.ChargeType {
		case billingv1alpha1.ChargeTypeUsage:
			err = b.addUsage(sp)
		case billingv1alpha1.ChargeTypeOneTime, billingv1alpha1.ChargeTypeRecurring:
			err = b.addFlatFee(sp)
		default:
			// ServicePricingSpec.chargeType is enum validated, so this is
			// only reachable for a snapshot written by a newer API version.
			err = invalidOffer("service pricing %q: unsupported charge type %q", sp.Name, sp.Spec.ChargeType)
		}
		if err != nil {
			return offerMapping{}, err
		}
	}

	name := offer.Annotations[billingv1alpha1.DisplayNameAnnotation]
	if name == "" {
		name = offer.Name
	}
	return offerMapping{
		Plan: openmeter.DesiredPlan{
			Key:         planKeyForOffer(offer),
			Name:        name,
			Description: offer.Name,
			Currency:    "USD",
			Metadata:    map[string]string{metadataOfferName: offer.Name},
			Phases: []om.PlanPhase{{
				Key:       planPhaseKey,
				Name:      planPhaseName,
				RateCards: b.rateCards,
			}},
		},
		Features:   b.features,
		MeterSlugs: b.meterSlugs,
	}, nil
}

// mappingBuilder accumulates rate cards and features across service pricings.
type mappingBuilder struct {
	offer  *billingv1alpha1.Offer
	meters map[string]meterInfo

	rateCards []om.RateCard
	// rateCardKeys maps each used rate card key to the pricing that
	// produced it. OpenMeter rejects duplicate keys within a phase.
	rateCardKeys map[string]string

	features     []openmeter.DesiredFeature
	featureByKey map[string]int
	meterSlugs   []string

	// pricedMetrics maps each metric to the usage pricing that bills it.
	pricedMetrics map[string]string
}

func (b *mappingBuilder) claimRateCardKey(key, pricing string) error {
	if prev, dup := b.rateCardKeys[key]; dup {
		return invalidOffer("offer %q: service pricings %q and %q map to the same OpenMeter rate card key %q",
			b.offer.Name, prev, pricing, key)
	}
	b.rateCardKeys[key] = pricing
	return nil
}

func (b *mappingBuilder) addFeature(f openmeter.DesiredFeature) {
	if _, seen := b.featureByKey[f.Key]; seen {
		return
	}
	b.featureByKey[f.Key] = len(b.features)
	b.features = append(b.features, f)
	if !slices.Contains(b.meterSlugs, f.MeterSlug) {
		b.meterSlugs = append(b.meterSlugs, f.MeterSlug)
	}
}

// addUsage maps a Usage pricing to one usage-based rate card per rate, each
// bound to a feature that selects exactly the usage that rate prices.
//
// PricingRate semantics: a rate with Match prices usage whose dimension
// equals the value; the unmatched rate is the catch-all for everything the
// matched rates do not cover. Features are evaluated independently by
// OpenMeter, so the catch-all must explicitly exclude the matched values
// ($nin) — an unfiltered catch-all would bill matched usage twice. For the
// same reason the matched rates must partition one dimension: matches on two
// different dimensions overlap (an event can match both) and are rejected,
// as are duplicate values and more than one catch-all. A metric may be
// priced by only one Usage pricing per Offer, again to rule out overlap.
func (b *mappingBuilder) addUsage(sp billingv1alpha1.ServicePricingSnapshot) error {
	meter, ok := b.meters[sp.Spec.Metric]
	if !ok {
		return fmt.Errorf("service pricing %q: metric %q: %w", sp.Name, sp.Spec.Metric, errMeterNotFound)
	}
	if prev, dup := b.pricedMetrics[sp.Spec.Metric]; dup {
		return invalidOffer("offer %q: service pricings %q and %q both price metric %q; usage would be billed twice",
			b.offer.Name, prev, sp.Name, sp.Spec.Metric)
	}
	b.pricedMetrics[sp.Spec.Metric] = sp.Name
	if len(sp.Spec.Rates) == 0 {
		return invalidOffer("service pricing %q: usage pricing has no rates", sp.Name)
	}

	// Validate the rate set and collect the matched values.
	var dimension string
	var matched []string
	catchAll := -1
	for i, rate := range sp.Spec.Rates {
		if rate.Match == nil {
			if catchAll >= 0 {
				return invalidOffer("service pricing %q: rates %d and %d are both unmatched; only one catch-all rate is allowed",
					sp.Name, catchAll, i)
			}
			catchAll = i
			continue
		}
		switch {
		case dimension == "":
			dimension = rate.Match.Dimension
		case dimension != rate.Match.Dimension:
			return invalidOffer("service pricing %q: rates match on dimensions %q and %q; matches must share one dimension so rates do not overlap",
				sp.Name, dimension, rate.Match.Dimension)
		}
		if slices.Contains(matched, rate.Match.Value) {
			return invalidOffer("service pricing %q: dimension %q value %q is matched by more than one rate",
				sp.Name, dimension, rate.Match.Value)
		}
		matched = append(matched, rate.Match.Value)
	}
	if dimension != "" && dimension != billingv1alpha1.SystemDimensionProjectName &&
		!slices.Contains(meter.Dimensions, dimension) {
		return invalidOffer("service pricing %q: dimension %q is not declared on meter %q (dimensions: %v)",
			sp.Name, dimension, meter.Name, meter.Dimensions)
	}

	for _, rate := range sp.Spec.Rates {
		var filters map[string]om.FilterString
		var label string
		switch {
		case rate.Match != nil:
			value := rate.Match.Value
			filters = map[string]om.FilterString{dimension: {Eq: &value}}
			label = fmt.Sprintf("%s=%s", dimension, value)
		case len(matched) > 0:
			others := slices.Clone(matched)
			slices.Sort(others)
			filters = map[string]om.FilterString{dimension: {Nin: &others}}
			label = fmt.Sprintf("%s not in (%s)", dimension, strings.Join(others, ", "))
		}

		feature, err := usageFeature(meter, filters, label)
		if err != nil {
			return err
		}
		b.addFeature(feature)

		price, err := usagePrice(sp.Name, rate)
		if err != nil {
			return err
		}
		name := sp.Spec.DisplayName
		if name == "" {
			name = sp.Name
		}
		if label != "" {
			name = fmt.Sprintf("%s (%s)", name, label)
		}
		featureKey := feature.Key
		var card om.RateCard
		if err := card.FromRateCardUsageBased(om.RateCardUsageBased{
			Type:           om.RateCardUsageBasedTypeUsageBased,
			Key:            feature.Key,
			Name:           name,
			FeatureKey:     &featureKey,
			BillingCadence: usageBillingPeriod,
			Price:          &price,
			Metadata: &om.Metadata{
				metadataServiceRef:     sp.Spec.ServiceRef,
				metadataServicePricing: sp.Name,
				metadataPricingUnit:    sp.Spec.PricingUnit,
			},
		}); err != nil {
			return fmt.Errorf("service pricing %q: encode rate card: %w", sp.Name, err)
		}
		if err := b.claimRateCardKey(feature.Key, sp.Name); err != nil {
			return err
		}
		b.rateCards = append(b.rateCards, card)
	}
	return nil
}

// usagePrice maps a PricingRate to an OpenMeter usage price.
//
// Tier bounds: Milo documents upTo as an exclusive upper bound, while
// OpenMeter's upToAmount is inclusive ("up to and including"). For the
// continuous quantities Milo meters the two agree everywhere except the
// single boundary point, and the Amberflo provider (startAfterUnit = upTo)
// makes the same inclusive choice, so the bound is passed through unchanged.
func usagePrice(pricing string, rate billingv1alpha1.PricingRate) (om.RateCardUsageBasedPrice, error) {
	var price om.RateCardUsageBasedPrice
	switch {
	case rate.Flat != "" && len(rate.Tiered) > 0:
		return price, invalidOffer("service pricing %q: a rate sets both flat and tiered", pricing)
	case rate.Flat != "":
		err := price.FromUnitPriceWithCommitments(om.UnitPriceWithCommitments{
			Type:   om.UnitPriceWithCommitmentsTypeUnit,
			Amount: om.Numeric(rate.Flat),
		})
		return price, err
	case len(rate.Tiered) > 0:
		tiers := make([]om.PriceTier, 0, len(rate.Tiered))
		for i, band := range rate.Tiered {
			tier := om.PriceTier{UnitPrice: &om.UnitPrice{
				Type:   om.UnitPriceTypeUnit,
				Amount: om.Numeric(band.Rate),
			}}
			if band.UpTo != "" {
				upTo := om.Numeric(band.UpTo)
				tier.UpToAmount = &upTo
			} else if i != len(rate.Tiered)-1 {
				return price, invalidOffer("service pricing %q: only the last tier may omit upTo", pricing)
			}
			tiers = append(tiers, tier)
		}
		err := price.FromTieredPriceWithCommitments(om.TieredPriceWithCommitments{
			Type:  om.TieredPriceWithCommitmentsTypeTiered,
			Mode:  om.TieredPriceModeGraduated,
			Tiers: tiers,
		})
		return price, err
	default:
		return price, invalidOffer("service pricing %q: a rate must set flat or tiered", pricing)
	}
}

// addFlatFee maps a OneTime or Recurring pricing to a flat-fee rate card.
// OneTime cards carry no cadence, so OpenMeter charges them once when the
// subscription starts (BillingAccountActivation is the only trigger).
func (b *mappingBuilder) addFlatFee(sp billingv1alpha1.ServicePricingSnapshot) error {
	if sp.Spec.Amount == "" {
		return invalidOffer("service pricing %q: %s pricing has no amount", sp.Name, sp.Spec.ChargeType)
	}
	key := boundedKey(sanitizeKeyPart(sp.Name), "flat_fee", sp.Name)
	name := sp.Spec.DisplayName
	if name == "" {
		name = sp.Name
	}
	meta := om.Metadata{
		metadataServiceRef:     sp.Spec.ServiceRef,
		metadataServicePricing: sp.Name,
	}
	card := om.RateCardFlatFee{
		Type: om.RateCardFlatFeeTypeFlatFee,
		Key:  key,
		Name: name,
		Price: &om.FlatPriceWithPaymentTerm{
			Type:   om.FlatPriceWithPaymentTermTypeFlat,
			Amount: om.Numeric(sp.Spec.Amount),
		},
		Metadata: &meta,
	}
	switch sp.Spec.ChargeType {
	case billingv1alpha1.ChargeTypeRecurring:
		if sp.Spec.Interval != "" && sp.Spec.Interval != billingv1alpha1.ChargeIntervalMonthly {
			return invalidOffer("service pricing %q: unsupported recurring interval %q", sp.Name, sp.Spec.Interval)
		}
		cadence := usageBillingPeriod
		card.BillingCadence = &cadence
	case billingv1alpha1.ChargeTypeOneTime:
		if sp.Spec.Trigger != "" {
			meta[metadataTrigger] = string(sp.Spec.Trigger)
		}
	}

	var wrapper om.RateCard
	if err := wrapper.FromRateCardFlatFee(card); err != nil {
		return fmt.Errorf("service pricing %q: encode rate card: %w", sp.Name, err)
	}
	if err := b.claimRateCardKey(key, sp.Name); err != nil {
		return err
	}
	b.rateCards = append(b.rateCards, wrapper)
	return nil
}

// usageFeature builds the feature for meter restricted by filters (nil for
// all usage). Features are shared across Offers and immutable in OpenMeter,
// so the key is derived from the full definition — meter slug plus filters —
// via a hash suffix: the same definition always yields the same key, and two
// different definitions (e.g. "us-east" vs "us.east", or catch-alls excluding
// different value sets) never share one.
func usageFeature(meter meterInfo, filters map[string]om.FilterString, label string) (openmeter.DesiredFeature, error) {
	identity, err := json.Marshal(struct {
		Meter   string                     `json:"meter"`
		Filters map[string]om.FilterString `json:"filters,omitempty"`
	}{meter.Slug, filters})
	if err != nil {
		return openmeter.DesiredFeature{}, fmt.Errorf("encode feature identity: %w", err)
	}

	readable := meter.Slug
	name := meter.Name
	switch {
	case len(filters) == 0:
		readable += "_all"
	default:
		for dim, f := range filters {
			if f.Eq != nil {
				readable += "_" + sanitizeKeyPart(dim) + "_" + sanitizeKeyPart(*f.Eq)
			} else {
				readable += "_" + sanitizeKeyPart(dim) + "_other"
			}
		}
		name = fmt.Sprintf("%s [%s]", meter.Name, label)
	}

	return openmeter.DesiredFeature{
		Key:                         boundedKey(readable, "feature", string(identity)),
		Name:                        name,
		MeterSlug:                   meter.Slug,
		AdvancedMeterGroupByFilters: filters,
		Metadata:                    map[string]string{metadataMeterName: meter.Name},
	}, nil
}

// boundedKey returns a valid OpenMeter key: readable (already sanitized),
// suffixed with a short hash of identity, and truncated to fit the 64
// character limit. fallback replaces an empty readable part.
func boundedKey(readable, fallback, identity string) string {
	sum := sha256.Sum256([]byte(identity))
	suffix := hex.EncodeToString(sum[:])[:keyHashLen]
	if readable == "" {
		readable = fallback
	}
	maxPrefix := openMeterKeyMaxLen - len(suffix) - 1
	if len(readable) > maxPrefix {
		readable = strings.TrimRight(readable[:maxPrefix], "_")
	}
	return readable + "_" + suffix
}

// sanitizeKeyPart maps an arbitrary dimension or value string (which may
// carry dots, spaces, hyphens, or mixed case) onto a single OpenMeter-key
// safe segment. OpenMeter keys must match `^[a-z0-9]+(?:_[a-z0-9]+)*$`, so
// every run of characters outside [a-z0-9] collapses to one underscore,
// the input is lowercased, and surrounding underscore runs are dropped. The
// function never emits leading/trailing underscores, so joining sanitized
// segments with "_" always yields a valid key. It is not injective; keys
// that must be unique append a hash (see boundedKey).
func sanitizeKeyPart(s string) string {
	var b strings.Builder
	pendingUnderscore := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if pendingUnderscore && b.Len() > 0 {
				b.WriteByte('_')
			}
			pendingUnderscore = false
			b.WriteRune(r)
		} else {
			pendingUnderscore = true
		}
	}
	return b.String()
}
