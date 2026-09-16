// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Regression test for the F-02 audit finding: GET /admin/keys issued a single
// unpaginated Scan, so an org whose table passed DynamoDB's ~1 MB page boundary saw a
// partial key list presented as complete. scanAllKeys must follow LastEvaluatedKey.
package main

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// fakeScanner replays a fixed sequence of pages and records the ExclusiveStartKey it was
// called with, so the test can assert the loop actually threads LastEvaluatedKey through.
type fakeScanner struct {
	pages     []*dynamodb.ScanOutput
	calls     int
	startKeys []map[string]ddbtypes.AttributeValue
}

func (f *fakeScanner) Scan(_ context.Context, in *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	f.startKeys = append(f.startKeys, in.ExclusiveStartKey)
	out := f.pages[f.calls]
	f.calls++
	return out, nil
}

func TestScanAllKeys_FollowsLastEvaluatedKey(t *testing.T) {
	lek := map[string]ddbtypes.AttributeValue{"api_key_hash": s("page1-last")}
	fs := &fakeScanner{pages: []*dynamodb.ScanOutput{
		{
			Items: []map[string]ddbtypes.AttributeValue{
				{"api_key_hash": s("k1")},
				{"api_key_hash": s("k2")},
			},
			LastEvaluatedKey: lek,
		},
		{
			Items: []map[string]ddbtypes.AttributeValue{
				{"api_key_hash": s("k3")},
			},
			LastEvaluatedKey: nil,
		},
	}}

	items, err := scanAllKeys(context.Background(), fs, "acme")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3 (pagination did not follow LastEvaluatedKey)", len(items))
	}
	if fs.calls != 2 {
		t.Fatalf("got %d Scan calls, want 2", fs.calls)
	}
	if fs.startKeys[0] != nil {
		t.Fatalf("first call must start with no ExclusiveStartKey, got %v", fs.startKeys[0])
	}
	got, ok := fs.startKeys[1]["api_key_hash"].(*ddbtypes.AttributeValueMemberS)
	if !ok || got.Value != "page1-last" {
		t.Fatalf("second call's ExclusiveStartKey = %v, want page 1's LastEvaluatedKey", fs.startKeys[1])
	}
}

func TestScanAllKeys_SinglePage(t *testing.T) {
	fs := &fakeScanner{pages: []*dynamodb.ScanOutput{
		{
			Items:            []map[string]ddbtypes.AttributeValue{{"api_key_hash": s("only")}},
			LastEvaluatedKey: nil,
		},
	}}
	items, err := scanAllKeys(context.Background(), fs, "acme")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 1 || fs.calls != 1 {
		t.Fatalf("got %d items / %d calls, want 1 / 1", len(items), fs.calls)
	}
}
