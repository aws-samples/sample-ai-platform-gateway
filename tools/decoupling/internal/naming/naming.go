// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package naming is the single source of AIPlat's Naming Convention and of the
// account-dependent identifier constructors.
//
// Feature: multi-account-decoupling.
//
// The convention is: ${project}-${environment}-${dom}-${resource}
// (e.g.: aiplat-poc-inf-api-keys). project/environment are restricted to
// [a-z0-9] (no hyphen) to guarantee injectivity — see Property 2.
package naming

import (
	"fmt"
	"regexp"
)

// Doms are the valid domain prefixes. "inf" is the core's legacy prefix
// (preserved on purpose so tables/APIs are not recreated).
var Doms = map[string]bool{
	"inf": true, // core (legacy)
	"obs": true, // observability
	"gov": true, // governance
	"fe":  true, // frontend
	"bo":  true, // backoffice
}

var segmentRe = regexp.MustCompile(`^[a-z0-9]+$`)

// ValidateSegment restricts project/environment to [a-z0-9] (no hyphen and no
// uppercase). Without it, ("a","b-c") and ("a-b","c") would collide (Property 2).
func ValidateSegment(s string) error {
	if !segmentRe.MatchString(s) {
		return fmt.Errorf("invalid segment %q: must match ^[a-z0-9]+$", s)
	}
	return nil
}

// ValidateDom ensures the domain prefix is one of the known ones.
func ValidateDom(dom string) error {
	if !Doms[dom] {
		return fmt.Errorf("invalid domain %q", dom)
	}
	return nil
}

// Prefix returns "${project}-${environment}-${dom}".
func Prefix(project, environment, dom string) string {
	return project + "-" + environment + "-" + dom
}

// Name returns the resource name "${project}-${environment}-${dom}-${resource}".
// It is idempotent: same input, same output (Property 1).
func Name(project, environment, dom, resource string) string {
	return Prefix(project, environment, dom) + "-" + resource
}

// --- Account-dependent identifiers (built at apply time) ---
// No account literal is hard-coded (Requirements 1.2, 1.4).

// QueueURL: https://sqs.<region>.amazonaws.com/<account_id>/<name>
func QueueURL(region, accountID, name string) string {
	return "https://sqs." + region + ".amazonaws.com/" + accountID + "/" + name
}

// QueueARN: arn:aws:sqs:<region>:<account_id>:<name>
func QueueARN(region, accountID, name string) string {
	return "arn:aws:sqs:" + region + ":" + accountID + ":" + name
}

// EventBusARN: arn:aws:events:<region>:<account_id>:event-bus/<name>
func EventBusARN(region, accountID, name string) string {
	return "arn:aws:events:" + region + ":" + accountID + ":event-bus/" + name
}

var queueURLRe = regexp.MustCompile(`^https://sqs\.([^.]+)\.amazonaws\.com/([^/]+)/(.+)$`)
var queueARNRe = regexp.MustCompile(`^arn:aws:sqs:([^:]+):([^:]+):(.+)$`)

// ParseQueueURL recovers (region, accountID, name) from a queue URL.
func ParseQueueURL(url string) (region, accountID, name string, ok bool) {
	m := queueURLRe.FindStringSubmatch(url)
	if m == nil {
		return "", "", "", false
	}
	return m[1], m[2], m[3], true
}

// ParseQueueARN recovers (region, accountID, name) from a queue ARN.
func ParseQueueARN(arn string) (region, accountID, name string, ok bool) {
	m := queueARNRe.FindStringSubmatch(arn)
	if m == nil {
		return "", "", "", false
	}
	return m[1], m[2], m[3], true
}
