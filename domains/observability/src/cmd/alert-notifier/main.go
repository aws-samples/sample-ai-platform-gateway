// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// alert-notifier of the Observability domain: SCHEDULED alert evaluator.
// On every tick (EventBridge schedule) it scans the org-scoped configs from the
// gov-config table, computes the current month's metrics in the Cost_Store (data
// this domain owns) and, when a rule fires, POSTs to the org's webhook. A daily
// cooldown per rule prevents resends. This is the DELIVERY mechanism — the customer
// only plugs in the webhook URL.
//
// Boundary: reads the Governance config table as a CONTRACT (same pattern as the
// Core), with no synchronous call to the other domain's Lambda. The table name comes
// by convention (${project}-${environment}-gov-config), via env var.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aiplat/observability/internal/telemetry"
	"github.com/aws/aws-lambda-go/lambda"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

var (
	ddb       *dynamodb.Client
	costTable = os.Getenv("COST_STORE_TABLE")
	cfgTable  = os.Getenv("CONFIG_TABLE")
	// OBS-SEC-001: refuse redirects. Without CheckRedirect a validated https host
	// could 302 to http://169.254.169.254/… and the client would follow it, defeating
	// the SSRF guard. ErrUseLastResponse stops at the first response.
	httpc = &http.Client{
		Timeout:       8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
)

// validateWebhookURL is the SSRF guard on a customer-supplied alert webhook
// (OBS-SEC-001): https only, and the host must not resolve to a loopback, private,
// link-local or unspecified address (which would let a config write POST to
// 169.254.169.254 or an internal service). The same rule runs at write time in the
// governance config-api; this is the delivery-time check, since a config may predate
// validation. Kept deliberately identical in both places (a domain does not import
// another domain) — the shared contract is the rule, not the code.
func validateWebhookURL(raw string) error {
	u, err := neturl.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("invalid URL")
	}
	if u.Scheme != "https" {
		return fmt.Errorf("webhook must use https")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("webhook has no host")
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("webhook host does not resolve")
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return fmt.Errorf("webhook host resolves to a non-routable address")
		}
	}
	return nil
}

// rule describes how to evaluate each config.alerts key.
// cmp ">=" fires when value >= threshold; "<" when value < threshold.
type rule struct {
	key    string
	metric string
	cmp    string
	label  string
	unit   string
}

// The same rules as the console (minus the billing-quota rule, which was
// removed along with the rest of the plan/billing model).
var rules = []rule{
	// These labels reach the customer's webhook, so they are user-facing text and
	// follow the English-source rule. They intentionally match the ALOG_RULE map in
	// console.html, which is what lets the Alerts history translate them on screen.
	{"cost_usd", "cost_usd", ">=", "LLM spend this month", "US$"},
	{"latency_ms", "avg_latency_ms", ">=", "Average latency", "ms"},
	{"cache_below", "cache_pct", "<", "Low cache", "%"},
	{"error_rate", "error_rate", ">=", "Error rate (last hour)", "%"},
	{"provider_capacity", "provider_quota_count", ">=", "Provider capacity exhausted (last hour)", "count"},
}

func s(v string) *ddbtypes.AttributeValueMemberS { return &ddbtypes.AttributeValueMemberS{Value: v} }

func num(av ddbtypes.AttributeValue) float64 {
	if n, ok := av.(*ddbtypes.AttributeValueMemberN); ok {
		f, _ := strconv.ParseFloat(n.Value, 64)
		return f
	}
	return 0
}
func str(av ddbtypes.AttributeValue) string {
	if x, ok := av.(*ddbtypes.AttributeValueMemberS); ok {
		return x.Value
	}
	return ""
}
func boolOf(av ddbtypes.AttributeValue) bool {
	if b, ok := av.(*ddbtypes.AttributeValueMemberBOOL); ok {
		return b.Value
	}
	return false
}

type alertRule struct {
	On        bool    `json:"on"`
	Threshold float64 `json:"threshold"`
}
type alertsConfig struct {
	WebhookURL string               `json:"webhook_url"`
	Rules      map[string]alertRule `json:"-"`
}

