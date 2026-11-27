package blocks

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestDestinationInventory(t *testing.T) {
	const groupTarget = "telegram:-100123:11"
	const exceptionTarget = "telegram:-100123:22"
	groups := []Group{
		{Name: "alpha", Projects: []string{"aligned", "wrong-thread", "exception", "pending-exception", "empty", "conflict", "aligned"}},
		{Name: "beta", Projects: []string{"conflict", "unapproved"}},
		{Name: "bad", Projects: []string{"invalid"}},
	}
	policies := map[string]DestinationPolicy{
		"alpha": {Destination: groupTarget, Approved: true, Exceptions: map[string]DestinationException{
			"exception":         {Destination: exceptionTarget, Approved: true},
			"pending-exception": {Destination: exceptionTarget},
		}},
		"beta": {Destination: groupTarget},
		"bad":  {Destination: "telegram:-100123", Approved: true},
	}
	projects := []DestinationProject{
		{Name: "aligned", NamespaceID: "first", Destination: groupTarget},
		{Name: "wrong-thread", NamespaceID: "second", Destination: exceptionTarget},
		{Name: "exception", NamespaceID: "second", Destination: exceptionTarget},
		{Name: "pending-exception", Destination: groupTarget},
		{Name: "empty"}, {Name: "conflict", Destination: groupTarget},
		{Name: "unapproved", Destination: groupTarget}, {Name: "invalid"},
		{Name: "unknown", Destination: groupTarget},
	}
	before, err := json.Marshal([]any{groups, policies, projects})
	if err != nil {
		t.Fatal(err)
	}
	got := InventoryDestinations(groups, policies, projects)
	want := []DestinationInventoryItem{
		{Project: "aligned", NamespaceID: "first", Origin: "alpha", Status: "aligned"},
		{Project: "conflict", Status: "conflict"},
		{Project: "empty", Origin: "alpha", Status: "unset"},
		{Project: "exception", NamespaceID: "second", Origin: "alpha:exception", Status: "aligned"},
		{Project: "invalid", Origin: "bad", Status: "invalid"},
		{Project: "pending-exception", Origin: "alpha", Status: "unknown"},
		{Project: "unapproved", Origin: "beta", Status: "unknown"},
		{Project: "unknown", Status: "unknown"},
		{Project: "wrong-thread", NamespaceID: "second", Origin: "alpha", Status: "drift"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("inventory = %#v, want %#v", got, want)
	}
	after, err := json.Marshal([]any{groups, policies, projects})
	if err != nil || string(before) != string(after) {
		t.Fatalf("inventory changed its inputs: %v", err)
	}
	output, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(output), "-100123") || strings.Contains(string(output), "telegram:") {
		t.Fatal("inventory exposed a destination")
	}
}

func TestDestinationInventoryRejectsInvalidTargets(t *testing.T) {
	for _, target := range []string{"", "telegram:123", "telegram:123:", "telegram:123:0", "telegram:0:1", "telegram:+123:1", "telegram:0123:1", "telegram:123:01", "telegram:123:-1", "other:123:1", "telegram:123:1:2"} {
		t.Run(target, func(t *testing.T) {
			got := InventoryDestinations([]Group{{Name: "g", Projects: []string{"p"}}}, map[string]DestinationPolicy{"g": {Approved: true, Destination: target}}, []DestinationProject{{Name: "p"}})
			if len(got) != 1 || got[0].Status != "invalid" {
				t.Fatalf("got %#v", got)
			}
		})
	}
}
