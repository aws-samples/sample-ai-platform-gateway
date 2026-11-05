// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package gateway

import (
	"context"
	"testing"
)

// fakeConfigStore returns the base config with an override layered on top — enough to
// drive loadConfig without DynamoDB.
type fakeConfigStore struct{ override map[string]interface{} }

func (f fakeConfigStore) Effective(_ context.Context, _, _ string, base map[string]interface{}) (map[string]interface{}, string, string) {
	m := map[string]interface{}{}
	for k, v := range base {
		m[k] = v
	}
	for k, v := range f.override {
		m[k] = v
	}
	return m, "ORG#test", "ORG#test"
}

// CORE-FUN-002 / FE-FUN-001: the console writes cache_ttl:0 to mean "Cache OFF". The
// gateway must keep 0 as 0 (disabled), not resurrect it into 3600. The 3600 default
// lives in envConfig (the base of the merge), so a 0 after the merge can only be a
// scope that explicitly turned the cache off.
func TestLoadConfig_CacheOffStaysOff(t *testing.T) {
	prev := configStore
	t.Cleanup(func() { configStore = prev })

	configStore = fakeConfigStore{override: map[string]interface{}{"cache_ttl": 0}}
	c := loadConfig(context.Background(), "team", "app")
	if c.CacheTTL != 0 {
		t.Fatalf("cache_ttl 0 (console 'off') must stay 0, got %d", c.CacheTTL)
	}
	if got := effectiveCacheTTL(c); got != 0 {
		t.Errorf("effectiveCacheTTL with cache off = %d, want 0 (cache disabled)", got)
	}
}

// When no scope sets cache_ttl, the envConfig default (3600) must survive the merge.
func TestLoadConfig_DefaultTTLWhenUnset(t *testing.T) {
	prev := configStore
	t.Cleanup(func() { configStore = prev })

	configStore = fakeConfigStore{override: nil}
	c := loadConfig(context.Background(), "team", "app")
	if c.CacheTTL != 3600 {
		t.Fatalf("unset cache_ttl must default to 3600, got %d", c.CacheTTL)
	}
}

// effectiveCacheTTL: 0 means off, UNLESS the semantic cache is on (then the store needs
// a live TTL so the semantic index has responses to serve).
func TestEffectiveCacheTTL(t *testing.T) {
	if got := effectiveCacheTTL(&Config{CacheTTL: 0}); got != 0 {
		t.Errorf("cache_ttl 0, semantic off → %d, want 0", got)
	}
	if got := effectiveCacheTTL(&Config{CacheTTL: 0, SemanticCache: true}); got != semDefaultStoreTTL {
		t.Errorf("cache_ttl 0, semantic on → %d, want %d", got, semDefaultStoreTTL)
	}
	if got := effectiveCacheTTL(&Config{CacheTTL: 600}); got != 600 {
		t.Errorf("cache_ttl 600 → %d, want 600", got)
	}
}