// parseAlerts extracts webhook_url and the rules (dynamic keys) from the alerts block.
func parseAlerts(raw map[string]json.RawMessage) alertsConfig {
	ac := alertsConfig{Rules: map[string]alertRule{}}
	if w, ok := raw["webhook_url"]; ok {
		json.Unmarshal(w, &ac.WebhookURL)
	}
	for k, v := range raw {
		if k == "webhook_url" {
			continue
		}
		var r alertRule
		if json.Unmarshal(v, &r) == nil {
			ac.Rules[k] = r
		}
	}
	return ac
}

// metrics computes cost/latency/cache for the current month for one org (served only).
func metrics(ctx context.Context, org string) map[string]float64 {
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	m := map[string]float64{"cost_usd": 0, "avg_latency_ms": 0, "cache_pct": 0}
	var latSum, reqs, cacheHits float64
	var lek map[string]ddbtypes.AttributeValue
	for {
		out, err := ddb.Query(ctx, &dynamodb.QueryInput{
			TableName:              &costTable,
			KeyConditionExpression: awsString("pk = :p AND sk BETWEEN :a AND :b"),
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":p": s(telemetry.UsagePartition),
				":a": s("TS#" + monthStart),
				":b": s("TS#" + now.Format(time.RFC3339) + "~"),
			},
			ExclusiveStartKey: lek,
		})
		if err != nil {
			// OBS-NFR-003: a query failure mid-aggregation used to be swallowed, so a
			// partial month silently under-counted cost/cache. Log it (the metrics for
			// this org this tick are not trustworthy) and stop.
			log.Printf(`{"event":"alert_metrics_query_failed","org":%q,"error":%q}`, org, err.Error())
			break
		}
		for _, it := range out.Items {
			st := str(it["status"])
			if st != "" && st != "success" {
				continue // blocked/error does not count toward cost/latency
			}
			m["cost_usd"] += num(it["estimated_cost_usd"])
			latSum += num(it["latency_ms"])
			reqs++
			if boolOf(it["cache_hit"]) {
				cacheHits++
			}
		}
		if out.LastEvaluatedKey == nil {
			break
		}
		lek = out.LastEvaluatedKey
	}
	if reqs >= cacheMinReqs {
		m["avg_latency_ms"] = latSum / reqs
		m["cache_pct"] = cacheHits / reqs * 100
	} else {
		// OBS-NFR-003: below a minimum request count the cache ratio is noise. Return
		// the -1 "insufficient data" sentinel so the `cache_below` rule does not fire a
		// daily "Low cache" alert on an idle org (0 < threshold would otherwise fire).
		m["cache_pct"] = -1
	}
	return m
}

// cacheMinReqs is the minimum served requests in the month before the cache ratio is
// trustworthy enough to drive the `cache_below` alert.
const cacheMinReqs = 5

// errorRateRecent computes the error % over the last hour (the real monitoring
// window, not the month). Returns -1 when there is not enough traffic (< minReqs),
// to avoid alerting on noise. It counts errors (status="error") over the total of
// requests that reached the gateway (excluding policy blocks — those are not a
// service failure).
// errWindowRange counts (errors, eligible total) in a [fromISO, toISO] window.
// Excludes policy blocks (not a service failure).
func errWindowRange(ctx context.Context, org, fromISO, toISO string) (errs, total int) {
	var lek map[string]ddbtypes.AttributeValue
	for {
		out, err := ddb.Query(ctx, &dynamodb.QueryInput{
			TableName:              &costTable,
			KeyConditionExpression: awsString("pk = :p AND sk BETWEEN :a AND :b"),
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":p": s(telemetry.UsagePartition),
				":a": s("TS#" + fromISO),
				":b": s("TS#" + toISO + "~"),
			},
			ExclusiveStartKey: lek,
		})
		if err != nil {
			// OBS-NFR-003: do not silently return a partial count — a half-read window
			// understates the error rate and would mask a real burn.
			log.Printf(`{"event":"alert_errwindow_query_failed","org":%q,"from":%q,"to":%q,"error":%q}`, org, fromISO, toISO, err.Error())
			break
		}
		for _, it := range out.Items {
			st := str(it["status"])
			if st == "blocked" {
				continue
			}
			isSuccess := st == "" || st == "success"
			// OBS-NFR-003: mirror telemetry.SLI eligibility (eligible = success OR
			// sli_eligible). A failure that does not count against us — a client typo or
			// a provider-quota refusal (sli_eligible=false) — must not enter the error
			// rate / burn rate, or a customer's bad requests would drain the budget.
			if !isSuccess && !boolOf(it["sli_eligible"]) {
				continue
			}
			total++
			if st == "error" {
				errs++
			}
		}
		if out.LastEvaluatedKey == nil {
			break
		}
		lek = out.LastEvaluatedKey
	}
	return errs, total
}

