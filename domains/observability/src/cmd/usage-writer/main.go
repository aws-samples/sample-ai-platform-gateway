// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// usage-writer of the Observability domain: consumes Usage_Records from SQS and
// writes them to the Cost_Store (DynamoDB). Asynchronous ingestion, fault tolerant
// (DLQ on the queue).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// hotRetention is how long a Usage_Record stays queryable in the Cost_Store.
// The console never looks back further than 90 days (usage/summary period
// selector tops out there) — 120 gives that a margin without the table growing
// unbounded. Kept as a named const (not a magic number in buildItem) so the
// retention decision reads as a decision, not an accident of arithmetic.
const hotRetention = 120 * 24 * time.Hour

var ddb *dynamodb.Client
var table = os.Getenv("COST_STORE_TABLE")

type usage struct {
	RequestID string `json:"request_id"`
	// Tenant removed - single org per deployment
	Team     string `json:"team"`
	AppTag   string `json:"app_tag"`
	Feature  string `json:"feature"`
	Provider string `json:"provider"`
	Upstream string `json:"upstream"` // real destination (host): openrouter.ai, api.groq.com, bedrock…
	Model    string `json:"model"`
	// RequestedModel is the model the customer asked for (or the org default when not
	// given). When it differs from Model, a swap happened (auto_cheapest/fallback/
	// budget_degrade) and it is the BASELINE of the counterfactual savings.
	// RequestedCostUSD is what that baseline would have cost on the same tokens.
	// Persisting both is what makes every savings line auditable (not only how much
	// was saved, but against what).
	RequestedModel   string  `json:"requested_model"`
	RequestedCostUSD float64 `json:"requested_cost_usd"`
	TokensIn         int     `json:"tokens_in"`
	TokensOut        int     `json:"tokens_out"`
	Cost             float64 `json:"estimated_cost_usd"`
	Saved            float64 `json:"saved_usd"`
	SavingsReason    string  `json:"savings_reason"`
	// Partition of Saved by STRENGTH OF PROOF: verified (cache — same model,
	// observable) vs counterfactual (model served != requested, assumed baseline).
	// SavedVerified + SavedCounterfactual == Saved.
	SavedVerified       float64 `json:"saved_verified_usd"`
	SavedCounterfactual float64 `json:"saved_counterfactual_usd"`
	SavingsClass        string  `json:"savings_class"`
	LatencyMs           int     `json:"latency_ms"`
	CacheHit            bool    `json:"cache_hit"`
	// Status of how the request ended at the gateway: success | error | blocked.
	// Served requests (cache included) are "success"; a provider failure is "error";
	// a policy block (suspended/guardrail/rate/budget/model) is "blocked".
	Status string `json:"status"`
	// Reason: short code for why it failed (rate_limit_exceeded, secret_detected...).
	// Detail: short text only for provider errors (never contains the customer prompt).
	Reason string `json:"reason"`
	Detail string `json:"detail"`
	// Category (FCAPS): config|auth|policy|dependency|platform|capacity|ok.
	// SLIEligible: whether it counts toward the platform reliability SLI (only our own
	// failures).
	Category    string `json:"category"`
	SLIEligible bool   `json:"sli_eligible"`
	// PaidFrom partitions the cost by pocket: credit (provider credit) or cash (real
	// cash out). CreditUSD + CashUSD == Cost. Persisting it is mandatory for the ledger
	// to separate ROUTING savings from credit that was merely burned.
	PaidFrom  string  `json:"paid_from"`
	CreditUSD float64 `json:"credit_usd"`
	CashUSD   float64 `json:"cash_usd"`
	// PriceSource: `list` (the provider's list price) or `contract` (the customer's
	// negotiated price). Persisted per request because a customer with a commitment
	// discount pays less than list: computing from list inflates cost and savings
	// together. Knowing the provenance of each record is what makes the history
	// auditable when the contract is registered later.
	PriceSource string `json:"price_source"`
	// SwapClass says WHAT KIND of substitution happened: "" (served as requested),
	// same_model (different route, same declared model — no quality risk),
	// equivalent (different model, tier not lower) or downgrade (lower tier).
	// Without it, "changed provider" and "changed model" were both `fallback`, so a
	// customer auditing response quality could not tell them apart.
	// ServedModelID is the declared identity of the route that served, kept on the
	// record so a reader does not have to resolve a catalog that may have changed.
	SwapClass     string `json:"swap_class"`
	ServedModelID string `json:"served_model_id"`
	// Canary marks a request routed to a candidate route as part of a declared
	// experiment. Persisting it is what allows the comparison to exclude experiment
	// traffic from the reference side — without the mark, a canary would pollute the
	// very baseline it is being compared against.
	Canary      bool   `json:"canary"`
	CanaryRoute string `json:"canary_route"`
	Ts          string `json:"ts"`
}

func s(v string) *ddbtypes.AttributeValueMemberS { return &ddbtypes.AttributeValueMemberS{Value: v} }
func n(v string) *ddbtypes.AttributeValueMemberN { return &ddbtypes.AttributeValueMemberN{Value: v} }

// expiresAt computes the TTL from the instant OF THE EVENT (u.Ts), not the instant of
// ingestion — same rule as the audit trail (auditcore.RetentionDays): a record
// reprocessed days later from the DLQ must not gain extra retention just because it
// landed late. Falls back to "now" only when Ts fails to parse, so a malformed
// timestamp still gets a sane expiry instead of TTL silently not being set.
func expiresAt(ts string, retention time.Duration) int64 {
	base, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		base = time.Now().UTC()
	}
	return base.Add(retention).Unix()
}

