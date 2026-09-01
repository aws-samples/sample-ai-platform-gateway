// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package govcore

import "testing"

func TestNewAuditEntry(t *testing.T) {
	e := NewAuditEntry("org_x", "  Admin@Corp.COM ", "admin", AuditPasswordReset, " Dev@Corp.COM", "reset por e-mail", "2026-08-11T00:00:00Z")
	if e.Actor != "admin@corp.com" {
		t.Errorf("actor not normalized: %q", e.Actor)
	}
	if e.Target != "dev@corp.com" {
		t.Errorf("target not normalized: %q", e.Target)
	}
	if e.Action != "password_reset" || e.Org != "org_x" || e.TS == "" {
		t.Errorf("unexpected fields: %+v", e)
	}
}
