package dashboard

// SCHED-GAP-1601 — the operator control console: drive the scheduler's EXISTING
// mutating REST API from the web UI.
//
// DESIGN CONTRACT (decided, in writing — mirrored in the commit body):
//
//   - The console adds NO new mutation of its own. Every control maps 1:1 to
//     an operation the daemon already serves on /api/v1/* (the same 22 the
//     CTL-003 parity guard pins to MCP tools). No invented verbs, no second
//     confirm convention, no second audit path. The operation LIST is derived
//     from the API's own served /api/v1/openapi.json at test time
//     (control_parity_test.go), so the console cannot name an operation the
//     API does not have, and a rename in the API breaks this test loudly.
//
//   - AUTH: the browser path is HTTP basic (auth mode "basic",
//     SCHED-GAP-1602): the daemon answers 401 + WWW-Authenticate on a
//     mutating route and the browser prompts natively. The console NEVER
//     stores, types or forwards the credential — no token field, no
//     localStorage/sessionStorage/cookie. Same-origin XHR to
//     /dashboard/control carries the browser-cached basic credentials
//     automatically; the credential lives only in the 0600 env file the
//     daemon reads. Fail-closed: if the daemon has no credential configured
//     the API answers 503 and the console renders that refusal verbatim.
//     The console also works with token mode (X-Operator-Token via curl);
//     the browser path then degrades to 401 — the operator must use basic
//     mode for UI-driven control. That is the auth row's intended shape.
//
//   - PROXY, not builder: the browser never talks to /api/v1 directly for
//     controls. It posts a small instruction (action + target + confirm/
//     reason) to POST /dashboard/control, and the proxy forwards to the API
//     server IN-PROCESS (the very handler main.go mounted at /api/), returning
//     the API's OWN status code, body and error message verbatim — success
//     AND failure. No silent success: a 409/400/503 refusal is rendered with
//     the API's message, never as a green spinner.
//
//   - PURITY: the proxy performs NO substitution of its own. The target
//     (lane name / namespace id / group / template) always comes from the
//     client instruction; the proxy validates it is non-empty and
//     path-safe and substitutes it into the action's path template. There
//     is deliberately no "current project" fallback — an action whose path
//     needs a target is refused (400) without one, not guessed.
//
//   - CONFIRMATION: destructive or fleet-wide actions (every delete, global
//     pause, deploy) require confirm=true on the proxy instruction, which
//     the console's JS only sends after a modal dialog that NAMES the
//     target. The proxy reuses the API's own confirm=true query convention
//     (the same flag DELETE /api/v1/projects/{name} requires) — the console
//     invents no second scheme. Soft-delete only: the console never sends
//     purge=true — a permanent row removal has no place behind a single
//     modal click; it stays an explicit operator API/curl act.
//
//   - LONG-RUNNING: spawn (202 Accepted, tick enqueued) and deploy (per-
//     project board writes) are surfaced with their REAL result shape — the
//     UI says "accepted (tick_id …)" / renders the per-project deploy
//     results rather than claiming the work completed. A deploy dry-run
//     (the API's own dry_run=true) writes nothing, so it is exempt from
//     the confirmation gate.
//
//   - AUDIT: every proxied call goes through the API's mutation gate
//     (requireOperator), so every action — allowed or refused — writes the
//     existing api.auth events row (SCHED-GAP-1602). The console adds zero
//     audit code because the API is the audit point.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/blocks"
)

// ControlAction is one console control bound 1:1 to an API operation. The
// Path carries ONE %s slot for the target (lane name, namespace id, group or
// template name); global and create actions have no slot.
type ControlAction struct {
	ID     string // the ?action= value on POST /dashboard/control
	Method string // http.Method* forwarded to the API
	Path   string // API path template; %s = client-supplied target
	// Confirm marks destructive/fleet-wide operations: the console JS must
	// collect an explicit confirmation NAMING the target and send
	// confirm=true; the proxy refuses them without it (400) so a stray
	// click can never reach the API.
	Confirm bool
	// FleetWide marks operations whose blast radius is the whole scheduler.
	FleetWide bool
	// LongRunning marks operations whose success is an ACCEPTANCE (work
	// enqueued / planned), not a completed state change — the UI reports
	// them as accepted, never as done.
	LongRunning bool
	// Label / DoneLabel are the UI verb phrases ("Pause", "paused").
	Label     string
	DoneLabel string
}