// buildItem assembles the Cost_Store item from a Usage_Record, applying the
// compatibility defaults (app/feature/team/status) and the pk/sk composition that
// guarantees idempotency (sk = TS#<ts>#<request_id>). It is a PURE function (no IO):
// given the same `u`, it produces the same item — which is what lets
// `attribute_not_exists(sk)` deduplicate reprocessing without double counting.
// Post-refactor: pk = "USAGE" (no org), gsi1pk = "APP#<app>" (no org prefix).
// It stays in the shell (it returns SDK types) by design decision: usage-writer is a
// thin shell and does not get a write port (hexagonal-refactor task 17.3, D2).
func buildItem(u usage) map[string]ddbtypes.AttributeValue {
	app := u.AppTag
	if app == "" {
		app = "none"
	}
	feat := u.Feature
	if feat == "" {
		feat = "none"
	}
	team := u.Team
	if team == "" {
		team = "default"
	}
	status := u.Status
	if status == "" {
		status = "success" // compatibility: legacy records are served requests
	}
	item := map[string]ddbtypes.AttributeValue{
		"pk":                       s("USAGE"),
		"sk":                       s("TS#" + u.Ts + "#" + u.RequestID),
		"gsi1pk":                   s("APP#" + app),
		"request_id":               s(u.RequestID),
		"app_tag":                  s(app),
		"team":                     s(team),
		"feature":                  s(feat),
		"provider":                 s(u.Provider),
		"model":                    s(u.Model),
		"requested_cost_usd":       n(strconv.FormatFloat(u.RequestedCostUSD, 'f', -1, 64)),
		"tokens_in":                n(strconv.Itoa(u.TokensIn)),
		"tokens_out":               n(strconv.Itoa(u.TokensOut)),
		"estimated_cost_usd":       n(strconv.FormatFloat(u.Cost, 'f', -1, 64)),
		"saved_usd":                n(strconv.FormatFloat(u.Saved, 'f', -1, 64)),
		"savings_reason":           s(u.SavingsReason),
		"latency_ms":               n(strconv.Itoa(u.LatencyMs)),
		"cache_hit":                &ddbtypes.AttributeValueMemberBOOL{Value: u.CacheHit},
		"status":                   s(status),
		"credit_usd":               n(strconv.FormatFloat(u.CreditUSD, 'f', -1, 64)),
		"cash_usd":                 n(strconv.FormatFloat(u.CashUSD, 'f', -1, 64)),
		"saved_verified_usd":       n(strconv.FormatFloat(u.SavedVerified, 'f', -1, 64)),
		"saved_counterfactual_usd": n(strconv.FormatFloat(u.SavedCounterfactual, 'f', -1, 64)),
		"ts":                       s(u.Ts),
		"expires_at":               n(strconv.FormatInt(expiresAt(u.Ts, hotRetention), 10)),
	}
	if u.RequestedModel != "" {
		item["requested_model"] = s(u.RequestedModel)
	}
	if u.PaidFrom != "" {
		item["paid_from"] = s(u.PaidFrom)
	}
	if u.SavingsClass != "" {
		item["savings_class"] = s(u.SavingsClass)
	}
	if u.PriceSource != "" {
		item["price_source"] = s(u.PriceSource)
	}
	// Both are written only when present. Absence is meaningful and distinct from a
	// blank value: an old record simply predates the dimension, and an undeclared
	// identity must not read as "declared and empty".
	if u.SwapClass != "" {
		item["swap_class"] = s(u.SwapClass)
	}
	if u.ServedModelID != "" {
		item["served_model_id"] = s(u.ServedModelID)
	}
	if u.Canary {
		item["canary"] = &ddbtypes.AttributeValueMemberBOOL{Value: true}
		if u.CanaryRoute != "" {
			item["canary_route"] = s(u.CanaryRoute)
		}
	}
	if u.Reason != "" {
		item["reason"] = s(u.Reason)
	}
	if u.Detail != "" {
		item["detail"] = s(u.Detail)
	}
	if u.Category != "" {
		item["category"] = s(u.Category)
	}
	if u.Upstream != "" {
		item["upstream"] = s(u.Upstream)
	}
	item["sli_eligible"] = &ddbtypes.AttributeValueMemberBOOL{Value: u.SLIEligible}
	return item
}

func handle(ctx context.Context, e events.SQSEvent) error {
	for _, rec := range e.Records {
		var u usage
		if err := json.Unmarshal([]byte(rec.Body), &u); err != nil {
			continue // a malformed record must not stall the batch
		}
		item := buildItem(u)
		// Idempotency: pk+sk already embeds ts+request_id. If the message is
		// reprocessed (retry/DLQ), the item already exists and we do not double count.
		_, err := ddb.PutItem(ctx, &dynamodb.PutItemInput{
			TableName:           &table,
			Item:                item,
			ConditionExpression: awsString("attribute_not_exists(sk)"),
		})
		if err != nil {
			var ccf *ddbtypes.ConditionalCheckFailedException
			if errors.As(err, &ccf) {
				continue // already written before → ignore (idempotent)
			}
			return err // real error → message goes back to the queue / DLQ
		}
	}
	return nil
}

func awsString(s string) *string { return &s }

func main() {
	cfg, _ := config.LoadDefaultConfig(context.TODO())
	ddb = dynamodb.NewFromConfig(cfg)
	lambda.Start(handle)
}
