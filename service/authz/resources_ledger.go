package authz

// ResourceLedger covers wallets and their ledger entries in the admin API.
// It is distinct from the "wallet" access token scope, which is a user's own
// top-up page.
const ResourceLedger = "ledger"

var (
	LedgerRead  = Permission{Resource: ResourceLedger, Action: ActionRead}
	LedgerWrite = Permission{Resource: ResourceLedger, Action: ActionWrite}
)

func init() {
	RegisterResource(ResourceDefinition{
		Resource: ResourceLedger,
		LabelKey: "Wallets and ledger",
		Actions: []ActionDefinition{
			{
				Action:         ActionRead,
				LabelKey:       "Read wallets",
				DescriptionKey: "View user wallet balances and ledger entries.",
				DefaultRoles:   []string{BuiltInRoleAdmin},
			},
			{
				// No default role: only root changes balances until it grants
				// this action.
				Action:         ActionWrite,
				LabelKey:       "Change wallet balances",
				DescriptionKey: "Credit top-ups, adjust balances, and reverse ledger entries.",
			},
		},
	})
}
