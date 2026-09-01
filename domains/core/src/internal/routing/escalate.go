// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package routing

import (
	"context"

	"github.com/aiplat/core/internal/ports"
)

// Escalation outcomes, recorded in the Usage_Record (Req 14.4–14.6).
const (
	EscalationNone        = ""
	EscalationUnavailable = "escalation_unavailable"
	EscalationFailed      = "escalation_failed"
	ValidationFailed      = "validation_failed"
)

// Attempt is one provider call, with its cost.
type Attempt struct {
	Model   string
	Result  ports.Result
	CostUSD Money
}

// EscalationOutcome is the complete result, with ALL attempts.
//
// Keeping both attempts is not a logging detail: without the cost of both, the
// savings declared in the ledger is false — and the auditable ledger is the product.
type EscalationOutcome struct {
	Attempts  []Attempt
	Escalated bool
	Reason    string // reason for the invalidity that triggered the escalation
	Outcome   string // "" | validation_failed | escalation_unavailable | escalation_failed
	TotalCost Money
}

// Final returns the attempt whose response goes to the client: the last one executed.
func (o EscalationOutcome) Final() Attempt {
	if len(o.Attempts) == 0 {
		return Attempt{}
	}
	return o.Attempts[len(o.Attempts)-1]
}

// CostFn computes the realized cost of an attempt. Injected so the domain does not
// need to know Config or the price table of the impure layer.
type CostFn func(model string, res ports.Result) Money

// NextTier returns the next candidate of a tier HIGHER than the current model, or
// (Candidate{}, false) when none exists. Injected to keep Escalate pure.
type NextTierFn func(current string) (Candidate, bool)

// Escalate performs the call and, if the response is structurally invalid and
// escalation is enabled, retries ONCE on a higher-tier model.
//
// It takes ports.Provider — that is the reason the port is not optional. Without it,
// this behavior would only be testable with the cloud, and this is precisely where
// the cost doubles.
func Escalate(
	ctx context.Context,
	p ports.Provider,
	in ports.InvokeInput,
	shape RequestShape,
	cost CostFn,
	next NextTierFn,
	enabled bool,
	expectJSON bool,
) (EscalationOutcome, error) {
	var out EscalationOutcome

	res, err := p.Invoke(ctx, in)
	if err != nil {
		return out, err
	}
	first := Attempt{Model: in.Model, Result: res, CostUSD: cost(in.Model, res)}
	out.Attempts = append(out.Attempts, first)
	out.TotalCost = first.CostUSD

	v := Validate(res, shape, expectJSON)
	if v.Valid {
		return out, nil
	}
	out.Reason = v.Reason

	// Disabled: return what came back and RECORD it. Staying silent here would hide
	// the failure rate of economy mode, which is the data that justifies turning it
	// on or not.
	if !enabled {
		out.Outcome = ValidationFailed
		return out, nil
	}

	up, ok := next(in.Model)
	if !ok {
		out.Outcome = EscalationUnavailable
		return out, nil
	}

	// At most ONE escalation attempt (Req 14.3): without a ceiling, a chain of bad
	// models would multiply cost and latency with no guarantee of success.
	in2 := in
	in2.Model = up.Model
	res2, err2 := p.Invoke(ctx, in2)
	if err2 != nil {
		// Transport failure on the second call: return the first response, which at
		// least exists, and do not charge for the attempt that produced nothing.
		out.Outcome = EscalationFailed
		return out, nil
	}
	second := Attempt{Model: up.Model, Result: res2, CostUSD: cost(up.Model, res2)}
	out.Attempts = append(out.Attempts, second)
	out.TotalCost += second.CostUSD
	out.Escalated = true

	if v2 := Validate(res2, shape, expectJSON); !v2.Valid {
		out.Outcome = EscalationFailed
		out.Reason = v2.Reason
	}
	return out, nil
}

// NetSavings is the request's NET savings, with a floor of zero (Req 14.8, 14.9).
//
// Subtracting the cost of ALL attempts is what keeps the ledger from lying: a feature
// in economy mode that fails 30% of the time pays for two models and may have spent
// MORE than the requested model. Reporting the gross difference there would declare
// savings where there was a loss.
func NetSavings(requestedCost Money, totalAttemptCost Money) (saved Money, floored bool) {
	d := requestedCost - totalAttemptCost
	if d <= 0 {
		return 0, true
	}
	return d, false
}

// TierRank orders the known tiers from weakest to strongest. An unknown tier sits in
// the middle: it neither promotes nor prevents escalation.
func TierRank(tier string) int {
	switch tier {
	case "fast":
		return 1
	case "balanced":
		return 2
	case "frontier":
		return 3
	}
	return 2
}

// NextTierFrom builds a NextTierFn from the eligible candidates: the CHEAPEST among
// those of a tier strictly higher than the current one.
//
// Escalating to the cheapest of the tier above, rather than to the top, keeps the
// cost of the retry proportional to the problem.
func NextTierFrom(cands []Candidate, current string, costOf func(Candidate) (Money, bool)) NextTierFn {
	return func(_ string) (Candidate, bool) {
		curRank := 0
		for _, c := range cands {
			if c.Model == current {
				curRank = TierRank(c.Caps.Tier)
			}
		}
		var best *Candidate
		var bestCost Money
		for i := range cands {
			c := cands[i]
			if c.Model == current || TierRank(c.Caps.Tier) <= curRank {
				continue
			}
			cost, ok := costOf(c)
			if !ok {
				continue // an unknown price does not enter the choice
			}
			if best == nil || cost < bestCost {
				best, bestCost = &c, cost
			}
		}
		if best == nil {
			return Candidate{}, false
		}
		return *best, true
	}
}
