// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package main

import "testing"

// The merge in listBedrockModels talks to Bedrock, so these cover the pure parts it
// depends on. They are the parts that decide WHICH id ends up in the dropdown, and a
// wrong id fails only at invocation time, in the customer's account.

func TestBaseModelFromARN(t *testing.T) {
	cases := map[string]string{
		"arn:aws:bedrock:us-west-2::foundation-model/anthropic.claude-sonnet-5": "anthropic.claude-sonnet-5",
		"arn:aws:bedrock:us-east-1::foundation-model/amazon.nova-pro-v1:0":      "amazon.nova-pro-v1:0",
		// Already bare, or an unexpected shape: returned as-is rather than emptied,
		// because an empty base model id silently drops the profile's metadata.
		"amazon.nova-lite-v1:0": "amazon.nova-lite-v1:0",
		"":                      "",
	}
	for in, want := range cases {
		if got := baseModelFromARN(in); got != want {
			t.Errorf("baseModelFromARN(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVendorFromModelID(t *testing.T) {
	cases := map[string]string{
		// Every cross-region prefix has to be stripped, otherwise the vendor comes
		// out as "Us" and every model lands in the same bucket.
		"us.amazon.nova-premier-v1:0":      "Amazon",
		"global.anthropic.claude-sonnet-5": "Anthropic",
		"eu.anthropic.claude-sonnet-4":     "Anthropic",
		"apac.amazon.nova-lite-v1:0":       "Amazon",
		"us-gov.anthropic.claude-haiku-4":  "Anthropic",
		// Bare foundation model id.
		"mistral.mistral-7b-instruct-v0:2": "Mistral",
		// No vendor segment at all: empty, so the caller keeps whatever it had.
		"": "",
	}
	for in, want := range cases {
		if got := vendorFromModelID(in); got != want {
			t.Errorf("vendorFromModelID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestModelModes(t *testing.T) {
	type modality string
	if got := modelModes([]modality{"TEXT", "IMAGE"}); got != "TEXT,IMAGE" {
		t.Errorf("got %q", got)
	}
	// An empty list must be the empty string, not ",": listBedrockModels treats an
	// UNDECLARED modality as "keep it", and a stray separator would break that test.
	if got := modelModes([]modality{}); got != "" {
		t.Errorf("got %q, want empty", got)
	}
	if got := modelModes[modality](nil); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