// errWindow: [now-dur, now] window. Basis for error_rate and for the burn rate.
func errWindow(ctx context.Context, org string, dur time.Duration) (errs, total int) {
	now := time.Now().UTC()
	return errWindowRange(ctx, org, now.Add(-dur).Format(time.RFC3339), now.Format(time.RFC3339))
}

func errorRateRecent(ctx context.Context, org string) float64 {
	const minReqs = 5
	errs, total := errWindow(ctx, org, time.Hour)
	if total < minReqs {
		return -1
	}
	return float64(errs) / float64(total) * 100
}

// providerQuotaRecent counts events of exhausted provider quota/balance in the last
// hour. This is a CRITICAL CAPACITY signal (the customer ran out of anywhere to
// consume from) — a single occurrence is enough to matter. Outside the platform SLI,
// but it still alerts.
func providerQuotaRecent(ctx context.Context, org string) float64 {
	now := time.Now().UTC()
	from := now.Add(-1 * time.Hour).Format(time.RFC3339)
	var count float64
	var lek map[string]ddbtypes.AttributeValue
	for {
		out, err := ddb.Query(ctx, &dynamodb.QueryInput{
			TableName:              &costTable,
			KeyConditionExpression: awsString("pk = :p AND sk BETWEEN :a AND :b"),
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":p": s(telemetry.UsagePartition),
				":a": s("TS#" + from),
				":b": s("TS#" + now.Format(time.RFC3339) + "~"),
			},
			ExclusiveStartKey: lek,
		})
		if err != nil {
			// OBS-NFR-003: log rather than swallow — a partial read undercounts the
			// capacity signal.
			log.Printf(`{"event":"alert_quota_query_failed","org":%q,"error":%q}`, org, err.Error())
			break
		}
		for _, it := range out.Items {
			if str(it["reason"]) == "provider_quota_exceeded" {
				count++
			}
		}
		if out.LastEvaluatedKey == nil {
			break
		}
		lek = out.LastEvaluatedKey
	}
	return count
}

func fires(cmp string, value, threshold float64) bool {
	if cmp == "<" {
		return value < threshold
	}
	return value >= threshold
}

// After the hexagonal split, the reliability math (SLO per tier,
// multi-window burn rate and anomaly by binomial z-score) lives in the PURE domain
// internal/telemetry — testable offline, by property, with no SDK and no clock.
// What remains here is only ALIASES stitching the functions together: the
// alert-notifier becomes a shell (it orchestrates IO — Cost_Store and webhook — and
// delegates the decision to telemetry). The aliases preserve exactly the names the
// characterization suite (characterization_test.go) exercises, proving the move did
// not change behavior (D6).
var (
	burnRate    = telemetry.BurnRate
	evalBurn    = telemetry.EvalBurn
	evalAnomaly = telemetry.EvalAnomaly
	anomalyZ    = telemetry.AnomalyZ
)

// cooldownKey: one firing per rule per day (notification idempotency).
func cooldownHit(ctx context.Context, org, ruleKey string) bool {
	day := time.Now().UTC().Format("2006-01-02")
	pk := "ALERTSTATE#" + org
	sk := "RULE#" + ruleKey + "#" + day
	_, err := ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: &costTable,
		Item: map[string]ddbtypes.AttributeValue{
			"pk": s(pk), "sk": s(sk),
			"expires_at": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(time.Now().Add(48*time.Hour).Unix(), 10)},
		},
		ConditionExpression: awsString("attribute_not_exists(sk)"),
	})
	if err != nil {
		return true // already fired today (condition failed) → in cooldown
	}
	return false // first firing of the day → notify
}

