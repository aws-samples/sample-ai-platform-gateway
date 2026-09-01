// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// hints-publisher: aggregates the Cost_Store and publishes the Routing_Hints.
//
// Asynchronous contract Observability → Core. The Core needs two things to decide
// routing and CANNOT obtain them through a synchronous call to this domain (the
// golden rule of `aiplat-domains.md`):
//
//  1. E[tokens_out] per (org, feature, model) — the historical median is a much
//     better prediction than any heuristic, and it is data only we have.
//  2. An unavailability signal per model/provider — derived from status/reason.
//
// We publish a versioned artifact that the Core reads as data, with a short cache and
// fallback. If this Lambda stops, the Core degrades to the heuristic and keeps serving.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const hintsVersion = 1

var (
	ddb        *dynamodb.Client
	costTable  = os.Getenv("COST_STORE_TABLE")
	hintsTable = os.Getenv("HINTS_TABLE")
	cfgTable   = os.Getenv("CONFIG_TABLE")
)

func windowDays() int {
	if v, err := strconv.Atoi(os.Getenv("WINDOW_DAYS")); err == nil && v > 0 {
		return v
	}
	return 7
}

// samplePoint is one observation of a served request.
type samplePoint struct {
	feature string
	model   string
	outTok  int
}

// failPoint is one failure observation, for the unavailability signal.
type failPoint struct {
	model    string
	provider string
	reason   string
	at       time.Time
}

func str(av ddbtypes.AttributeValue) string {
	if s, ok := av.(*ddbtypes.AttributeValueMemberS); ok {
		return s.Value
	}
	return ""
}
func num(av ddbtypes.AttributeValue) float64 {
	if n, ok := av.(*ddbtypes.AttributeValueMemberN); ok {
		f, _ := strconv.ParseFloat(n.Value, 64)
		return f
	}
	return 0
}

// orgs discovers the existing orgs by reading gov-config as a contract.
// Only the pure ORG#<org> keys (without TEAM#/APP#) matter — that is the level where
// credit and policy live, and it is the Cost_Store partition.
func orgs(ctx context.Context) ([]string, error) {
	var out []string
	var lek map[string]ddbtypes.AttributeValue
	for {
		r, err := ddb.Scan(ctx, &dynamodb.ScanInput{
			TableName:            &cfgTable,
			ProjectionExpression: aws.String("pk"),
			ExclusiveStartKey:    lek,
		})
		if err != nil {
			return nil, err
		}
		for _, it := range r.Items {
			pk := str(it["pk"])
			if !strings.HasPrefix(pk, "ORG#") || strings.Contains(pk, "#TEAM#") || strings.Contains(pk, "#APP#") {
				continue
			}
			out = append(out, strings.TrimPrefix(pk, "ORG#"))
		}
		if r.LastEvaluatedKey == nil {
			break
		}
		lek = r.LastEvaluatedKey
	}
	return out, nil
}