// controlActions is THE registry: every console action, each bound to the
// exact API operation (method + path from openapi.json) it forwards to. The
// rendering slices below list which of these appear on which page; the
// parity test pins the registry to the LIVE API spec.
var controlActions = map[string]ControlAction{
	// ── Lane (project) controls — /projects/{name} page + overview row menu ──
	"project_pause": {
		ID: "project_pause", Method: http.MethodPost,
		Path: "/api/v1/projects/%s/pause",
		// Not Confirm: a pause is one click reversible via resume, and the
		// satellite cascade (SCHED-GAP-180) is announced on the button title.
		Label: "Pause", DoneLabel: "paused",
	},
	"project_resume": {
		ID: "project_resume", Method: http.MethodPost,
		Path:  "/api/v1/projects/%s/resume",
		Label: "Resume", DoneLabel: "resumed",
	},
	"project_spawn": {
		ID: "project_spawn", Method: http.MethodPost,
		Path: "/api/v1/projects/%s/spawn", LongRunning: true,
		Label: "Spawn tick", DoneLabel: "tick accepted",
	},
	"project_bump": {
		ID: "project_bump", Method: http.MethodPost,
		Path:  "/api/v1/projects/%s/bump",
		Label: "Bump", DoneLabel: "bump active",
	},
	"project_unbump": {
		ID: "project_unbump", Method: http.MethodPost,
		Path:  "/api/v1/projects/%s/unbump",
		Label: "Unbump", DoneLabel: "bump cleared",
	},
	"project_update": {
		ID: "project_update", Method: http.MethodPut,
		Path:  "/api/v1/projects/%s",
		Label: "Save", DoneLabel: "updated",
	},
	"project_delete": {
		ID: "project_delete", Method: http.MethodDelete,
		Path: "/api/v1/projects/%s", Confirm: true,
		Label: "Delete", DoneLabel: "deleted (enabled=false, row retained)",
	},

	// ── Namespace controls — /namespaces/{id} page + overview table ──
	"namespace_update": {
		ID: "namespace_update", Method: http.MethodPut,
		Path:  "/api/v1/namespaces/%s",
		Label: "Save", DoneLabel: "updated",
	},
	"namespace_move": {
		ID: "namespace_move", Method: http.MethodPost,
		Path:  "/api/v1/namespaces/%s/move",
		Label: "Move lane in", DoneLabel: "lane assigned",
	},
	"namespace_create": {
		ID: "namespace_create", Method: http.MethodPost,
		Path:  "/api/v1/namespaces",
		Label: "Create", DoneLabel: "created",
	},
	"namespace_delete": {
		ID: "namespace_delete", Method: http.MethodDelete,
		Path: "/api/v1/namespaces/%s", Confirm: true,
		Label: "Delete", DoneLabel: "deleted (enabled=false, members unassigned)",
	},

	// ── Global controls — fleet overview page ──
	"global_pause": {
		ID: "global_pause", Method: http.MethodPost,
		Path: "/api/v1/pause", FleetWide: true, Confirm: true,
		Label: "Pause fleet", DoneLabel: "scheduler paused fleet-wide",
	},
	"global_resume": {
		ID: "global_resume", Method: http.MethodPost,
		Path: "/api/v1/resume", FleetWide: true,
		Label: "Resume fleet", DoneLabel: "scheduler resumed",
	},
	"global_evaluate": {
		ID: "global_evaluate", Method: http.MethodPost,
		Path:  "/api/v1/evaluate",
		Label: "Force evaluate", DoneLabel: "evaluation triggered",
	},

	// ── Group + template controls — deploy blocks ──
	"group_create": {
		ID: "group_create", Method: http.MethodPost,
		Path:  "/api/v1/groups",
		Label: "Create", DoneLabel: "created",
	},
	"group_update": {
		ID: "group_update", Method: http.MethodPut,
		Path:  "/api/v1/groups/%s",
		Label: "Save", DoneLabel: "updated",
	},
	"group_delete": {
		ID: "group_delete", Method: http.MethodDelete,
		Path: "/api/v1/groups/%s", Confirm: true,
		Label: "Delete", DoneLabel: "deleted",
	},
	"group_deploy": {
		ID: "group_deploy", Method: http.MethodPost,
		Path: "/api/v1/groups/%s/deploy", Confirm: true, LongRunning: true,
		Label: "Deploy", DoneLabel: "deploy executed",
	},
	"template_create": {
		ID: "template_create", Method: http.MethodPost,
		Path:  "/api/v1/templates",
		Label: "Create", DoneLabel: "created",
	},
	"template_update": {
		ID: "template_update", Method: http.MethodPut,
		Path:  "/api/v1/templates/%s",
		Label: "Save", DoneLabel: "updated",
	},
	"template_delete": {
		ID: "template_delete", Method: http.MethodDelete,
		Path: "/api/v1/templates/%s", Confirm: true,
		Label: "Delete", DoneLabel: "deleted",
	},
}