// alertLogTTL: how long the firing history stays queryable. 90 days is enough for
// the customer to investigate "why wasn't I warned in March" without turning into a
// large table — the cooldown is daily per rule, so the volume is dozens of items per
// month, not thousands.
const alertLogTTL = 90 * 24 * time.Hour

// writeAlertLog persists the alert firing in the Cost_Store (this domain's data),
// already carrying the delivery outcome.
//
// Why persist: the cooldown consumes the firing for the day. If delivery fails, the
// alert is lost and, with no record, the customer never learns there was an alert
// that never reached them — they conclude everything was fine. The CloudWatch line
// solves this for us, not for the customer (they have no access to our log group).
//
// Never stores the webhook URL — only the HOST. Webhook URLs commonly carry a token
// in the path (Slack, Discord), and this record is read by the console.
func writeAlertLog(ctx context.Context, org string, payload map[string]interface{}, host string, status int, delivered bool) {
	now := time.Now().UTC()
	rule, _ := payload["rule"].(string)

	item := map[string]ddbtypes.AttributeValue{
		"pk":         s(telemetry.AlertLogPartition),
		"sk":         s("TS#" + now.Format(time.RFC3339Nano) + "#" + rule),
		"rule":       s(rule),
		"ts":         s(now.Format(time.RFC3339)),
		"delivered":  &ddbtypes.AttributeValueMemberBOOL{Value: delivered},
		"host":       s(host),
		"expires_at": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(now.Add(alertLogTTL).Unix(), 10)},
	}
	// Optional fields only go in when they exist: a burn rate rule has no
	// "threshold", a cost rule has no "severity". Smaller item, and absence stays
	// distinguishable from zero on read.
	for k, key := range map[string]string{
		"label": "label", "message": "message", "severity": "severity",
		"metric": "metric", "comparator": "comparator", "unit": "unit", "window": "window",
	} {
		if v, ok := payload[key].(string); ok && v != "" {
			item[k] = s(v)
		}
	}
	for k, key := range map[string]string{
		"value": "value", "threshold": "threshold", "burn_rate": "burn_rate",
		"slo_target": "slo_target", "current_pct": "current_pct", "baseline_pct": "baseline_pct",
	} {
		if v, ok := payload[key].(float64); ok {
			item[k] = &ddbtypes.AttributeValueMemberN{Value: strconv.FormatFloat(v, 'f', -1, 64)}
		}
	}
	if status > 0 {
		item["status_code"] = &ddbtypes.AttributeValueMemberN{Value: strconv.Itoa(status)}
	}

	if _, err := ddb.PutItem(ctx, &dynamodb.PutItemInput{TableName: &costTable, Item: item}); err != nil {
		// Best-effort: failing here must not take down the evaluation of other alerts.
		log.Printf(`{"event":"alert_log_write_failed","org":%q,"rule":%q,"error":%q}`, org, rule, err.Error())
	}
}

// postWebhook delivers the alert, LOGS the outcome and PERSISTS the firing.
//
// Delivery is best-effort and the cooldown has already consumed the firing for the
// day: if the customer's webhook is down, the alert is lost. The log solves that for
// us; the persisted record solves it for the CUSTOMER, who has no access to our log
// group. Logs and stores only the host (never the whole URL, which may carry a token
// in the path).
func postWebhook(ctx context.Context, org, url string, payload map[string]interface{}) {
	rule, _ := payload["rule"].(string)
	host := hostOf(url)

	b, _ := json.Marshal(payload)
	// Carries the handler's context so a hanging customer webhook is cancelled
	// with the invocation instead of outliving it.
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		logWebhook(org, rule, url, 0, err)
		writeAlertLog(ctx, org, payload, host, 0, false)
		return
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("user-agent", "aiplat-alert-notifier")
	resp, err := httpc.Do(req)
	if err != nil {
		logWebhook(org, rule, url, 0, err)
		writeAlertLog(ctx, org, payload, host, 0, false)
		return
	}
	defer resp.Body.Close()
	logWebhook(org, rule, url, resp.StatusCode, nil)
	// 2xx is delivery; any other code is the customer's webhook refusing —
	// and that needs to be visible to them, not just in our log.
	writeAlertLog(ctx, org, payload, host, resp.StatusCode, resp.StatusCode >= 200 && resp.StatusCode < 300)
}

