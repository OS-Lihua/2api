package authz

const ResourceKeyPermission = "key_permission"

var (
	KeyPermissionRead  = Permission{Resource: ResourceKeyPermission, Action: ActionRead}
	KeyPermissionWrite = Permission{Resource: ResourceKeyPermission, Action: ActionWrite}
)

func init() {
	RegisterResource(ResourceDefinition{Resource: ResourceKeyPermission, LabelKey: "Key permissions", Actions: []ActionDefinition{
		{Action: ActionRead, LabelKey: "View", DescriptionKey: "View Key permissions.", DefaultRoles: []string{BuiltInRoleAdmin}},
		{Action: ActionWrite, LabelKey: "Edit", DescriptionKey: "Edit Key permissions.", DefaultRoles: []string{BuiltInRoleAdmin}},
	}})
}