// laneControlActions is the ordered control set rendered on the lane
// (/projects/{name}) page and in the overview row menu.
var laneControlActions = []string{
	"project_pause", "project_resume", "project_spawn",
	"project_bump", "project_unbump", "project_update", "project_delete",
}

// namespaceControlActions is the ordered control set rendered on the
// /namespaces/{id} page.
var namespaceControlActions = []string{
	"namespace_update", "namespace_move", "namespace_delete",
}

// globalControlActions is the ordered control set rendered in the fleet
// overview's global console strip. namespace_create lives here: creating a
// namespace is a fleet-structure act (no target of its own), and the
// overview is the one page that always renders regardless of drill-down.
var globalControlActions = []string{"global_pause", "global_resume", "global_evaluate", "namespace_create"}

// blockControlActions is the ordered control set rendered on the deploy
// groups/templates pages.
var blockControlActions = []string{
	"group_create", "group_update", "group_delete", "group_deploy",
	"template_create", "template_update", "template_delete",
}

// controlRegistryComplete fails the purity test if a registry entry is not
// reachable from one of the rendering slices (a registered but unrenderable
// action is exactly the "unlisted gap" the brief forbids) or a slice names
// an unregistered action.
func controlRegistryComplete() error {
	seen := map[string]bool{}
	for _, group := range [][]string{laneControlActions, namespaceControlActions, globalControlActions, blockControlActions} {
		for _, id := range group {
			if _, ok := controlActions[id]; !ok {
				return fmt.Errorf("render list names %q which is not in the registry", id)
			}
			if seen[id] {
				return fmt.Errorf("action %q appears in more than one render list", id)
			}
			seen[id] = true
		}
	}
	for id := range controlActions {
		if !seen[id] {
			return fmt.Errorf("registry action %q is not rendered on any surface — name the page it belongs to or remove it", id)
		}
	}
	return nil
}

// substituteTarget fills the action's %s slot with the client-supplied
// target. PURITY: the proxy never fabricates a target — an action whose
// path needs one is refused without it. The value must be path-safe: no
// slash, no whitespace, no query/fragment characters (lane names,
// namespace ids, group and template names are identifier-shaped;
// blocks.ValidateGroup enforces the same shape for groups).
func substituteTarget(pathTemplate, target string) (string, error) {
	if !strings.Contains(pathTemplate, "%s") {
		// Global/create action: the target (if any) does not ride the path.
		return pathTemplate, nil
	}
	if strings.TrimSpace(target) == "" {
		return "", errors.New("target is required for this action")
	}
	if strings.ContainsAny(target, "/ \t\r\n?#%") {
		return "", errors.New("target must be a bare identifier (no slash, whitespace or query characters)")
	}
	return strings.Replace(pathTemplate, "%s", url.PathEscape(target), 1), nil
}

// controlRequest is the decoded POST /dashboard/control instruction. The
// console's JS always posts urlencoded; a JSON shape exists so scripts can
// drive the same proxy.
type controlRequest struct {
	Action string `json:"action"`
	Target string `json:"target"`
	// Confirm is the console-level confirmation: destructive/fleet-wide
	// actions refuse without it. It is forwarded as the API's own
	// confirm=true query convention on the proxied request.
	Confirm  bool   `json:"confirm"`
	Reason   string `json:"reason"` // required for project_bump
	Ticks    int    `json:"ticks"`  // optional bump fields; zero = API default
	Cooldown int    `json:"cooldown"`

	// ── project_update (PUT) fields; only supplied fields are applied ──
	RepoURL        *string  `json:"repo_url,omitempty"`
	Workdir        *string  `json:"workdir,omitempty"`
	Weight         *int     `json:"weight,omitempty"`
	Priority       *int     `json:"priority,omitempty"`
	CooldownS      *int     `json:"cooldown_s,omitempty"`
	DecayRate      *float64 `json:"decay_rate,omitempty"`
	Model          *string  `json:"model,omitempty"`
	Provider       *string  `json:"provider,omitempty"`
	WorkerModel    *string  `json:"worker_model,omitempty"`
	WorkerProvider *string  `json:"worker_provider,omitempty"`
	Prompt         *string  `json:"prompt,omitempty"`
	Deliver        *string  `json:"deliver,omitempty"`

	// ── namespace_update (PUT) / namespace_create (POST) fields ──
	NSWeight        *int    `json:"ns_weight,omitempty"`
	NSReserved      *int    `json:"ns_reserved,omitempty"`
	NSHardCap       *int    `json:"ns_hard_cap,omitempty"`
	NSMaxConcurrent *int    `json:"ns_max_concurrent,omitempty"`
	NSDescription   *string `json:"ns_description,omitempty"`
	NSProject       *string `json:"ns_project,omitempty"` // namespace_move: lane to assign

	// ── group/template fields ──
	GName        string   `json:"g_name,omitempty"`     // create: name
	GProjects    []string `json:"g_projects,omitempty"` // create/update: members
	GDescription *string  `json:"g_description,omitempty"`
	TDescription *string  `json:"t_description,omitempty"`
	// TTasksJSON is a JSON-encoded []blocks.TemplateTask (create/update).
	TTasksJSON string `json:"t_tasks,omitempty"`
	// DeployTemplate names the template a group deploy applies.
	DeployTemplate string `json:"deploy_template,omitempty"`
	// DryRun plans a deploy without writing (the API's own dry_run). Also
	// exempts the deploy from the console confirmation gate.
	DryRun bool `json:"dry_run,omitempty"`
}

