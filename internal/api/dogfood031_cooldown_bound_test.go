package api_test

// DOGFOOD-031: negative cooldown_s on project create/update must be rejected
// with a 400 that names the field (weight/priority/decay_rate were bounded,
// cooldown_s was not — a negative value made a lane perpetually eligible).
// cooldown_s = 0 stays accepted: on update it is stored verbatim and means
// "no cooldown"; on create it takes the 900 default.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

func TestDOGFOOD031_CooldownBound(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name       string
		cooldownS  int
		wantStatus int
		wantMsgHas string // empty = no message assertion
	}{
		{
			name:       "negative -1 rejected with 400 naming the field",
			cooldownS:  -1,
			wantStatus: http.StatusBadRequest,
			wantMsgHas: "cooldown_s",
		},
		{
			name:       "zero accepted — 0 means no cooldown",
			cooldownS:  0,
			wantStatus: 0, // filled per-route below (201 create / 200 update)
		},
		{
			name:       "positive value accepted",
			cooldownS:  600,
			wantStatus: 0, // filled per-route below
		},
	}

	t.Run("create POST /api/v1/projects", func(t *testing.T) {
		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				a := newAPITestServer(t)
				name := "dogfood031-create"
				wantStatus := tc.wantStatus
				if wantStatus == 0 {
					wantStatus = http.StatusCreated
				}
				status, body := a.do(t, "POST", "/api/v1/projects", map[string]interface{}{
					"name":       name,
					"repo_url":   "https://example.com/" + name,
					"workdir":    "/tmp/" + name,
					"cooldown_s": tc.cooldownS,
				})
				if status != wantStatus {
					t.Fatalf("POST cooldown_s=%d: status = %d, want %d (body %v)",
						tc.cooldownS, status, wantStatus, body)
				}
				if tc.wantMsgHas != "" {
					msg, _ := body["error"].(string)
					if !strings.Contains(msg, tc.wantMsgHas) {
						t.Errorf("error message must name the field %q, got %q", tc.wantMsgHas, msg)
					}
					if !strings.Contains(msg, "invalid project fields:") {
						t.Errorf("error message must use the 'invalid project fields:' style, got %q", msg)
					}
					// No row may have been written by the refused create.
					if _, err := database.GetProject(ctx, a.db, name); err == nil {
						t.Errorf("project %q was created despite the negative cooldown_s", name)
					}
				}
			})
		}
	})

	t.Run("update PUT /api/v1/projects/{name}", func(t *testing.T) {
		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				a := newAPITestServer(t)
				name := "dogfood031-update"
				status, _ := a.do(t, "POST", "/api/v1/projects", map[string]interface{}{
					"name":     name,
					"repo_url": "https://example.com/" + name,
					"workdir":  "/tmp/" + name,
				})
				if status != http.StatusCreated {
					t.Fatalf("seed create: status = %d, want 201", status)
				}

				wantStatus := tc.wantStatus
				if wantStatus == 0 {
					wantStatus = http.StatusOK
				}
				status, body := a.do(t, "PUT", "/api/v1/projects/"+name, map[string]interface{}{
					"cooldown_s": tc.cooldownS,
				})
				if status != wantStatus {
					t.Fatalf("PUT cooldown_s=%d: status = %d, want %d (body %v)",
						tc.cooldownS, status, wantStatus, body)
				}
				if tc.wantMsgHas != "" {
					msg, _ := body["error"].(string)
					if !strings.Contains(msg, tc.wantMsgHas) {
						t.Errorf("error message must name the field %q, got %q", tc.wantMsgHas, msg)
					}
					if !strings.Contains(msg, "invalid project fields:") {
						t.Errorf("error message must use the 'invalid project fields:' style, got %q", msg)
					}
					return
				}

				// Acceptance arm: the stored row must carry the value verbatim
				// (0 = no cooldown is a real stored state, not a default).
				p, err := database.GetProject(ctx, a.db, name)
				if err != nil {
					t.Fatalf("GetProject: %v", err)
				}
				if p.CooldownS != tc.cooldownS {
					t.Errorf("stored cooldown_s = %d, want %d (0 must be honored as 'no cooldown', not re-defaulted)", p.CooldownS, tc.cooldownS)
				}
			})
		}
	})

	t.Run("create control zero takes the 900 default", func(t *testing.T) {
		// The create path defaults an omitted/zero cooldown_s to 900 — a zero
		// body must stay accepted (201), not 400, and not stored as 0.
		a := newAPITestServer(t)
		status, body := a.do(t, "POST", "/api/v1/projects", map[string]interface{}{
			"name":       "dogfood031-create-default",
			"repo_url":   "https://example.com/dogfood031-create-default",
			"workdir":    "/tmp/dogfood031-create-default",
			"cooldown_s": 0,
		})
		if status != http.StatusCreated {
			t.Fatalf("POST cooldown_s=0: status = %d, want 201 (body %v)", status, body)
		}
		p, err := database.GetProject(ctx, a.db, "dogfood031-create-default")
		if err != nil {
			t.Fatalf("GetProject: %v", err)
		}
		if p.CooldownS != 900 {
			t.Errorf("stored cooldown_s = %d, want 900 (create default)", p.CooldownS)
		}
	})
}