// collect reads one org's window with a Query on its own partition.
// Query by partition, never Scan: isolation is structural, not a correct WHERE.
func collect(ctx context.Context, org string, from time.Time) ([]samplePoint, []failPoint, error) {
	var samples []samplePoint
	var fails []failPoint
	var lek map[string]ddbtypes.AttributeValue
	skFrom := "TS#" + from.UTC().Format(time.RFC3339)
	skTo := "TS#9999"

	for {
		r, err := ddb.Query(ctx, &dynamodb.QueryInput{
			TableName:              &costTable,
			KeyConditionExpression: aws.String("pk = :p AND sk BETWEEN :a AND :b"),
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":p": &ddbtypes.AttributeValueMemberS{Value: "TENANT#" + org},
				":a": &ddbtypes.AttributeValueMemberS{Value: skFrom},
				":b": &ddbtypes.AttributeValueMemberS{Value: skTo},
			},
			ExclusiveStartKey: lek,
		})
		if err != nil {
			return nil, nil, err
		}
		for _, it := range r.Items {
			status := str(it["status"])
			mode := str(it["mode"])
			// Only the synchronous path feeds the prediction: batch has a different
			// load shape and would contaminate the median.
			if mode != "" && mode != "sync" {
				continue
			}
			model := str(it["model"])
			if model == "" {
				continue
			}
			if status == "" || status == "success" {
				// A cache hit has zero tokens_out and would skew the median downward.
				if b, ok := it["cache_hit"].(*ddbtypes.AttributeValueMemberBOOL); ok && b.Value {
					continue
				}
				out := int(num(it["tokens_out"]))
				if out <= 0 {
					continue
				}
				samples = append(samples, samplePoint{
					feature: str(it["feature"]), model: model, outTok: out,
				})
				continue
			}
			ts, _ := time.Parse(time.RFC3339, str(it["ts"]))
			fails = append(fails, failPoint{
				model: model, provider: str(it["provider"]),
				reason: str(it["reason"]), at: ts,
			})
		}
		if r.LastEvaluatedKey == nil {
			break
		}
		lek = r.LastEvaluatedKey
	}
	return samples, fails, nil
}

func median(v []int) int {
	if len(v) == 0 {
		return 0
	}
	sort.Ints(v)
	n := len(v)
	if n%2 == 1 {
		return v[n/2]
	}
	return (v[n/2-1] + v[n/2]) / 2
}

// unavailability derives until when to avoid each model/provider.
//
// Only reasons that indicate UNAVAILABILITY are counted. A policy failure (rate
// limit, budget, guardrail) or a customer config failure does NOT make the model
// unavailable — the model is fine, it was the request that was refused. Conflating
// the two would take good models out of circulation.
func unavailability(fails []failPoint, now time.Time) map[string]time.Time {
	const quotaWindow = 30 * time.Minute
	const failWindow = 10 * time.Minute

	out := map[string]time.Time{}
	mark := func(key string, until time.Time) {
		if key == "" {
			return
		}
		if cur, ok := out[key]; !ok || until.After(cur) {
			out[key] = until
		}
	}
	for _, f := range fails {
		if f.at.IsZero() {
			continue
		}
		switch f.reason {
		case "provider_quota_exceeded", "provider_rate_limited":
			// The customer's quota/balance at the provider: avoid for longer, because
			// it does not clear itself in seconds.
			if now.Sub(f.at) < quotaWindow {
				mark(f.model, f.at.Add(quotaWindow))
			}
		case "provider_unreachable", "provider_down":
			// Provider down: short window, because it comes back on its own.
			if now.Sub(f.at) < failWindow {
				mark(f.provider, f.at.Add(failWindow))
			}
		}
	}
	return out
}

type artifact struct {
	Version     int            `json:"version"`
	GeneratedAt string         `json:"generated_at"`
	WindowDays  int            `json:"window_days"`
	Samples     int            `json:"samples"`
	MedianOut   map[string]int `json:"median_out_by_model"`
	// SamplesByKey is the sample count PER KEY, not of the artifact.
	//
	// Without it, a feature item with 3 samples would make the consumer discard the
	// org's aggregate medians too, which may have hundreds — throwing away
	// well-supported data because of a counter at the wrong scope. The confidence
	// threshold has to be evaluated on the key that is actually going to be used.
	SamplesByKey map[string]int `json:"samples_by_key"`
	Unavailable  map[string]int `json:"unavailable_until_unix"`
}

func publish(ctx context.Context, org, feature string, a artifact, ttl time.Time) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	sk := "HINTS#v" + strconv.Itoa(hintsVersion) + "#"
	if feature == "" {
		sk += "*"
	} else {
		sk += feature
	}
	_, err = ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: &hintsTable,
		Item: map[string]ddbtypes.AttributeValue{
			"pk":         &ddbtypes.AttributeValueMemberS{Value: "ORG#" + org},
			"sk":         &ddbtypes.AttributeValueMemberS{Value: sk},
			"hints":      &ddbtypes.AttributeValueMemberS{Value: string(b)},
			"expires_at": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(ttl.Unix(), 10)},
		},
	})
	return err
}

