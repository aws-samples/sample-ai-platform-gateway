// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// pretoken of the Governance domain: Cognito pre-token-generation trigger.
//
// Injects the member's team scope INTO the JWT. Reads the MEMBER#<org>#<sub>
// record from the config and adds the `team` and `apps` claims to the token.
// This way the access scope TRAVELS with the request and every API trusts the
// authorizer — with no cross-domain read of the members table on the hot path.
//
// It is the foundation of per-team enforcement (Slice B): usage-api / keyadmin /
// config-api read the `team` claim from the JWT and force the scope. See
// aiplat-security.md.
package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// ddbGetter is the one DynamoDB operation the trigger needs. Narrowing the global
// to an interface (satisfied by *dynamodb.Client) lets a test inject a fake and
// exercise handle end-to-end — which is how the GOV-SEC-003 sub-keying is proven
// behaviourally rather than by inspecting a key string.
type ddbGetter interface {
	GetItem(ctx context.Context, in *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
}

var (
	ddb         ddbGetter
	configTable = os.Getenv("CONFIG_TABLE")
)

func handle(ctx context.Context, ev events.CognitoEventUserPoolsPreTokenGen) (events.CognitoEventUserPoolsPreTokenGen, error) {
	attrs := ev.Request.UserAttributes
	org := attrs["custom:org_id"]
	// GOV-SEC-003: the MEMBER# access record is keyed by the member's IMMUTABLE
	// Cognito sub, never the e-mail. The e-mail is mutable (a member can change and
	// re-verify it); keying the scope by e-mail let a team-scoped member shed their
	// scope by moving to an address with no MEMBER# record. There is NO e-mail
	// fallback here on purpose — a fallback would reopen exactly that escape for any
	// member whose record had not yet been re-keyed.
	sub := strings.TrimSpace(attrs["sub"])
	if org == "" || sub == "" || configTable == "" {
		return ev, nil // no scope to inject
	}

	pk := "MEMBER#" + org + "#" + sub
	out, err := ddb.GetItem(ctx, &dynamodb.GetItemInput{TableName: &configTable,
		Key: map[string]ddbtypes.AttributeValue{"pk": &ddbtypes.AttributeValueMemberS{Value: pk}}})
	if err != nil || out.Item == nil {
		return ev, nil // owner/admin with no member record → no restriction
	}
	var meta map[string]interface{}
	if cv, ok := out.Item["config"].(*ddbtypes.AttributeValueMemberS); ok {
		json.Unmarshal([]byte(cv.Value), &meta)
	}

	if add := claimsFromMeta(meta); len(add) > 0 {
		ev.Response.ClaimsOverrideDetails.ClaimsToAddOrOverride = add
	}
	return ev, nil
}

// claimsFromMeta derives the claims to inject (team + apps) from the MEMBER#
// record. PURE: no IO, testable. apps becomes a comma-separated list; an empty
// team and empty apps are omitted (an unrestricted owner/admin gets no claim).
func claimsFromMeta(meta map[string]interface{}) map[string]string {
	add := map[string]string{}
	if t, ok := meta["team"].(string); ok && t != "" {
		add["team"] = t
	}
	if raw, ok := meta["apps"].([]interface{}); ok && len(raw) > 0 {
		var apps []string
		for _, a := range raw {
			if s, ok := a.(string); ok && s != "" {
				apps = append(apps, s)
			}
		}
		if len(apps) > 0 {
			add["apps"] = strings.Join(apps, ",")
		}
	}
	return add
}

func main() {
	cfg, _ := awscfg.LoadDefaultConfig(context.TODO())
	ddb = dynamodb.NewFromConfig(cfg)
	lambda.Start(handle)
}