// parseControlRequest decodes the instruction from an urlencoded form body
// (the console's JS always posts this shape) or a JSON body.
func parseControlRequest(r *http.Request) (controlRequest, error) {
	var req controlRequest
	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "application/x-www-form-urlencoded"), strings.HasPrefix(ct, "multipart/form-data"):
		if err := r.ParseForm(); err != nil {
			return req, fmt.Errorf("invalid form body: %w", err)
		}
		form := r.PostForm
		get := func(k string) string { return strings.TrimSpace(form.Get(k)) }
		req.Action = get("action")
		req.Target = get("target")
		req.Confirm = form.Get("confirm") == "true"
		req.Reason = get("reason")
		if v := get("ticks"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return req, errors.New("ticks must be an integer")
			}
			req.Ticks = n
		}
		if v := get("cooldown"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return req, errors.New("cooldown must be an integer")
			}
			req.Cooldown = n
		}
		req.GName = get("g_name")
		// Key-present semantics: an empty g_projects field CLEARS the member
		// list (a deliberate update), while an absent field leaves it alone.
		if _, present := form["g_projects"]; present {
			req.GProjects = []string{}
			if v := form.Get("g_projects"); strings.TrimSpace(v) != "" {
				for _, p := range strings.Split(v, ",") {
					if p = strings.TrimSpace(p); p != "" {
						req.GProjects = append(req.GProjects, p)
					}
				}
			}
		}
		req.DeployTemplate = get("deploy_template")
		req.DryRun = form.Get("dry_run") == "true"
		req.TTasksJSON = form.Get("t_tasks")
		strPtr := func(k string) *string {
			raw, ok := form[k]
			if !ok || len(raw) == 0 {
				return nil
			}
			v := raw[0] // NOT trimmed: prompts and descriptions may be padded
			return &v
		}
		intPtr := func(k string) *int {
			v := get(k)
			if v == "" {
				return nil
			}
			n, err := strconv.Atoi(v)
			if err != nil {
				return nil // unparseable leaves the field untouched; the API re-validates
			}
			return &n
		}
		floatPtr := func(k string) *float64 {
			v := get(k)
			if v == "" {
				return nil
			}
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return nil
			}
			return &f
		}
		req.RepoURL = strPtr("repo_url")
		req.Workdir = strPtr("workdir")
		req.Weight = intPtr("weight")
		req.Priority = intPtr("priority")
		req.CooldownS = intPtr("cooldown_s")
		req.DecayRate = floatPtr("decay_rate")
		req.Model = strPtr("model")
		req.Provider = strPtr("provider")
		req.WorkerModel = strPtr("worker_model")
		req.WorkerProvider = strPtr("worker_provider")
		req.Prompt = strPtr("prompt")
		req.Deliver = strPtr("deliver")
		req.NSWeight = intPtr("ns_weight")
		req.NSReserved = intPtr("ns_reserved")
		req.NSHardCap = intPtr("ns_hard_cap")
		req.NSMaxConcurrent = intPtr("ns_max_concurrent")
		req.NSDescription = strPtr("ns_description")
		if v := get("ns_project"); v != "" {
			req.NSProject = &v
		}
		req.GDescription = strPtr("g_description")
		req.TDescription = strPtr("t_description")
	default:
		// JSON instruction (direct API use of the proxy).
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			return req, fmt.Errorf("read body: %w", err)
		}
		if len(body) == 0 {
			return req, errors.New("missing instruction body (action required)")
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return req, fmt.Errorf("invalid JSON instruction: %w", err)
		}
	}
	if strings.TrimSpace(req.Action) == "" {
		return req, errors.New("action is required")
	}
	return req, nil
}