func handle(ctx context.Context) (map[string]interface{}, error) {
	if costTable == "" || hintsTable == "" || cfgTable == "" {
		return nil, fmt.Errorf("missing env: COST_STORE_TABLE/HINTS_TABLE/CONFIG_TABLE")
	}
	now := time.Now().UTC()
	from := now.AddDate(0, 0, -windowDays())
	// TTL of 3x the cadence: if the publisher stops, the hint expires and the Core
	// falls back to the heuristic instead of deciding on stale data.
	ttl := now.Add(3 * time.Hour)

	list, err := orgs(ctx)
	if err != nil {
		return nil, err
	}

	published, skipped := 0, 0
	for _, org := range list {
		samples, fails, err := collect(ctx, org, from)
		if err != nil {
			// One org with a problem must not block the others.
			fmt.Printf(`{"lvl":"error","evt":"hints_collect_failed","org":%q,"err":%q}`+"\n", org, err.Error())
			continue
		}
		if len(samples) == 0 && len(fails) == 0 {
			skipped++
			continue
		}

		unav := map[string]int{}
		for k, t := range unavailability(fails, now) {
			unav[k] = int(t.Unix())
		}

		// Group by feature|model and by *|model (the org aggregate).
		byFeatureModel := map[string][]int{}
		byModel := map[string][]int{}
		perFeature := map[string]int{}
		for _, s := range samples {
			if s.feature != "" {
				byFeatureModel[s.feature+"|"+s.model] = append(byFeatureModel[s.feature+"|"+s.model], s.outTok)
				perFeature[s.feature]++
			}
			byModel["*|"+s.model] = append(byModel["*|"+s.model], s.outTok)
		}

		// Org aggregate item (fallback when there is no hint for the feature).
		orgMed, orgN := map[string]int{}, map[string]int{}
		for k, v := range byModel {
			orgMed[k], orgN[k] = median(v), len(v)
		}
		if err := publish(ctx, org, "", artifact{
			Version: hintsVersion, GeneratedAt: now.Format(time.RFC3339),
			WindowDays: windowDays(), Samples: len(samples),
			MedianOut: orgMed, SamplesByKey: orgN, Unavailable: unav,
		}, ttl); err != nil {
			fmt.Printf(`{"lvl":"error","evt":"hints_publish_failed","org":%q,"err":%q}`+"\n", org, err.Error())
			continue
		}
		published++

		// One item per feature: the Core does a GetItem for the current request's
		// feature only, so small items instead of one giant item per org.
		for feature, n := range perFeature {
			med, cnt := map[string]int{}, map[string]int{}
			for k, v := range byFeatureModel {
				if strings.HasPrefix(k, feature+"|") {
					med[k], cnt[k] = median(v), len(v)
				}
			}
			// Repeat the org aggregate as a fallback inside the item itself, with ITS
			// own count — that is what allows using the aggregate when the feature does
			// not have enough history yet.
			for k, v := range orgMed {
				med[k], cnt[k] = v, orgN[k]
			}
			if err := publish(ctx, org, feature, artifact{
				Version: hintsVersion, GeneratedAt: now.Format(time.RFC3339),
				WindowDays: windowDays(), Samples: n,
				MedianOut: med, SamplesByKey: cnt, Unavailable: unav,
			}, ttl); err == nil {
				published++
			}
		}
	}

	res := map[string]interface{}{
		"orgs": len(list), "published": published, "skipped": skipped,
		"window_days": windowDays(),
	}
	b, _ := json.Marshal(res)
	fmt.Printf(`{"lvl":"info","evt":"hints_published","result":%s}`+"\n", b)
	return res, nil
}

func main() {
	cfg, _ := awscfg.LoadDefaultConfig(context.TODO())
	ddb = dynamodb.NewFromConfig(cfg)
	lambda.Start(handle)
}