// hostOf extracts only the host from the URL. Used in the log and in the persisted
// record: the whole URL may carry a token in the path.
func hostOf(rawURL string) string {
	if u, e := neturl.Parse(rawURL); e == nil && u.Host != "" {
		return u.Host
	}
	return "invalid"
}

func logWebhook(org, rule, rawURL string, status int, err error) {
	host := hostOf(rawURL)
	ev := map[string]interface{}{
		"event": "alert_webhook", "org": org, "rule": rule, "host": host,
		"delivered": err == nil && status >= 200 && status < 300,
	}
	if status > 0 {
		ev["status"] = status
	}
	if err != nil {
		// The net/http error comes wrapped in *url.Error, which prints the WHOLE
		// URL. Webhook URLs commonly carry a token in the path (Slack, Discord), so
		// logging raw err.Error() would leak a customer secret into CloudWatch.
		// Unwrap so only the cause remains (DNS, timeout, TLS…).
		var ue *neturl.Error
		if errors.As(err, &ue) && ue.Err != nil {
			ev["error"] = ue.Err.Error()
		} else {
			ev["error"] = err.Error()
		}
	}
	line, _ := json.Marshal(ev)
	log.Println(string(line))
}

// orgConfigs returns the configs at ORG scope (pk == ORG#<org>, without #TEAM#/#APP#).
func orgConfigs(ctx context.Context) map[string]map[string]json.RawMessage {
	res := map[string]map[string]json.RawMessage{}
	var lek map[string]ddbtypes.AttributeValue
	for {
		out, err := ddb.Scan(ctx, &dynamodb.ScanInput{TableName: &cfgTable, ExclusiveStartKey: lek})
		if err != nil {
			break
		}
		for _, it := range out.Items {
			pk := str(it["pk"])
			if !strings.HasPrefix(pk, "ORG#") || strings.Contains(pk, "#TEAM#") || strings.Contains(pk, "#APP#") {
				continue
			}
			org := strings.TrimPrefix(pk, "ORG#")
			var cfg map[string]json.RawMessage
			if json.Unmarshal([]byte(str(it["config"])), &cfg) == nil {
				res[org] = cfg
			}
		}
		if out.LastEvaluatedKey == nil {
			break
		}
		lek = out.LastEvaluatedKey
	}
	return res
}