// buildAPIRequest materializes the API call one instruction forwards to: the
// registry lookup, target substitution, the API's own confirm convention and
// the per-action body. It returns a request ready to be served by the
// in-process API handler, or an error the proxy surfaces as 400 (client
// fault) — never a fabricated API response.
func buildAPIRequest(ctx context.Context, req controlRequest) (*http.Request, error) {
	action, ok := controlActions[req.Action]
	if !ok {
		return nil, fmt.Errorf("unknown action %q — every console action must be a registered API operation", req.Action)
	}
	// Confirmation gate (console-side mirror of the API's own confirm=true
	// requirement): destructive/fleet-wide actions refuse without it —
	// EXCEPT a deploy dry-run, which writes nothing by API contract. The
	// dialog must have NAMED the target; the proxy cannot verify the prose,
	// but the flow (dialog → confirm=true) is enforced end to end.
	if action.Confirm && !req.Confirm && !req.DryRun {
		return nil, fmt.Errorf("%s requires explicit confirmation (confirm=true) — the console asks for it before sending", action.ID)
	}
	path, err := substituteTarget(action.Path, req.Target)
	if err != nil {
		return nil, err
	}
	// The API's OWN confirm convention — the same ?confirm=true DELETE
	// /api/v1/projects/{name} and /namespaces/{id} require. The console
	// NEVER sends purge=true (see the design contract above).
	if action.Confirm {
		q := url.Values{}
		q.Set("confirm", "true")
		path += "?" + q.Encode()
	}

	var body io.Reader
	switch req.Action {
	case "project_bump":
		if strings.TrimSpace(req.Reason) == "" {
			return nil, errors.New("bump requires a reason (the API refuses an unattributed speed-up)")
		}
		bump := map[string]interface{}{"reason": req.Reason}
		if req.Ticks != 0 {
			bump["ticks"] = req.Ticks
		}
		if req.Cooldown != 0 {
			bump["cooldown"] = req.Cooldown
		}
		body = jsonBody(bump)
	case "project_update":
		u := map[string]interface{}{}
		if req.RepoURL != nil {
			u["repo_url"] = *req.RepoURL
		}
		if req.Workdir != nil {
			u["workdir"] = *req.Workdir
		}
		if req.Weight != nil {
			u["weight"] = *req.Weight
		}
		if req.Priority != nil {
			u["priority"] = *req.Priority
		}
		if req.CooldownS != nil {
			u["cooldown_s"] = *req.CooldownS
		}
		if req.DecayRate != nil {
			u["decay_rate"] = *req.DecayRate
		}
		if req.Model != nil {
			u["model"] = *req.Model
		}
		if req.Provider != nil {
			u["provider"] = *req.Provider
		}
		if req.WorkerModel != nil {
			u["worker_model"] = *req.WorkerModel
		}
		if req.WorkerProvider != nil {
			u["worker_provider"] = *req.WorkerProvider
		}
		if req.Prompt != nil {
			u["prompt"] = *req.Prompt
		}
		if req.Deliver != nil {
			u["deliver"] = *req.Deliver
		}
		body = jsonBody(u)
	case "namespace_update":
		patch := map[string]interface{}{}
		if req.NSWeight != nil {
			patch["weight"] = *req.NSWeight
		}
		if req.NSReserved != nil {
			patch["reserved"] = *req.NSReserved
		}
		if req.NSHardCap != nil {
			patch["hard_cap"] = *req.NSHardCap
		}
		if req.NSMaxConcurrent != nil {
			patch["max_concurrent"] = *req.NSMaxConcurrent
		}
		if req.NSDescription != nil {
			patch["description"] = *req.NSDescription
		}
		body = jsonBody(patch)
	case "namespace_move":
		if req.NSProject == nil || strings.TrimSpace(*req.NSProject) == "" {
			return nil, errors.New("namespace move requires the lane name to assign (ns_project)")
		}
		if strings.ContainsAny(*req.NSProject, "/ \t\r\n?#%") {
			return nil, errors.New("lane name must be a bare identifier")
		}
		body = jsonBody(map[string]string{"project": strings.TrimSpace(*req.NSProject)})
	case "namespace_create":
		if strings.TrimSpace(req.Target) == "" || strings.ContainsAny(req.Target, "/ \t\r\n?#%") {
			return nil, errors.New("namespace create requires a bare-identifier id (target)")
		}
		if req.NSWeight == nil || *req.NSWeight <= 0 {
			return nil, errors.New("namespace create requires a positive weight (ns_weight)")
		}
		create := map[string]interface{}{"id": req.Target, "weight": *req.NSWeight}
		if req.NSDescription != nil {
			create["description"] = *req.NSDescription
		}
		body = jsonBody(create)
	case "group_create":
		if strings.TrimSpace(req.GName) == "" || strings.ContainsAny(req.GName, " \t\r\n") {
			return nil, errors.New("group create requires a whitespace-free name (g_name)")
		}
		body = jsonBody(blocks.Group{Name: req.GName, Projects: req.GProjects, Description: valueOr(req.GDescription)})
	case "group_update":
		patch := blocks.GroupUpdate{}
		if req.GProjects != nil {
			patch.Projects = &req.GProjects
		}
		if req.GDescription != nil {
			patch.Description = req.GDescription
		}
		body = jsonBody(patch)
	case "group_deploy":
		if strings.TrimSpace(req.DeployTemplate) == "" {
			return nil, errors.New("deploy requires the template to apply (deploy_template)")
		}
		d := map[string]interface{}{"template": req.DeployTemplate}
		if req.DryRun {
			d["dry_run"] = true
		}
		body = jsonBody(d)
	case "template_create":
		if strings.TrimSpace(req.GName) == "" || strings.ContainsAny(req.GName, " \t\r\n") {
			return nil, errors.New("template create requires a whitespace-free name (g_name)")
		}
		tasks, err := decodeTemplateTasks(req.TTasksJSON)
		if err != nil {
			return nil, err
		}
		body = jsonBody(blocks.Template{Name: req.GName, Description: valueOr(req.TDescription), Tasks: tasks})
	case "template_update":
		patch := blocks.TemplateUpdate{}
		if req.TTasksJSON != "" {
			tasks, err := decodeTemplateTasks(req.TTasksJSON)
			if err != nil {
				return nil, err
			}
			patch.Tasks = &tasks
		}
		if req.TDescription != nil {
			patch.Description = req.TDescription
		}
		body = jsonBody(patch)
	default:
		// pause/resume/spawn/unbump/delete: the API op takes an empty body.
		body = strings.NewReader("{}")
	}

	httpReq, err := http.NewRequestWithContext(ctx, action.Method, path, body)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	return httpReq, nil
}

