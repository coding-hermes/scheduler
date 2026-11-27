package blocks

import (
	"sort"
	"strconv"
	"strings"
)

// DestinationPolicy is an explicitly approved candidate for one deploy group.
// This value is only an input to the read-only inventory; it does not enable
// delivery or persist a policy. An exception must itself be approved.
type DestinationPolicy struct {
	Destination string
	Approved    bool
	Exceptions  map[string]DestinationException
}

// DestinationException is a project-specific candidate destination.
type DestinationException struct {
	Destination string
	Approved    bool
}

// DestinationProject is the minimal project data needed by the inventory.
type DestinationProject struct {
	Name        string
	NamespaceID string
	Destination string
}

// DestinationInventoryItem deliberately contains no destination identifiers.
// Status is unknown, conflict, invalid, unset, aligned, or drift. Origin names
// the owning group and, for an approved exception, has an exception suffix.
type DestinationInventoryItem struct {
	Project     string `json:"project"`
	NamespaceID string `json:"namespace_id"`
	Origin      string `json:"origin,omitempty"`
	Status      string `json:"status"`
}

// validTelegramDestination requires an explicit chat and thread; it never
// guesses a missing thread or normalizes a supplied identifier.
func validTelegramDestination(destination string) bool {
	parts := strings.Split(destination, ":")
	if len(parts) != 3 || parts[0] != "telegram" {
		return false
	}
	chat, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || chat == 0 || strconv.FormatInt(chat, 10) != parts[1] {
		return false
	}
	thread, err := strconv.ParseInt(parts[2], 10, 64)
	return err == nil && thread > 0 && strconv.FormatInt(thread, 10) == parts[2]
}

// InventoryDestinations compares explicit candidate policies against current
// project values without modifying either store. Multiple memberships are
// conflicts even when their destinations agree: agreement does not establish
// ownership. Unknown, unapproved and invalid policies never propose a route.
// ch:trace row=SCHED-GAP-17083
func InventoryDestinations(groups []Group, policies map[string]DestinationPolicy, projects []DestinationProject) []DestinationInventoryItem {
	memberships := make(map[string]map[string]struct{})
	for _, group := range groups {
		for _, project := range group.Projects {
			if memberships[project] == nil {
				memberships[project] = make(map[string]struct{})
			}
			memberships[project][group.Name] = struct{}{}
		}
	}
	result := make([]DestinationInventoryItem, 0, len(projects))
	for _, project := range projects {
		item := DestinationInventoryItem{Project: project.Name, NamespaceID: project.NamespaceID, Status: "unknown"}
		owners := memberships[project.Name]
		if len(owners) > 1 {
			item.Status = "conflict"
		} else if len(owners) == 1 {
			for owner := range owners {
				item.Origin = owner
				policy, ok := policies[owner]
				if !ok || !policy.Approved {
					break
				}
				destination := policy.Destination
				if exception, exists := policy.Exceptions[project.Name]; exists {
					if !exception.Approved {
						break
					}
					destination = exception.Destination
					item.Origin += ":exception"
				}
				if !validTelegramDestination(destination) {
					item.Status = "invalid"
				} else if project.Destination == "" {
					item.Status = "unset"
				} else if project.Destination == destination {
					item.Status = "aligned"
				} else {
					item.Status = "drift"
				}
			}
		}
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Project < result[j].Project })
	return result
}
