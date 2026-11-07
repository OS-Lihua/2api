package authz

const ResourceModel = "model"

var (
	ModelRead  = Permission{Resource: ResourceModel, Action: ActionRead}
	ModelWrite = Permission{Resource: ResourceModel, Action: ActionWrite}
)

func init() {
	RegisterResource(ResourceDefinition{
		Resource: ResourceModel,
		LabelKey: "Models",
		Actions: []ActionDefinition{
			{Action: ActionRead, LabelKey: "View", DescriptionKey: "View models and vendors, and preview upstream sync.", DefaultRoles: []string{BuiltInRoleAdmin}},
			{Action: ActionWrite, LabelKey: "Edit", DescriptionKey: "Edit models and vendors, and sync them from upstream.", DefaultRoles: []string{BuiltInRoleAdmin}},
		},
	})
}