func valueOr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func jsonBody(v interface{}) io.Reader {
	b, _ := json.Marshal(v)
	return strings.NewReader(string(b))
}

// decodeTemplateTasks parses the JSON-encoded task list the template forms
// submit. It fails closed: an unparseable task list is refused here (400)
// rather than forwarded — the API's 400 stays free to name semantic problems
// (missing titles) instead of transport ones.
func decodeTemplateTasks(raw string) ([]blocks.TemplateTask, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("a template needs at least one task (JSON list of {title, detail?, labels?})")
	}
	var tasks []blocks.TemplateTask
	if err := json.Unmarshal([]byte(raw), &tasks); err != nil {
		return nil, fmt.Errorf("tasks must be a JSON list of {title, ...}: %w", err)
	}
	return tasks, nil
}

// controlResponseRecorder is a minimal http.ResponseWriter that captures the
// in-process API call's status, headers and body. Deliberately NOT
// httptest.NewRecorder: this is production code, and the type is four small
// methods.
type controlResponseRecorder struct {
	code   int
	header http.Header
	buf    strings.Builder
}

func (c *controlResponseRecorder) Header() http.Header {
	if c.header == nil {
		c.header = http.Header{}
	}
	return c.header
}
func (c *controlResponseRecorder) Write(b []byte) (int, error) {
	if c.code == 0 {
		c.code = http.StatusOK
	}
	return c.buf.Write(b)
}
func (c *controlResponseRecorder) WriteHeader(code int) { c.code = code }
func (c *controlResponseRecorder) result() (int, http.Header, string) {
	code := c.code
	if code == 0 {
		code = http.StatusOK
	}
	return code, c.header, c.buf.String()
}

// controlServeTimeout bounds the in-process API call. Spawns and deploys
// answer far inside it; a wedged API answers 504 from the proxy instead of
// hanging the console request.
const controlServeTimeout = 30 * time.Second

