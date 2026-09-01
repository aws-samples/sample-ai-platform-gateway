// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Reading the alert FIRING history.
//
// Post-refactor: the record is written by the alert-notifier (same domain) in the
// ALERTLOG partition of the Cost_Store (no org prefix). It stays outside the
// ports.CostStore port on purpose: the port models the Usage_Record (the usage series),
// and cramming a second shape of data into it would mix two contracts with different
// life cycles.
//
// Why this history exists: the cooldown consumes the firing for the day. If webhook
// delivery fails, the alert is lost — and with no record the customer concludes
// everything was fine. The CloudWatch line solves that for us, not for them.
package main

import (
	"context"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// queryAlertLog returns the deployment's alert firings since fromISO, most recent first.
// Post-refactor: no org parameter — single deployment scope.
func queryAlertLog(ctx context.Context, fromISO string, limit int) ([]map[string]interface{}, error) {
	if ddbClient == nil || costTable == "" {
		return []map[string]interface{}{}, nil
	}
	out := make([]map[string]interface{}, 0, limit)
	var lek map[string]ddbtypes.AttributeValue
	for {
		res, err := ddbClient.Query(ctx, &dynamodb.QueryInput{
			TableName:              &costTable,
			KeyConditionExpression: strPtr("pk = :p AND sk BETWEEN :a AND :b"),
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":p": &ddbtypes.AttributeValueMemberS{Value: "ALERTLOG"},
				":a": &ddbtypes.AttributeValueMemberS{Value: "TS#" + fromISO},
				":b": &ddbtypes.AttributeValueMemberS{Value: "TS#9999~"},
			},
			ScanIndexForward:  boolPtr(false), // most recent first
			ExclusiveStartKey: lek,
		})
		if err != nil {
			return nil, err
		}
		for _, it := range res.Items {
			row := map[string]interface{}{}
			for _, k := range []string{"rule", "label", "message", "severity", "metric",
				"comparator", "unit", "window", "host", "ts"} {
				if v, ok := it[k].(*ddbtypes.AttributeValueMemberS); ok && v.Value != "" {
					row[k] = v.Value
				}
			}
			for _, k := range []string{"value", "threshold", "burn_rate", "slo_target",
				"current_pct", "baseline_pct"} {
				if v, ok := it[k].(*ddbtypes.AttributeValueMemberN); ok {
					if f, e := strconv.ParseFloat(v.Value, 64); e == nil {
						row[k] = f
					}
				}
			}
			if v, ok := it["status_code"].(*ddbtypes.AttributeValueMemberN); ok {
				if n, e := strconv.Atoi(v.Value); e == nil {
					row["status_code"] = n
				}
			}
			// delivered is the field that gives the screen its value: an alert that
			// fired and did NOT arrive is the case the customer needs to see to act.
			if v, ok := it["delivered"].(*ddbtypes.AttributeValueMemberBOOL); ok {
				row["delivered"] = v.Value
			}
			out = append(out, row)
			if len(out) >= limit {
				return out, nil
			}
		}
		if res.LastEvaluatedKey == nil {
			return out, nil
		}
		lek = res.LastEvaluatedKey
	}
}

func strPtr(v string) *string { return &v }
func boolPtr(v bool) *bool    { return &v }
