// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package repository provides DynamoDB implementation of UsageRepository.
// Post-refactor: no TENANT# partition prefix — single org per deployment.
package repository

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"

	"github.com/aiplat/observability/internal/telemetry"
	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// DynamoDBUsageRepository implements UsageRepository for DynamoDB.
// Schema change: pk = "USAGE" (no org prefix), sk = "TS#<timestamp>#<request_id>"
type DynamoDBUsageRepository struct {
	client *dynamodb.Client
	table  string
}

// NewDynamoDBUsageRepository creates a new DynamoDB-backed usage repository.
func NewDynamoDBUsageRepository(ctx context.Context, cfg Config) (*DynamoDBUsageRepository, error) {
	awsCfg, err := awscfg.LoadDefaultConfig(ctx, awscfg.WithRegion(cfg.DynamoDBRegion))
	if err != nil {
		// Log detailed error internally
		log.Printf("ERROR: Failed to load AWS config: %v", err)
		// Return generic error to client
		return nil, fmt.Errorf("database configuration failed")
	}

	return &DynamoDBUsageRepository{
		client: dynamodb.NewFromConfig(awsCfg),
		table:  cfg.DynamoDBTable,
	}, nil
}

// RecordUsage persists a usage record to DynamoDB.
// Post-refactor: no org in partition key — single deployment scope.
func (r *DynamoDBUsageRepository) RecordUsage(ctx context.Context, record *UsageRecord) error {
	item := r.buildItem(record)

	// Idempotency: pk+sk already embeds ts+request_id
	_, err := r.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(r.table),
		Item:                item,
		ConditionExpression: aws.String("attribute_not_exists(sk)"),
	})

	if err != nil {
		var ccf *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &ccf) {
			// Already exists - idempotent success
			return nil
		}
		// Log detailed error internally
		log.Printf("ERROR: Failed to write usage record to DynamoDB: %v", err)
		// Return generic error to client
		return fmt.Errorf("database operation failed")
	}

	return nil
}

// QueryUsage retrieves usage records matching the filter.
// Post-refactor: queries by global partition, no org prefix.
func (r *DynamoDBUsageRepository) QueryUsage(ctx context.Context, filter UsageFilter) ([]telemetry.Record, error) {
	pk := "USAGE"
	skFrom := "TS#" + filter.StartTime.Format("2006-01-02T15:04:05Z07:00")
	skTo := "TS#" + filter.EndTime.Format("2006-01-02T15:04:05Z07:00") + "~"

	var records []telemetry.Record
	var lek map[string]ddbtypes.AttributeValue

	for {
		q := &dynamodb.QueryInput{
			TableName:              aws.String(r.table),
			KeyConditionExpression: aws.String("pk = :pk AND sk BETWEEN :a AND :b"),
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":pk": &ddbtypes.AttributeValueMemberS{Value: pk},
				":a":  &ddbtypes.AttributeValueMemberS{Value: skFrom},
				":b":  &ddbtypes.AttributeValueMemberS{Value: skTo},
			},
			ExclusiveStartKey: lek,
		}

		result, err := r.client.Query(ctx, q)
		if err != nil {
			// Log detailed error internally
			log.Printf("ERROR: Failed to query usage records from DynamoDB: %v", err)
			// Return generic error to client
			return nil, fmt.Errorf("database operation failed")
		}

		for _, item := range result.Items {
			rec := r.itemToRecord(item)

			// Apply filters (team, app, feature, model, provider)
			if filter.Team != "" && rec.Team != filter.Team {
				continue
			}
			if filter.App != "" && rec.App != filter.App {
				continue
			}
			if filter.Feature != "" && rec.Feature != filter.Feature {
				continue
			}
			if filter.Model != "" && rec.Model != filter.Model {
				continue
			}
			if filter.Provider != "" && rec.Provider != filter.Provider {
				continue
			}

			records = append(records, rec)
		}

		if result.LastEvaluatedKey == nil {
			break
		}
		lek = result.LastEvaluatedKey
	}

	return records, nil
}

