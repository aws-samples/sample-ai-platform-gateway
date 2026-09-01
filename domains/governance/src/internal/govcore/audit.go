// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package govcore

import "strings"

// Audited administrative actions (control plane). Constants so magic strings do
// not spread through the shell and the vocabulary stays stable in the log.
const (
	AuditMemberInvite  = "member_invite"
	AuditMemberUpdate  = "member_update"
	AuditMemberRemove  = "member_remove"
	AuditPasswordReset = "password_reset"
	AuditInviteResend  = "invite_resend"
	AuditMemberEnable  = "member_enable"
	AuditMemberDisable = "member_disable"
)

// AuditEntry is the record of an administrative action: who did what, to whom,
// when. It NEVER carries a password, token or secret — metadata only. It is pure
// data; the shell decides where to store it (DynamoDB) and how to query it.
type AuditEntry struct {
	Org       string `json:"org"`
	Actor     string `json:"actor"`      // e-mail of whoever executed it
	ActorRole string `json:"actor_role"` // role of whoever executed it
	Action    string `json:"action"`     // one of the Audit* constants
	Target    string `json:"target"`     // target e-mail/resource
	Detail    string `json:"detail"`     // short, readable text, no secrets
	TS        string `json:"ts"`         // RFC3339 (the instant comes in as a parameter)
}

// NewAuditEntry builds the record, normalizing e-mails to lowercase. PURE: no IO,
// no clock (the `ts` instant is injected by the shell).
func NewAuditEntry(org, actor, actorRole, action, target, detail, ts string) AuditEntry {
	return AuditEntry{
		Org:       org,
		Actor:     strings.ToLower(strings.TrimSpace(actor)),
		ActorRole: actorRole,
		Action:    action,
		Target:    strings.ToLower(strings.TrimSpace(target)),
		Detail:    detail,
		TS:        ts,
	}
}
