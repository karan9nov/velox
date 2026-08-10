package credit

import (
	"fmt"
	"strings"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// GrantValidationError captures structured validation failures for credit grants.
// The handler maps these to 4xx with a stable error code so clients can retry
// with corrected payloads. Keeping validation in its own file (vs inline in
// service.go) makes the rules testable without a DB and keeps service.go focused
// on transaction wiring and clock binding.
type GrantValidationError struct {
	Field   string
	Code    string
	Message string
}

func (e GrantValidationError) Error() string {
	return fmt.Sprintf("%s: %s (%s)", e.Field, e.Message, e.Code)
}

// GrantValidationResult aggregates all failures so the API returns every
// problem at once (better DX than fail-fast on first error). The service
// short-circuits on empty customer_id because downstream checks assume it.
type GrantValidationResult struct {
	Errors []GrantValidationError
}

func (r GrantValidationResult) IsValid() bool {
	return len(r.Errors) == 0
}

func (r GrantValidationResult) ToErr() error {
	if r.IsValid() {
		return nil
	}
	// Return first error as errs.InvalidArgument (handler maps it to 400);
	// full list is logged and returned in response body via handler's
	// validation error envelope. Keeping first-error return here preserves
	// existing service.go error handling that expects a single error.
	msgs := make([]string, 0, len(r.Errors))
	for _, e := range r.Errors {
		msgs = append(msgs, fmt.Sprintf("%s: %s", e.Field, e.Message))
	}
	return &errs.InvalidArgumentError{
		Field:   r.Errors[0].Field,
		Message: strings.Join(msgs, "; "),
	}
}

// ValidateGrantInput checks all user-supplied fields for a credit grant.
//
// Rules (ADR-078 + existing store invariants + new expiry safety):
// - customer_id required, non-empty, trimmed
// - amount_cents > 0, <= 10_000_000_00 ($10M cap to catch unit confusion)
// - description required, 3-500 chars after trim
// - expires_at, if set, must be future (> now + 5m) and <= 2y future
// - grant_kind: empty or promotional or commit, but commit rejected here
//   (only invoice finalize path may create commit-kind credits — see
//   GrantCommitForInvoiceTx); promotional requires amount <= $10k to catch
//   accidental large promo grants
// - source fields: if any proration field set, all four must be set
// - source_credit_note_id: if set, must be non-empty trimmed and invoice_id empty
// - invoice_id and source_credit_note_id mutually exclusive
// - at: if set, must not be future beyond 1h (clock skew) and not before tenant creation (not checked here, DB layer)
// - amount_cents vs description: promotional $0 not allowed, but $0 paid is allowed? No, $0 never allowed.
func ValidateGrantInput(input GrantInput, now time.Time) GrantValidationResult {
	var result GrantValidationResult
	add := func(field, code, msg string) {
		result.Errors = append(result.Errors, GrantValidationError{
			Field:   field,
			Code:    code,
			Message: msg,
		})
	}

	// customer_id
	if strings.TrimSpace(input.CustomerID) == "" {
		add("customer_id", "required", "customer_id is required")
		// Short-circuit: other checks assume customer_id present for logging
		return result
	}

	// amount_cents
	if input.AmountCents <= 0 {
		add("amount_cents", "positive_required", "amount_cents must be > 0")
	} else if input.AmountCents > 10_000_000_00 {
		add("amount_cents", "too_large", "amount_cents exceeds $10M cap — check unit (cents vs dollars)")
	}

	// description
	desc := strings.TrimSpace(input.Description)
	if desc == "" {
		add("description", "required", "description is required")
	} else if len(desc) < 3 {
		add("description", "too_short", "description must be >= 3 chars")
	} else if len(desc) > 500 {
		add("description", "too_long", "description must be <= 500 chars")
	}

	// expires_at
	if input.ExpiresAt != nil {
		exp := *input.ExpiresAt
		if exp.Before(now.Add(5 * time.Minute)) {
			add("expires_at", "must_be_future", "expires_at must be at least 5m in future")
		}
		if exp.After(now.Add(2 * 365 * 24 * time.Hour)) {
			add("expires_at", "too_far_future", "expires_at must be <= 2y future")
		}
		// Expiry must not be before At (if At set and is the earning time)
		if !input.At.IsZero() && exp.Before(input.At) {
			add("expires_at", "before_earned_at", "expires_at cannot be before earned-at time")
		}
	}

	// grant_kind
	switch input.GrantKind {
	case "", domain.GrantKindPromotional, domain.GrantKindCommit:
		// empty = unclassified, allowed
	default:
		add("grant_kind", "invalid", fmt.Sprintf("grant_kind %q invalid — valid: promotional, commit", input.GrantKind))
	}
	if input.GrantKind == domain.GrantKindCommit {
		add("grant_kind", "reserved", "commit kind is reserved for invoice finalize path — use GrantCommitForInvoiceTx")
	}
	if input.GrantKind == domain.GrantKindPromotional && input.AmountCents > 1_000_000 {
		add("grant_kind", "promo_too_large", "promotional grant > $10k requires manual approval — split or use paid kind")
	}

	// Source fields: proration credits require full tuple
	hasProration := input.SourceSubscriptionID != "" || input.SourceSubscriptionItemID != "" || input.SourcePlanChangedAt != nil || input.SourceChangeType != ""
	if hasProration {
		if input.SourceSubscriptionID == "" {
			add("source_subscription_id", "required_with_proration", "source_subscription_id required when proration fields present")
		}
		if input.SourceSubscriptionItemID == "" {
			add("source_subscription_item_id", "required_with_proration", "source_subscription_item_id required when proration fields present")
		}
		if input.SourcePlanChangedAt == nil {
			add("source_plan_changed_at", "required_with_proration", "source_plan_changed_at required when proration fields present")
		}
		if input.SourceChangeType == "" {
			add("source_change_type", "required_with_proration", "source_change_type required when proration fields present")
		}
	}

	// source_credit_note_id vs invoice_id mutual exclusion
	if input.SourceCreditNoteID != "" && input.InvoiceID != "" {
		add("source_credit_note_id", "mutually_exclusive", "source_credit_note_id and invoice_id are mutually exclusive")
	}
	if input.SourceCreditNoteID != "" && strings.TrimSpace(input.SourceCreditNoteID) == "" {
		add("source_credit_note_id", "invalid", "source_credit_note_id cannot be whitespace only")
	}

	// At (simulated earned-at)
	if !input.At.IsZero() {
		if input.At.After(now.Add(1 * time.Hour)) {
			add("at", "future_too_far", "earned-at time cannot be >1h in future (clock skew)")
		}
		// Allow At up to 10y past for catchup/backfill, but not more
		if input.At.Before(now.Add(-10 * 365 * 24 * time.Hour)) {
			add("at", "too_far_past", "earned-at time cannot be >10y in past")
		}
	}

	return result
}

// CalculateEffectiveExpiry computes the effective expiry for a grant, factoring
// in tenant-level defaults, grant-kind rules, and the new safety cap that
// promotional credits cannot outlive 90d (ADR-078 follow-up). This is called
// both at grant creation and at read time for display — so it must be pure
// and idempotent, no DB calls, no side effects. The store still persists the
// raw expires_at; effective is computed on read via a view or service helper.
//
// Rules:
// - If raw expiry nil, returns nil (never expires) except promotional which
//   caps at now+90d
// - If raw expiry set, promotional caps to min(raw, now+90d)
// - Commit grants: expiry must be nil (commit credits are perpetual until
//   consumed); if set, caller should have been rejected by ValidateGrantInput,
//   but we defensively return nil to avoid accidental expiry of commit funds.
func CalculateEffectiveExpiry(
	kind domain.GrantKind,
	rawExpiry *time.Time,
	now time.Time,
) *time.Time {
	if kind == domain.GrantKindCommit {
		return nil
	}
	if rawExpiry == nil {
		if kind == domain.GrantKindPromotional {
			t := now.Add(90 * 24 * time.Hour)
			return &t
		}
		return nil
	}
	if kind == domain.GrantKindPromotional {
		cap := now.Add(90 * 24 * time.Hour)
		if rawExpiry.After(cap) {
			return &cap
		}
	}
	return rawExpiry
}

// BulkGrantInput wraps many GrantInput with shared tenant/customer for atomic
// bulk creation. The service's BulkGrant validates each input and fails the
// whole batch if any is invalid — all-or-nothing preserves ledger consistency
// and avoids partial credit application that downstream invoice finalization
// would need to unwind. Retry safety comes from idempotency keys per grant
// (source_credit_note_id or source proration tuple).
type BulkGrantInput struct {
	TenantID   string
	CustomerID string
	Grants     []GrantInput
}

// ValidateBulkGrant checks bulk-level invariants plus per-grant rules.
func ValidateBulkGrant(input BulkGrantInput, now time.Time) GrantValidationResult {
	var result GrantValidationResult
	add := func(field, code, msg string) {
		result.Errors = append(result.Errors, GrantValidationError{
			Field:   field,
			Code:    code,
			Message: msg,
		})
	}

	if strings.TrimSpace(input.TenantID) == "" {
		add("tenant_id", "required", "tenant_id is required")
	}
	if strings.TrimSpace(input.CustomerID) == "" {
		add("customer_id", "required", "customer_id is required")
	}
	if len(input.Grants) == 0 {
		add("grants", "empty", "at least one grant required")
	}
	if len(input.Grants) > 100 {
		add("grants", "too_many", "bulk grant max 100 entries per request")
	}

	// Per-grant validation + cross-grant uniqueness of idempotency keys
	seenCreditNote := make(map[string]int)
	seenProration := make(map[string]int)
	for i, g := range input.Grants {
		// Ensure grant inherits customer_id from bulk if not set per-entry
		if g.CustomerID == "" {
			g.CustomerID = input.CustomerID
		}
		if g.CustomerID != input.CustomerID {
			add(fmt.Sprintf("grants[%d].customer_id", i), "mismatch", "per-grant customer_id must match bulk customer_id or be empty")
		}
		per := ValidateGrantInput(g, now)
		for _, e := range per.Errors {
			add(fmt.Sprintf("grants[%d].%s", i, e.Field), e.Code, e.Message)
		}
		if g.SourceCreditNoteID != "" {
			if prev, ok := seenCreditNote[g.SourceCreditNoteID]; ok {
				add(fmt.Sprintf("grants[%d].source_credit_note_id", i), "duplicate", fmt.Sprintf("duplicate credit_note_id with grants[%d]", prev))
			}
			seenCreditNote[g.SourceCreditNoteID] = i
		}
		if g.SourceSubscriptionID != "" && g.SourceSubscriptionItemID != "" {
			key := fmt.Sprintf("%s:%s:%s", g.SourceSubscriptionID, g.SourceSubscriptionItemID, g.SourceChangeType)
			if prev, ok := seenProration[key]; ok {
				add(fmt.Sprintf("grants[%d]", i), "duplicate_proration", fmt.Sprintf("duplicate proration tuple with grants[%d]", prev))
			}
			seenProration[key] = i
		}
		// Sum cap: bulk total must not exceed $10M either
	}

	// Total amount cap
	var total int64
	for _, g := range input.Grants {
		total += g.AmountCents
	}
	if total > 10_000_000_00 {
		add("grants", "total_too_large", "bulk total exceeds $10M cap")
	}

	return result
}
