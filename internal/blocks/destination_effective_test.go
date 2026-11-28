package blocks

import (
	"encoding/json"
	"strings"
	"testing"
)

// SCHED-GAP-17084: DecideDelivery is the delivery-time sibling of
// InventoryDestinations. These tests pin its rules, its parity with the
// inventory vocabulary, and the promise that a decision never carries a
// destination into JSON.

const (
	deliveryGroupTarget     = "telegram:-100888:31"
	deliveryExceptionTarget = "telegram:-100888:32"
	deliveryForeignTarget   = "telegram:-100888:33"
)

func TestDecideDeliveryRules(t *testing.T) {
	approved := func(name, destination string, members ...string) Group {
		return Group{Name: name, Projects: members, DestinationPolicy: &DestinationPolicy{Destination: destination, Approved: true}}
	}
	withException := func(exception DestinationException) Group {
		g := approved("alpha", deliveryGroupTarget, "lane")
		g.DestinationPolicy.Exceptions = map[string]DestinationException{"lane": exception}
		return g
	}

	cases := []struct {
		name      string
		groups    []Group
		current   string
		status    string
		allowed   bool
		effective string
		origin    string
	}{
		{
			name: "aligned", groups: []Group{approved("alpha", deliveryGroupTarget, "lane")},
			current: deliveryGroupTarget, status: DestinationAligned, allowed: true,
			effective: deliveryGroupTarget, origin: "alpha",
		},
		{
			name: "inherited", groups: []Group{approved("alpha", deliveryGroupTarget, "lane")},
			current: "", status: DestinationInherited, allowed: true,
			effective: deliveryGroupTarget, origin: "alpha",
		},
		{
			name: "drift", groups: []Group{approved("alpha", deliveryGroupTarget, "lane")},
			current: deliveryForeignTarget, status: DestinationDrift, allowed: false,
			effective: "", origin: "alpha",
		},
		{
			name: "approved exception", groups: []Group{withException(DestinationException{Destination: deliveryExceptionTarget, Approved: true})},
			current: deliveryExceptionTarget, status: DestinationAligned, allowed: true,
			effective: deliveryExceptionTarget, origin: "alpha:exception",
		},
		{
			name: "unapproved exception", groups: []Group{withException(DestinationException{Destination: deliveryExceptionTarget})},
			current: deliveryExceptionTarget, status: DestinationUnapproved, allowed: false,
			effective: "", origin: "alpha",
		},
		{
			name:    "approved exception with an invalid target",
			groups:  []Group{withException(DestinationException{Destination: "nonsense", Approved: true})},
			current: deliveryExceptionTarget, status: DestinationInvalid, allowed: false,
			effective: "", origin: "alpha",
		},
		{
			name: "conflicting authorized ownership",
			groups: []Group{
				approved("alpha", deliveryGroupTarget, "lane"),
				approved("beta", deliveryExceptionTarget, "lane"),
			},
			current: deliveryGroupTarget, status: DestinationConflict, allowed: false,
			effective: "", origin: "alpha,beta",
		},
		{
			name: "conflicting ownership without an approved policy is not authority",
			groups: []Group{
				approved("alpha", deliveryGroupTarget, "lane"),
				{Name: "beta", Projects: []string{"lane"}},
			},
			current: deliveryGroupTarget, status: DestinationConflict, allowed: false,
			effective: "", origin: "alpha,beta",
		},
		{
			name: "two policy-less owners leave delivery alone",
			groups: []Group{
				{Name: "alpha", Projects: []string{"lane"}},
				{Name: "beta", Projects: []string{"lane"}},
			},
			current: deliveryForeignTarget, status: DestinationUnknown, allowed: true,
			effective: deliveryForeignTarget, origin: "alpha,beta",
		},
		{
			name: "unapproved policy is not authority", groups: []Group{
				{Name: "alpha", Projects: []string{"lane"}, DestinationPolicy: &DestinationPolicy{Destination: deliveryGroupTarget}},
			},
			current: deliveryForeignTarget, status: DestinationUnknown, allowed: true,
			effective: deliveryForeignTarget, origin: "alpha",
		},
		{
			name: "no group at all", groups: []Group{approved("alpha", deliveryGroupTarget, "other")},
			current: deliveryForeignTarget, status: DestinationUnknown, allowed: true,
			effective: deliveryForeignTarget, origin: "",
		},
		{
			name: "invalid approved destination", groups: []Group{approved("alpha", "telegram:-100888", "lane")},
			current: deliveryForeignTarget, status: DestinationInvalid, allowed: false,
			effective: "", origin: "alpha",
		},
		{
			name: "empty approved destination", groups: []Group{approved("alpha", "", "lane")},
			current: "", status: DestinationInvalid, allowed: false,
			effective: "", origin: "alpha",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideDelivery(tc.groups, "lane", tc.current)
			if got.Status != tc.status || got.Allowed != tc.allowed || got.Effective != tc.effective || got.Origin != tc.origin {
				t.Fatalf("DecideDelivery = %+v, want status=%q allowed=%v effective=%q origin=%q",
					got, tc.status, tc.allowed, tc.effective, tc.origin)
			}
			// A decision is handed to logs and (potentially) HTTP: it must
			// never carry a destination.
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "telegram") || strings.Contains(string(encoded), "100888") {
				t.Fatalf("decision JSON leaked a destination: %s", encoded)
			}
		})
	}
}