// ControlProxy is POST /dashboard/control: decode the instruction, serve the
// mapped API operation through the IN-PROCESS API handler (the very handler
// main.go mounted at /api/ — identical auth gate, identical handlers,
// identical audit), and return the API's status code and body VERBATIM
// (success and failure).
//
// The browser authenticates via the API's basic mode: the incoming request's
// Authorization/X-Operator-Token headers are forwarded VERBATIM to the
// in-process call — the proxy never reads, stores or transforms the
// credential (it cannot: it never decodes the header). Without browser-cached
// basic credentials the mutating route answers 401 + WWW-Authenticate — the
// proxy forwards status, body AND that challenge header — and the browser
// prompts natively, then the operator retries. With NO credential configured
// the API answers 503 (fail-closed) and the console renders that refusal.
func (g *Generator) ControlProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeProxyError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	// controlAuthHeaders are the credential headers the proxy forwards VERBATIM
	// from the incoming browser request to the in-process API call. The proxy
	// never reads, stores or transforms them — it is a dumb pipe: the browser's
	// cached HTTP basic credentials ride the same-origin XHR, the API's mutation
	// gate validates them, and the audit row records the real identity. With no
	// credential configured the API answers 503 and the proxy forwards that
	// refusal — fail-closed at the API, exactly as for a direct curl.
	var controlAuthHeaders = []string{"Authorization", "X-Operator-Token"}

	apiHandler := g.controlHandler()
	if apiHandler == nil {
		// Fail-closed mirror of the auth row: with no API handler wired the
		// console cannot prove the mutation would be gated, so it refuses.
		writeProxyError(w, http.StatusServiceUnavailable,
			"control API not wired (no in-process API handler) — mutations refused, not forwarded")
		return
	}
	req, err := parseControlRequest(r)
	if err != nil {
		writeProxyError(w, http.StatusBadRequest, err.Error())
		return
	}
	apiReq, err := buildAPIRequest(r.Context(), req)
	if err != nil {
		writeProxyError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), controlServeTimeout)
	defer cancel()
	apiReq = apiReq.WithContext(ctx)
	// Forward the browser's credential headers VERBATIM (see
	// controlAuthHeaders): the same-origin XHR carries the browser-cached
	// HTTP basic credentials; the API's mutation gate validates them and
	// writes the audit row. The proxy is a dumb pipe — it never decodes or
	// stores the credential.
	for _, h := range controlAuthHeaders {
		if v := r.Header.Get(h); v != "" {
			apiReq.Header.Set(h, v)
		}
	}

	rec := &controlResponseRecorder{}
	// Served synchronously: the handler never escapes this call.
	apiHandler.ServeHTTP(rec, apiReq)
	code, header, body := rec.result()

	// Forward the auth-relevant headers so the browser's basic-auth prompt
	// fires on the 401 challenge path (WWW-Authenticate) and tooling can see
	// the gate (X-Operator-Auth: required on refusals).
	for _, h := range []string{"WWW-Authenticate", "X-Operator-Auth"} {
		if v := header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}

// writeProxyError answers a CONSOLE-side fault (bad instruction, unknown
// action, unwired API) in the same {"error": …} shape the API uses, so the
// UI renders every failure through one code path.
func writeProxyError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// ── Deploy blocks console (groups + templates page) ──

// BlocksConsoleData carries the deploy groups/templates console: the ordered
// actions, the live lists (rendered read-only beside their create/update/
// delete/deploy controls), and the global paused badge.
type BlocksConsoleData struct {
	Title            string
	GeneratedAt      string
	Actions          []ControlActionView
	Control          ControlData
	Groups           []blocks.Group
	Templates        []blocks.Template
	FleetPaused      bool
	FleetPausedKnown bool
	// StoreAvailable is false when no blocks store is wired — the console
	// renders the explicit unavailable state (mirroring the API's 503 on
	// those routes) instead of an empty list that could read as "none".
	StoreAvailable bool
}

// blocksStore returns the generator's blocks store, or nil (export_test.go
// exposes the test seam; the dashboard otherwise only reads through the API).
func (g *Generator) blocksStore() *blocks.Store { return g.blocks }

// SetBlocksStore wires the deploy-blocks JSONL store the /blocks console page
// lists read-only. Optional: nil renders the page's explicit unavailable
// state. Writes never happen here — they flow through the control proxy so
// the API stays the single mutation/audit point.
func (g *Generator) SetBlocksStore(st *blocks.Store) { g.blocks = st }

// blocksConsoleSrc returns the console's rendered-source corpus for the
// credential-handling scan: the overview page template (which embeds the
// console driver) plus the blocks console template. The pageTemplate const
// already carries the same driver, so the scan covers every console-bearing
// surface.
func blocksConsoleSrc() string {
	return pageTemplate
}

