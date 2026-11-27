package blocks

import "sort"

// DestinationApproval approves one specific transition, not a group-wide rewrite.
// Destination values are deliberately excluded from JSON output.
type DestinationApproval struct {
	Project string `json:"project"`
	From    string `json:"-"`
	To      string `json:"-"`
}

// DestinationPlanItem reports a non-mutating migration decision without
// disclosing either destination. Ready items require a fresh check at apply time.
type DestinationPlanItem struct {
	Project string `json:"project"`
	Origin  string `json:"origin,omitempty"`
	Status  string `json:"status"`
}

// PlanDestinationMigration previews individually approved transitions. A
// conflicting membership, unknown policy, unapproved exception, invalid target,
// duplicate approval or approval for stale values can never become ready.
// This function does not persist or apply the result.
// ch:trace row=SCHED-GAP-17083
func PlanDestinationMigration(groups []Group, policies map[string]DestinationPolicy, projects []DestinationProject, approvals []DestinationApproval) []DestinationPlanItem {
	inventory := InventoryDestinations(groups, policies, projects)
	current := make(map[string]DestinationProject, len(projects))
	duplicates := make(map[string]bool)
	for _, project := range projects {
		if _, exists := current[project.Name]; exists {
			duplicates[project.Name] = true
		}
		current[project.Name] = project
	}
	approved := make(map[string]DestinationApproval, len(approvals))
	for _, approval := range approvals {
		if _, exists := approved[approval.Project]; exists {
			duplicates[approval.Project] = true
		}
		approved[approval.Project] = approval
	}
	result := make([]DestinationPlanItem, 0, len(inventory))
	for _, item := range inventory {
		decision := DestinationPlanItem{Project: item.Project, Origin: item.Origin, Status: item.Status}
		if duplicates[item.Project] {
			decision.Status = "ambiguous"
		} else if item.Status == "drift" || item.Status == "unset" {
			decision.Status = "approval_required"
			approval, ok := approved[item.Project]
			if ok {
				var target string
				for _, group := range groups {
					for _, member := range group.Projects {
						if member != item.Project {
							continue
						}
						policy := policies[group.Name]
						target = policy.Destination
						if exception, exists := policy.Exceptions[item.Project]; exists {
							target = exception.Destination
						}
					}
				}
				if approval.From == current[item.Project].Destination && approval.To == target {
					decision.Status = "ready"
				} else {
					decision.Status = "stale_approval"
				}
			}
		}
		result = append(result, decision)
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Project < result[j].Project })
	return result
}