// TestDecideDeliveryMatchesInventoryVocabulary pins the audit twin: for a
// project the inventory classifies, the delivery decision must tell the same
// story (with "unset" ↔ "inherited" being the same state seen from either
// side) and must allow delivery exactly for aligned/unset/unknown.
func TestDecideDeliveryMatchesInventoryVocabulary(t *testing.T) {
	groups := []Group{
		{Name: "alpha", Projects: []string{"aligned", "wrong-thread", "exception", "empty", "conflict"}, DestinationPolicy: &DestinationPolicy{
			Destination: deliveryGroupTarget, Approved: true,
			Exceptions: map[string]DestinationException{"exception": {Destination: deliveryExceptionTarget, Approved: true}},
		}},
		{Name: "beta", Projects: []string{"conflict"}},
	}
	projects := []DestinationProject{
		{Name: "aligned", Destination: deliveryGroupTarget},
		{Name: "wrong-thread", Destination: deliveryExceptionTarget},
		{Name: "exception", Destination: deliveryExceptionTarget},
		{Name: "empty"},
		{Name: "conflict", Destination: deliveryGroupTarget},
		{Name: "unknown", Destination: deliveryForeignTarget},
	}
	policies := map[string]DestinationPolicy{"alpha": *groups[0].DestinationPolicy}

	inventory := map[string]string{}
	for _, item := range InventoryDestinations(groups, policies, projects) {
		inventory[item.Project] = item.Status
	}

	want := map[string]struct {
		decision string
		allowed  bool
	}{
		"aligned":      {DestinationAligned, true},
		"wrong-thread": {DestinationDrift, false},
		"exception":    {DestinationAligned, true},
		"empty":        {DestinationInherited, true},
		"conflict":     {DestinationConflict, false},
		"unknown":      {DestinationUnknown, true},
	}
	for _, project := range projects {
		got := DecideDelivery(groups, project.Name, project.Destination)
		expected := want[project.Name]
		if got.Status != expected.decision || got.Allowed != expected.allowed {
			t.Fatalf("%s: decision = %+v, want %+v (inventory said %q)", project.Name, got, expected, inventory[project.Name])
		}
	}
	// The inventory must agree on every status it can express; only its
	// "unset" spelling differs from the delivery-side "inherited".
	inventoryToDecision := map[string]string{
		"aligned": DestinationAligned, "unset": DestinationInherited, "drift": DestinationDrift,
		"conflict": DestinationConflict, "unknown": DestinationUnknown, "invalid": DestinationInvalid,
	}
	for project, status := range inventory {
		got := DecideDelivery(groups, project, destinationOf(projects, project))
		if mapped, ok := inventoryToDecision[status]; !ok || mapped != got.Status {
			t.Fatalf("%s: inventory=%q maps to %q but decision=%q", project, status, mapped, got.Status)
		}
	}
}

func destinationOf(projects []DestinationProject, name string) string {
	for _, project := range projects {
		if project.Name == name {
			return project.Destination
		}
	}
	return ""
}

// TestDecideDeliveryDoesNotMutateInputs guards the read-only promise the API
// preview depends on.
func TestDecideDeliveryDoesNotMutateInputs(t *testing.T) {
	groups := []Group{{
		Name:     "alpha",
		Projects: []string{"lane", "other"},
		DestinationPolicy: &DestinationPolicy{
			Destination: deliveryGroupTarget,
			Approved:    true,
			Exceptions:  map[string]DestinationException{"lane": {Destination: deliveryExceptionTarget}},
		},
	}}
	before, err := json.Marshal(groups)
	if err != nil {
		t.Fatal(err)
	}
	_ = DecideDelivery(groups, "lane", deliveryForeignTarget)
	_ = DecideDelivery(groups, "absent", deliveryForeignTarget)
	after, err := json.Marshal(groups)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("DecideDelivery mutated its input:\nbefore=%s\nafter=%s", before, after)
	}
}