func handle(ctx context.Context) (map[string]interface{}, error) {
	evaluated, fired := 0, 0
	for org, cfg := range orgConfigs(ctx) {
		alertsRaw, ok := cfg["alerts"]
		if !ok {
			continue
		}
		var raw map[string]json.RawMessage
		if json.Unmarshal(alertsRaw, &raw) != nil {
			continue
		}
		ac := parseAlerts(raw)
		if ac.WebhookURL == "" {
			continue // no channel, nothing to deliver
		}
		// OBS-SEC-001: refuse to deliver to a webhook that fails the SSRF guard
		// (non-https, or a host resolving to a private/loopback/link-local address).
		if err := validateWebhookURL(ac.WebhookURL); err != nil {
			log.Printf(`{"event":"alert_webhook_rejected","org":%q,"host":%q,"error":%q}`, org, hostOf(ac.WebhookURL), err.Error())
			continue
		}
		burnOn := false
		if rc, ok := ac.Rules["slo_burn"]; ok && rc.On {
			burnOn = true
		}
		anomalyOn := false
		if rc, ok := ac.Rules["anomaly"]; ok && rc.On {
			anomalyOn = true
		}
		anyOn := false
		for _, r := range rules {
			if rc, ok := ac.Rules[r.key]; ok && rc.On {
				anyOn = true
			}
		}
		if !anyOn && !burnOn && !anomalyOn {
			continue
		}
		mvals := metrics(ctx, org)
		// short windows (last hour), computed separately from the month.
		mvals["error_rate"] = errorRateRecent(ctx, org)
		mvals["provider_quota_count"] = providerQuotaRecent(ctx, org)

		// --- Multi-window burn rate (only alerts when the error budget is threatened) ---
		if burnOn {
			slo := telemetry.SLOTarget
			e1, t1 := errWindow(ctx, org, time.Hour)
			e6, t6 := errWindow(ctx, org, 6*time.Hour)
			sev, burn, win := evalBurn(e1, t1, e6, t6, slo)
			if sev != "" {
				evaluated++
				if !cooldownHit(ctx, org, "slo_burn_"+sev) {
					fired++
					postWebhook(ctx, org, ac.WebhookURL, map[string]interface{}{
						"source": "aiplat.alerts", "org": org, "rule": "slo_burn",
						"label": "Error budget burn (SLO)", "severity": sev, "window": win,
						"burn_rate": burn, "slo_target": slo,
						"ts":      time.Now().UTC().Format(time.RFC3339),
						"message": "Burn rate " + strconv.FormatFloat(burn, 'f', 1, 64) + "x over " + win + " (SLO " + strconv.FormatFloat(slo, 'f', 1, 64) + "%) — " + sev,
					})
				}
			}
		}

		// --- Anomaly vs. the CUSTOMER'S OWN baseline (Layer 3) ---
		// Learns the customer's normal error rate (7 days, excluding the last hour)
		// and alerts when the last hour deviates significantly from THEIR normal —
		// catching a spike even inside the global SLO. Note: recomputing 7d on every
		// run is simple; at scale, precompute the baseline in a daily job (known debt).
		if anomalyOn {
			now := time.Now().UTC()
			be, bt := errWindowRange(ctx, org, now.AddDate(0, 0, -7).Format(time.RFC3339), now.Add(-1*time.Hour).Format(time.RFC3339))
			ce, ct := errWindow(ctx, org, time.Hour)
			z, curRate, pBase, fire := evalAnomaly(be, bt, ce, ct)
			if fire {
				evaluated++
				if !cooldownHit(ctx, org, "anomaly") {
					fired++
					postWebhook(ctx, org, ac.WebhookURL, map[string]interface{}{
						"source": "aiplat.alerts", "org": org, "rule": "anomaly",
						"label":        "Anomaly vs. customer baseline",
						"current_pct":  math.Round(curRate*1000) / 10,
						"baseline_pct": math.Round(pBase*1000) / 10,
						"z_score":      math.Round(z*10) / 10,
						"ts":           now.Format(time.RFC3339),
						"message":      "Last hour error rate (" + strconv.FormatFloat(curRate*100, 'f', 1, 64) + "%) far above this customer's normal (" + strconv.FormatFloat(pBase*100, 'f', 1, 64) + "%), z=" + strconv.FormatFloat(z, 'f', 1, 64),
					})
				}
			}
		}
		for _, r := range rules {
			rc, ok := ac.Rules[r.key]
			if !ok || !rc.On {
				continue
			}
			evaluated++
			val := mvals[r.metric]
			// OBS-NFR-003: -1 is the "insufficient data" sentinel (cache_pct/error_rate
			// below the minimum request count). Skip rather than fire on noise — with a
			// "<" comparator, -1 would otherwise always trip cache_below.
			if val < 0 {
				continue
			}
			if !fires(r.cmp, val, rc.Threshold) {
				continue
			}
			if cooldownHit(ctx, org, r.key) {
				continue // already notified today
			}
			fired++
			postWebhook(ctx, org, ac.WebhookURL, map[string]interface{}{
				"source":     "aiplat.alerts",
				"org":        org,
				"rule":       r.key,
				"label":      r.label,
				"metric":     r.metric,
				"value":      val,
				"threshold":  rc.Threshold,
				"comparator": r.cmp,
				"unit":       r.unit,
				"ts":         time.Now().UTC().Format(time.RFC3339),
				"message":    r.label + " fired: " + strconv.FormatFloat(val, 'f', 2, 64) + " " + r.cmp + " " + strconv.FormatFloat(rc.Threshold, 'f', 2, 64),
			})
		}
	}
	return map[string]interface{}{"evaluated": evaluated, "fired": fired}, nil
}

func awsString(v string) *string { return &v }

func main() {
	cfg, _ := awscfg.LoadDefaultConfig(context.TODO())
	ddb = dynamodb.NewFromConfig(cfg)
	lambda.Start(handle)
}
