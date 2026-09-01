// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package auditcore is the PURE DOMAIN of Audit: it builds the audit record,
// computes the diff, redacts sensitive values, decides retention and decides who may
// read. Nothing here reaches an SDK, the network, a file, the environment, the clock
// or randomness — the instant and the event identifier always come in as parameters.
// That is what makes this logic testable offline and independent of Lambda.
//
// Boundary rule (verified by boundary_test.go): the transitive closure of imports is
// restricted to pure stdlib, and no import of another platform domain.
package auditcore

// Audit categories. They map 1:1 to the audit sub-tabs inside the Console's Logs tab
// — but the category is domain data, not screen data: it is what indexes the
// by_category LSI, so changing this set changes the store's access pattern.
//
// CatAccount is kept as a valid, filterable category even though no action maps to
// it today: org creation and plan-change events existed only in the multi-tenant
// SaaS signup/billing flow, both removed for the single-client deployment model.
// The category stays because it is a stable index value (not code that runs), and
// a future account-lifecycle action (e.g. deployment renamed) has somewhere to land
// without touching the LSI shape.
const (
	CatConfig  = "config"
	CatModels  = "models"
	CatMembers = "members"
	CatKeys    = "keys"
	CatAccount = "account"
)

// Auditable actions. Constants so magic strings do not spread across the shells and
// the vocabulary stays stable in the trail — a record is immutable, so a renamed
// action would leave history with two names for the same thing.
//
// The seven member actions repeat EXACTLY the values already defined in
// govcore.Audit* in Governance. The repetition is deliberate: a domain does not import
// a domain (that is verified by a test), and coherence between the two is guaranteed
// by the shared action-catalog.json fixture, not by a common library.
const (
	// config
	ActionConfigUpdate = "config_update"
	ActionCreditUpdate = "credit_update"
	ActionCreditDelete = "credit_delete"

	// models
	ActionModelAdd          = "model_add"
	ActionModelUpdate       = "model_update"
	ActionModelRemove       = "model_remove"
	ActionModelEnable       = "model_enable"
	ActionModelDisable      = "model_disable"
	ActionProviderSecretSet = "provider_secret_set"
	// Bundle and swap policy. They are category `models` because whoever audits "what
	// changed in routing" looks at the same sub-tab as the models; the swap policy in
	// particular changes what the customer RECEIVES without changing their code — hence
	// the need for actor, instant and previous value.
	ActionBundleAdd        = "bundle_add"
	ActionBundleUpdate     = "bundle_update"
	ActionBundleRemove     = "bundle_remove"
	ActionSwapPolicyUpdate = "swap_policy_update"

	// members (the first 7 mirror govcore.Audit*)
	ActionMemberInvite  = "member_invite"
	ActionMemberUpdate  = "member_update"
	ActionMemberRemove  = "member_remove"
	ActionMemberEnable  = "member_enable"
	ActionMemberDisable = "member_disable"
	ActionPasswordReset = "password_reset"
	ActionInviteResend  = "invite_resend"
	ActionTeamCreate    = "team_create"
	ActionTeamRename    = "team_rename"
	ActionTeamArchive   = "team_archive"
	ActionTeamRemove    = "team_remove"
	ActionAppCreate     = "app_create"
	ActionAppRename     = "app_rename"
	ActionAppArchive    = "app_archive"
	ActionAppRemove     = "app_remove"

	// keys
	ActionKeyIssue  = "key_issue"
	ActionKeyRevoke = "key_revoke"
)

// catalog maps each action to EXACTLY one category. It is the source of truth of the
// vocabulary; the shared fixture is generated/validated against this map.
var catalog = map[string]string{
	ActionConfigUpdate: CatConfig,
	ActionCreditUpdate: CatConfig,
	ActionCreditDelete: CatConfig,

	ActionModelAdd:          CatModels,
	ActionModelUpdate:       CatModels,
	ActionModelRemove:       CatModels,
	ActionModelEnable:       CatModels,
	ActionModelDisable:      CatModels,
	ActionProviderSecretSet: CatModels,
	ActionBundleAdd:         CatModels,
	ActionBundleUpdate:      CatModels,
	ActionBundleRemove:      CatModels,
	ActionSwapPolicyUpdate:  CatModels,

	ActionMemberInvite:  CatMembers,
	ActionMemberUpdate:  CatMembers,
	ActionMemberRemove:  CatMembers,
	ActionMemberEnable:  CatMembers,
	ActionMemberDisable: CatMembers,
	ActionPasswordReset: CatMembers,
	ActionInviteResend:  CatMembers,
	ActionTeamCreate:    CatMembers,
	ActionTeamRename:    CatMembers,
	ActionTeamArchive:   CatMembers,
	ActionTeamRemove:    CatMembers,
	ActionAppCreate:     CatMembers,
	ActionAppRename:     CatMembers,
	ActionAppArchive:    CatMembers,
	ActionAppRemove:     CatMembers,

	ActionKeyIssue:  CatKeys,
	ActionKeyRevoke: CatKeys,
}

// CategoryOf returns the action's category. The second return is false for an action
// OUTSIDE the catalog — and in that case the shell must persist the record anyway,
// only logging a warning. Dropping it would be worse: during a partial deploy the
// emitter may be newer than the writer, and losing an audit record is irreversible,
// whereas a label the UI does not yet know is cosmetic.
func CategoryOf(action string) (string, bool) {
	c, ok := catalog[action]
	return c, ok
}

// Categories returns the valid categories, so the API can validate a filter without
// duplicating the list.
func Categories() []string {
	return []string{CatConfig, CatModels, CatMembers, CatKeys, CatAccount}
}

// ValidCategory reports whether the string is a known category.
func ValidCategory(c string) bool {
	for _, x := range Categories() {
		if x == c {
			return true
		}
	}
	return false
}

// Catalog returns a COPY of the action→category map. A copy (and not the map) so no
// caller can mutate the vocabulary at runtime.
func Catalog() map[string]string {
	out := make(map[string]string, len(catalog))
	for k, v := range catalog {
		out[k] = v
	}
	return out
}
