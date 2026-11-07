package authz

const ResourceRequest = "request"

var RequestRead = Permission{Resource: ResourceRequest, Action: ActionRead}

func init() {
	RegisterResource(ResourceDefinition{
		Resource: ResourceRequest,
		LabelKey: "Requests",
		Actions:  []ActionDefinition{{Action: ActionRead, LabelKey: "Read requests", DescriptionKey: "View requests and attempts.", DefaultRoles: []string{BuiltInRoleAdmin}}},
	})
}
