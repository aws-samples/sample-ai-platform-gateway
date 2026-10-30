// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// usage-reconciler consumes the AgentCore Gateway's vended routing logs (delivered through
// a CloudWatch Logs subscription filter) and records the GATEWAY-SIDE TRUTH for each
// request: which target/model actually served it and how many outbound attempts (gateway
// invocations) it took. One idempotent row per gateway request.
//
// It does NOT flip AIPlat ledger rows: the gateway log carries no caller correlation id
// (MEASURED 2026-10-26), so there is no per-row join — AIPlat rows stay honestly
// reconcile_state="pending" and this parallel GATEWAY fact table is what the ROI/reporting
// reads to see the real served-target mix and the per-attempt invocation count. See
// internal/reconcile for the rationale and the pure parsing.
//
// Lives in the Core domain (not Observability) because it is co-located with the gateway
// and its vended log group, both in the gateway region (us-east-1): a CloudWatch Logs
// subscription filter can only target a function in the log group's own region. It writes
// cross-region to the Observability Cost_Store by table name + region.
package main

import (
	"context"
	"errors"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/aiplat/core/internal/reconcile"
)

const retention = 120 * 24 * time.Hour

var (
	ddb   *dynamodb.Client
	table = os.Getenv("COST_STORE_TABLE")
)

func s(v string) *ddbtypes.AttributeValueMemberS { return &ddbtypes.AttributeValueMemberS{Value: v} }
func n(v int) *ddbtypes.AttributeValueMemberN {
	return &ddbtypes.AttributeValueMemberN{Value: strconv.Itoa(v)}
}

// item builds the idempotent GATEWAY fact row. Pure (no IO): one gateway request → one row,
// keyed by the gateway request id, so at-least-once log delivery never double-counts.
func item(f reconcile.RoutingFact) map[string]ddbtypes.AttributeValue {
	expSec := f.EventTS/1000 + int64(retention/time.Second)
	it := map[string]ddbtypes.AttributeValue{
		"pk":                 s("GATEWAY"),
		"sk":                 s("REQ#" + f.GatewayRequestID),
		"gateway_request_id": s(f.GatewayRequestID),
		"resolved_model":     s(f.ResolvedModel),
		"requested_model":    s(f.RequestedModel),
		"target_name":        s(f.TargetName),
		"served_hop":         n(f.ServedHop),
		"invocations":        n(f.Invocations),
		"fallback":           &ddbtypes.AttributeValueMemberBOOL{Value: f.Fallback},
		"event_ts":           n(int(f.EventTS / 1000)),
		"expires_at":         &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(expSec, 10)},
	}
	return it
}

func handle(ctx context.Context, logsEvent events.CloudwatchLogsEvent) error {
	data, err := logsEvent.AWSLogs.Parse() // gunzip + base64 decode
	if err != nil {
		return err
	}
	for _, le := range data.LogEvents {
		f, ok := reconcile.ParseLogLine([]byte(le.Message))
		if !ok || f.GatewayRequestID == "" {
			continue
		}
		_, err := ddb.PutItem(ctx, &dynamodb.PutItemInput{
			TableName:           &table,
			Item:                item(f),
			ConditionExpression: awsString("attribute_not_exists(sk)"),
		})
		if err != nil {
			var ccf *ddbtypes.ConditionalCheckFailedException
			if errors.As(err, &ccf) {
				continue // already recorded (re-delivery) → idempotent
			}
			return err // real error → let Lambda retry the batch
		}
	}
	return nil
}

func awsString(v string) *string { return &v }

func main() {
	region := os.Getenv("COST_STORE_REGION")
	opts := []func(*awscfg.LoadOptions) error{}
	if region != "" {
		opts = append(opts, awscfg.WithRegion(region))
	}
	cfg, _ := awscfg.LoadDefaultConfig(context.TODO(), opts...)
	ddb = dynamodb.NewFromConfig(cfg)
	lambda.Start(handle)
}
