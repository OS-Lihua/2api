package authz

const ResourceRecommendation = "recommendation"

var (
	RecommendationRead  = Permission{Resource: ResourceRecommendation, Action: ActionRead}
	RecommendationWrite = Permission{Resource: ResourceRecommendation, Action: ActionWrite}
)

func init() {
	RegisterResource(ResourceDefinition{
		Resource: ResourceRecommendation, LabelKey: "Recommendation rules",
		Actions: []ActionDefinition{
			{Action: ActionRead, LabelKey: "Read recommendation rules", DescriptionKey: "View rules and query recommendations.", DefaultRoles: []string{BuiltInRoleAdmin}},
			{Action: ActionWrite, LabelKey: "Edit recommendation rules", DescriptionKey: "Create, edit, and delete recommendation rules.", DefaultRoles: []string{BuiltInRoleAdmin}},
		},
	})
}
