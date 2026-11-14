// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package main

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// fakeDDB serves a single ORG# config item from Scan; Query/PutItem are no-ops. A test that
// reaches metrics(Query) for the foreign org would be a bug (the filter should skip it).
type fakeDDB struct {
	scanItems []map[string]ddbtypes.AttributeValue
	queried   bool
	putCount  int
}

func (f *fakeDDB) Scan(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	return &dynamodb.ScanOutput{Items: f.scanItems}, nil
}
func (f *fakeDDB) Query(_ context.Context, _ *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	f.queried = true
	return &dynamodb.QueryOutput{}, nil
}
func (f *fakeDDB) PutItem(_ context.Context, _ *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.putCount++
	return &dynamodb.PutItemOutput{}, nil
}

// OBS-FUN-003: with DEPLOYMENT_ORG set, handle() evaluates ONLY that org. A foreign ORG#
// item carrying an alert + webhook must be skipped (evaluated 0, nothing queried, nothing
// written). Fails on the old code, which evaluated every ORG# item against the
// deployment-wide USAGE numbers.
func TestHandle_EvaluatesOnlyDeploymentOrg(t *testing.T) {
	prevDDB, prevOrg, prevCfg := ddb, deploymentOrg, cfgTable
	defer func() { ddb, deploymentOrg, cfgTable = prevDDB, prevOrg, prevCfg }()

	cfgTable = "cfg"
	deploymentOrg = "XYZ_ORG"
	// Only a FOREIGN org's config exists, with an alert rule and a webhook.
	foreignCfg := `{"alerts":{"webhook_url":"https://hooks.example/x","cost_usd":{"on":true,"threshold":1}}}`
	fake := &fakeDDB{scanItems: []map[string]ddbtypes.AttributeValue{
		{"pk": &ddbtypes.AttributeValueMemberS{Value: "ORG#fableaudit-alert"},
			"config": &ddbtypes.AttributeValueMemberS{Value: foreignCfg}},
	}}
	ddb = fake

	out, err := handle(context.Background())
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if out["evaluated"] != 0 {
		t.Fatalf("OBS-FUN-003: a foreign ORG# item must not be evaluated; evaluated=%v", out["evaluated"])
	}
	if out["fired"] != 0 {
		t.Fatalf("foreign org must not fire; fired=%v", out["fired"])
	}
	if fake.queried {
		t.Error("the foreign org must be skipped BEFORE the metrics query")
	}
	if fake.putCount != 0 {
		t.Errorf("no ALERTLOG write expected for a skipped org; got %d", fake.putCount)
	}
}
