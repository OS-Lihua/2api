package authz

const ResourcePricing = "pricing"

var (
	PricingRead  = Permission{Resource: ResourcePricing, Action: ActionRead}
	PricingWrite = Permission{Resource: ResourcePricing, Action: ActionWrite}
)

func init() {
	RegisterResource(ResourceDefinition{
		Resource: ResourcePricing,
		LabelKey: "Prices and exchange rates",
		Actions: []ActionDefinition{
			{Action: ActionRead, LabelKey: "Read pricing", DescriptionKey: "View price versions and exchange rates.", DefaultRoles: []string{BuiltInRoleAdmin}},
			{Action: ActionWrite, LabelKey: "Manage pricing", DescriptionKey: "Create price versions and maintain exchange rates.", DefaultRoles: []string{BuiltInRoleAdmin}},
		},
	})
}