// buildItem constructs a DynamoDB item from a UsageRecord.
// Post-refactor: pk = "USAGE" (no org), sk = "TS#<ts>#<request_id>"
func (r *DynamoDBUsageRepository) buildItem(record *UsageRecord) map[string]ddbtypes.AttributeValue {
	app := record.App
	if app == "" {
		app = "none"
	}
	feat := record.Feature
	if feat == "" {
		feat = "none"
	}
	team := record.Team
	if team == "" {
		team = "default"
	}
	status := record.Status
	if status == "" {
		status = "success"
	}

	ts := record.Timestamp.Format("2006-01-02T15:04:05Z07:00")

	item := map[string]ddbtypes.AttributeValue{
		"pk":                       s("USAGE"),
		"sk":                       s(fmt.Sprintf("TS#%s#%s", ts, record.RequestID)),
		"gsi1pk":                   s(fmt.Sprintf("APP#%s", app)),
		"request_id":               s(record.RequestID),
		"app_tag":                  s(app),
		"team":                     s(team),
		"feature":                  s(feat),
		"provider":                 s(record.Provider),
		"model":                    s(record.Model),
		"requested_cost_usd":       n(strconv.FormatFloat(record.RequestedCostUSD, 'f', -1, 64)),
		"tokens_in":                n(strconv.Itoa(record.TokensIn)),
		"tokens_out":               n(strconv.Itoa(record.TokensOut)),
		"estimated_cost_usd":       n(strconv.FormatFloat(record.Cost, 'f', -1, 64)),
		"saved_usd":                n(strconv.FormatFloat(record.Saved, 'f', -1, 64)),
		"savings_reason":           s(record.SavingsReason),
		"latency_ms":               n(strconv.Itoa(record.LatencyMs)),
		"cache_hit":                &ddbtypes.AttributeValueMemberBOOL{Value: record.CacheHit},
		"status":                   s(status),
		"credit_usd":               n(strconv.FormatFloat(record.CreditUSD, 'f', -1, 64)),
		"cash_usd":                 n(strconv.FormatFloat(record.CashUSD, 'f', -1, 64)),
		"saved_verified_usd":       n(strconv.FormatFloat(record.SavedVerified, 'f', -1, 64)),
		"saved_counterfactual_usd": n(strconv.FormatFloat(record.SavedCounterfactual, 'f', -1, 64)),
		"ts":                       s(ts),
	}

	if record.RequestedModel != "" {
		item["requested_model"] = s(record.RequestedModel)
	}
	if record.PaidFrom != "" {
		item["paid_from"] = s(record.PaidFrom)
	}
	if record.SavingsClass != "" {
		item["savings_class"] = s(record.SavingsClass)
	}
	if record.PriceSource != "" {
		item["price_source"] = s(record.PriceSource)
	}
	if record.SwapClass != "" {
		item["swap_class"] = s(record.SwapClass)
	}
	if record.ServedModelID != "" {
		item["served_model_id"] = s(record.ServedModelID)
	}
	if record.Canary {
		item["canary"] = &ddbtypes.AttributeValueMemberBOOL{Value: true}
		if record.CanaryRoute != "" {
			item["canary_route"] = s(record.CanaryRoute)
		}
	}
	if record.Reason != "" {
		item["reason"] = s(record.Reason)
	}
	if record.Detail != "" {
		item["detail"] = s(record.Detail)
	}
	if record.Category != "" {
		item["category"] = s(record.Category)
	}
	if record.Upstream != "" {
		item["upstream"] = s(record.Upstream)
	}
	if record.Mode != "" {
		item["mode"] = s(record.Mode)
	}
	item["sli_eligible"] = &ddbtypes.AttributeValueMemberBOOL{Value: record.SLIEligible}

	return item
}

// itemToRecord converts a DynamoDB item to a telemetry.Record.
func (r *DynamoDBUsageRepository) itemToRecord(item map[string]ddbtypes.AttributeValue) telemetry.Record {
	return telemetry.Record{
		Provider:            str(item["provider"]),
		Upstream:            str(item["upstream"]),
		Model:               str(item["model"]),
		RequestedModel:      str(item["requested_model"]),
		RequestedCostUSD:    num(item["requested_cost_usd"]),
		App:                 str(item["app_tag"]),
		Team:                str(item["team"]),
		Feature:             str(item["feature"]),
		TokensIn:            int(num(item["tokens_in"])),
		TokensOut:           int(num(item["tokens_out"])),
		Cost:                num(item["estimated_cost_usd"]),
		Saved:               num(item["saved_usd"]),
		CreditUSD:           num(item["credit_usd"]),
		CashUSD:             num(item["cash_usd"]),
		SavedVerified:       num(item["saved_verified_usd"]),
		SavedCounterfactual: num(item["saved_counterfactual_usd"]),
		PriceSource:         str(item["price_source"]),
		SwapClass:           str(item["swap_class"]),
		ServedModelID:       str(item["served_model_id"]),
		Canary:              boolOf(item["canary"]),
		CanaryRoute:         str(item["canary_route"]),
		Reason:              str(item["savings_reason"]),
		Latency:             int(num(item["latency_ms"])),
		CacheHit:            boolOf(item["cache_hit"]),
		Status:              str(item["status"]),
		FailReason:          str(item["reason"]),
		Detail:              str(item["detail"]),
		Category:            str(item["category"]),
		SLIEligible:         boolOf(item["sli_eligible"]),
		Mode:                modeOf(item["mode"]),
		TS:                  str(item["ts"]),
	}
}

// Helper functions for DynamoDB attribute conversion
func s(v string) *ddbtypes.AttributeValueMemberS {
	return &ddbtypes.AttributeValueMemberS{Value: v}
}

func n(v string) *ddbtypes.AttributeValueMemberN {
	return &ddbtypes.AttributeValueMemberN{Value: v}
}

func num(av ddbtypes.AttributeValue) float64 {
	if n, ok := av.(*ddbtypes.AttributeValueMemberN); ok {
		f, _ := strconv.ParseFloat(n.Value, 64)
		return f
	}
	return 0
}

func str(av ddbtypes.AttributeValue) string {
	if s, ok := av.(*ddbtypes.AttributeValueMemberS); ok {
		return s.Value
	}
	return ""
}

func boolOf(av ddbtypes.AttributeValue) bool {
	if b, ok := av.(*ddbtypes.AttributeValueMemberBOOL); ok {
		return b.Value
	}
	return false
}

func modeOf(av ddbtypes.AttributeValue) string {
	if s, ok := av.(*ddbtypes.AttributeValueMemberS); ok && s.Value != "" {
		return s.Value
	}
	return "sync"
}