// GenerateBlocksConsole renders the deploy groups/templates control page.
// It is read + render only: every write on this page goes through the same
// POST /dashboard/control proxy as the other pages (the API is the single
// audit point). A missing blocks store renders the explicit unavailable
// state rather than an empty list.
func (g *Generator) GenerateBlocksConsole(w io.Writer, paused *bool) error {
	data := BlocksConsoleData{
		Title:          "Deploy Blocks",
		GeneratedAt:    g.clock().Now().UTC().Format(time.RFC3339),
		Actions:        make([]ControlActionView, 0, len(blockControlActions)),
		StoreAvailable: g.blocksStore() != nil,
	}
	for _, a := range controlViews("blocks") {
		data.Actions = append(data.Actions, ControlActionView{ControlAction: a, DomID: "ctl-" + a.ID})
	}
	data.Control = ControlData{Actions: data.Actions, FleetWide: false}
	if paused != nil {
		data.FleetPaused, data.FleetPausedKnown = *paused, true
	}
	if store := g.blocksStore(); store != nil {
		groups, err := store.ListGroups()
		if err != nil {
			return fmt.Errorf("list groups: %w", err)
		}
		templates, err := store.ListTemplates()
		if err != nil {
			return fmt.Errorf("list templates: %w", err)
		}
		data.Groups, data.Templates = groups, templates
	}
	return g.blocksTmpl.Execute(w, data)
}

// import side effects: the deploy page renders store rows read-only; writes
// flow only through the control proxy above.

// SetControlAPIHandler wires the in-process API handler the console proxies
// to (main.go passes apiServer.Handler(), the SAME handler it mounts at
// /api/). Nil (tests, or a caller that never wired it) leaves the console
// fail-closed: the proxy answers 503 and the control strips refuse to act.
func (g *Generator) SetControlAPIHandler(h http.Handler) {
	g.controlAPI = h
}

// controlHandler returns the wired API handler, or nil when unwired.
func (g *Generator) controlHandler() http.Handler { return g.controlAPI }

// controlViews maps a page's control-group name to its ordered action list —
// the single source the templates' {{range}} loops and the parity tests read.
func controlViews(group string) []ControlAction {
	var ids []string
	switch group {
	case "lane":
		ids = laneControlActions
	case "namespace":
		ids = namespaceControlActions
	case "global":
		ids = globalControlActions
	case "blocks":
		ids = blockControlActions
	default:
		return nil
	}
	actions := make([]ControlAction, 0, len(ids))
	for _, id := range ids {
		actions = append(actions, controlActions[id])
	}
	return actions
}

// ControlActionView is one rendered control: the registry action plus the
// pre-computed pieces the template needs (an htmx-safe id, the fleet-wide /
// confirm flags for the dialog).
type ControlActionView struct {
	ControlAction
	// DomID is the button's id (action + current target) so the result
	// region can be addressed deterministically.
	DomID string
	// Target is the lane/namespace/group/template this instance acts on
	// ("" for global strips and create forms).
	Target string
}

// laneControls builds the control strip views for a lane page.
func laneControls(name string) []ControlActionView {
	views := make([]ControlActionView, 0, len(laneControlActions))
	for _, a := range controlViews("lane") {
		views = append(views, ControlActionView{ControlAction: a, DomID: "ctl-" + a.ID, Target: name})
	}
	return views
}

// namespaceControls builds the control strip views for a namespace page.
func namespaceControls(id string) []ControlActionView {
	views := make([]ControlActionView, 0, len(namespaceControlActions))
	for _, a := range controlViews("namespace") {
		views = append(views, ControlActionView{ControlAction: a, DomID: "ctl-" + a.ID, Target: id})
	}
	return views
}

// globalControls builds the fleet overview's global console strip.
func globalControls() []ControlActionView {
	views := make([]ControlActionView, 0, len(globalControlActions))
	for _, a := range controlViews("global") {
		views = append(views, ControlActionView{ControlAction: a, DomID: "ctl-" + a.ID})
	}
	return views
}

// ControlData is the shared shape the control templates consume: the page's
// ordered actions and the global paused state (rendered on every strip so
// the operator sees WHY pause/resume is the offered pair).
type ControlData struct {
	Actions   []ControlActionView
	FleetWide bool
}

// globalPaused reads the loop's authoritative paused flag through the API
// status payload (GET /api/v1/status carries scheduler_paused via the loop;
// the dashboard holds the same *scheduler.Loop-derived handler). Reading it
// here from the SAME loop the API reads keeps the console's badge honest.
// Errors degrade to "unknown" — never a fabricated state.
