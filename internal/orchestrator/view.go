package orchestrator

import (
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/bonnie/presence"
)

// viewKind identifies one top-level screen. Each screen owns one stable SSE
// patch root so unrelated screens never receive each other's markup.
type viewKind string

const (
	viewBoard    viewKind = "board"
	viewArchive  viewKind = "archive"
	viewPlanning viewKind = "planning"
	viewRuns     viewKind = "runs"
	viewAgents   viewKind = "agents"
	viewSettings viewKind = "settings"
)

var (
	idPattern    = regexp.MustCompile(`^[0-9a-f]{1,64}$`)
	runStateTabs = []string{"all", "active", "blocked", "failed", "ready"}
)

// viewParams is the validated, URL-safe description of what the user is
// looking at. Every field is allow-listed, so it can be embedded in URLs and
// Datastar expressions without escaping surprises.
type viewParams struct {
	View    viewKind
	Project string // project ID; empty means the first project
	Card    string // runs filtered to one card
	State   string // runs state tab
}

func viewFromPath(path string) (viewKind, bool) {
	switch strings.TrimSuffix(path, "/") {
	case "/archive":
		return viewArchive, true
	case "/board":
		return viewBoard, true
	case "/planning":
		return viewPlanning, true
	case "/runs":
		return viewRuns, true
	case "/agents":
		return viewAgents, true
	case "/settings":
		return viewSettings, true
	}
	return "", false
}

func validID(v string) string {
	if idPattern.MatchString(v) {
		return v
	}
	return ""
}

// parseViewParams reads the view from query or form values. Unknown or
// malformed values degrade to safe defaults instead of failing a request.
// An empty kind means "take it from the view field" (events, mutations).
func parseViewParams(r *http.Request, kind viewKind) viewParams {
	p := viewParams{View: kind, Project: validID(r.FormValue("project")), Card: validID(r.FormValue("card")), State: "all"}
	if kind == "" {
		p.View = viewBoard
		switch v := viewKind(r.FormValue("view")); v {
		case viewArchive, viewRuns, viewAgents, viewSettings, viewPlanning:
			p.View = v
		}
	}
	if s := r.FormValue("state"); slices.Contains(runStateTabs, s) {
		p.State = s
	}
	return p
}

// Path is the canonical, bookmarkable URL of this view.
func (v viewParams) Path() string {
	q := url.Values{}
	if v.Project != "" && (v.View == viewBoard || v.View == viewArchive) {
		q.Set("project", v.Project)
	}
	if v.View == viewRuns || v.View == viewPlanning {
		if v.Card != "" {
			q.Set("card", v.Card)
		}
		if v.State != "" && v.State != "all" {
			q.Set("state", v.State)
		}
	}
	out := "/" + string(v.View)
	if len(q) > 0 {
		out += "?" + q.Encode()
	}
	return out
}

// EventsURL is the long-lived Datastar SSE endpoint for this view.
func (v viewParams) EventsURL() string {
	q := url.Values{"view": {string(v.View)}}
	if v.Project != "" {
		q.Set("project", v.Project)
	}
	if v.Card != "" {
		q.Set("card", v.Card)
	}
	if v.State != "" {
		q.Set("state", v.State)
	}
	return "/events?" + q.Encode()
}

// SnapshotAction is the Datastar expression that refreshes this view once.
func (v viewParams) SnapshotAction() string {
	return "@get('" + strings.Replace(v.EventsURL(), "/events?", "/snapshot?", 1) + "')"
}

// runsPath builds a runs URL for a state tab while preserving the card filter.
func runsPath(v viewParams, state string) string {
	v.View, v.State = viewRuns, state
	return v.Path()
}

func boardPath(projectID string) string {
	return viewParams{View: viewBoard, Project: projectID}.Path()
}

func runsForCardPath(cardID string) string {
	return viewParams{View: viewRuns, Card: cardID, State: "all"}.Path()
}

// selectedProject resolves the project a board view should show.
func selectedProject(projects []Project, id string) (Project, bool) {
	for _, p := range projects {
		if p.ID == id {
			return p, true
		}
	}
	if len(projects) > 0 {
		return projects[0], true
	}
	return Project{}, false
}

// projectKey is a short, stable, human-friendly prefix such as "KIT".
func projectKey(name string) string {
	var out []rune
	for _, r := range strings.ToUpper(name) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			out = append(out, r)
		}
		if len(out) == 3 {
			break
		}
	}
	for len(out) < 3 {
		out = append(out, 'X')
	}
	return string(out)
}

// cardKey renders an identifier like KIT-6780 from a project and card ID.
func cardKey(projects []Project, c Card) string {
	name := ""
	for _, p := range projects {
		if p.ID == c.ProjectID {
			name = p.Name
		}
	}
	id := c.ID
	if len(id) > 4 {
		id = id[:4]
	}
	return projectKey(name) + "-" + strings.ToUpper(id)
}

func initial(name string) string {
	for _, r := range strings.ToUpper(strings.TrimSpace(name)) {
		return string(r)
	}
	return "?"
}

// runTone maps an attempt to the one visual state shown on lists and cards.
const interruptedNotice = "agent run interrupted; awaiting agent recovery"

const executionConflictNotice = "Terminal agent report could not be applied: execution identity is missing or conflicts with the pinned run. Reconciliation required."

func runTone(a Attempt) string {
	switch {
	case !a.Ready && a.Error == resumePendingNotice:
		return "resuming"
	case !a.Ready && a.Error == executionConflictNotice:
		return "blocked"
	case a.Ready || a.State == "ready":
		return "ready"
	case a.State == "failed":
		return "failed"
	case a.State == "blocked":
		return "blocked"
	case a.State == "running" || a.RunState == "running":
		return "running"
	}
	return "queued"
}

func runLabel(a Attempt) string {
	if !a.Ready && a.Error == executionConflictNotice {
		return "Needs reconciliation"
	}
	// A parked blocker can briefly report running before BONNIE records its
	// suspension; only the interruption notice marks a stalled running turn.
	if a.State == "blocked" && (a.RunState == "interrupted" || (a.RunState == "running" && a.Error == interruptedNotice)) {
		return "Interrupted"
	}
	if a.RunState == "cancelled" && !a.Ready {
		return "Cancelled"
	}
	if a.RunState == abandonedRunState && !a.Ready {
		return "Force-stopped"
	}
	switch runTone(a) {
	case "resuming":
		return "Resuming…"
	case "ready":
		return "Ready for review"
	case "failed":
		return "Failed"
	case "blocked":
		return "Blocked"
	case "running":
		return "Running"
	}
	if (a.State == "queued" || a.State == "submitted") && a.AgentID == "" && a.TargetAgentID != "" {
		return "Queued for " + a.TargetAgentID
	}
	if a.State == "queued" {
		return "Queued"
	}
	if a.State == "submitted" {
		return "Awaiting agent"
	}
	return "Waiting"
}

func matchesRunState(a Attempt, state string) bool {
	switch state {
	case "active":
		return attemptActive(a)
	case "blocked", "failed", "ready":
		return runTone(a) == state
	}
	return true
}

func cardByID(cards []Card, id string) (Card, bool) {
	for _, c := range cards {
		if c.ID == id {
			return c, true
		}
	}
	return Card{}, false
}

func projectByID(projects []Project, id string) (Project, bool) {
	for _, p := range projects {
		if p.ID == id {
			return p, true
		}
	}
	return Project{}, false
}

// workSummary counts cards with durable active work, not live agent processes.
// Archive visibility does not stop execution, so archived cards are included.
type workSummary struct {
	Planning, Building, Fix int
}

func (s workSummary) Total() int { return s.Planning + s.Building + s.Fix }

func (s workSummary) Label() string {
	noun := "items"
	if s.Total() == 1 {
		noun = "item"
	}
	return fmt.Sprintf("%d active work %s: %d planning, %d building, %d fixing (includes queued and archived work)", s.Total(), noun, s.Planning, s.Building, s.Fix)
}

func workingState(state string) bool {
	switch state {
	case "queued", "submitted", "running":
		return true
	}
	return false
}

// Terminal/paused evidence overrides stale running evidence. Pending resume is
// the explicit presentation exception, while the resume command is delivered.
func attemptWorking(a Attempt) bool {
	if a.Ready || a.State == "ready" {
		return false
	}
	if a.Error == resumePendingNotice {
		return true
	}
	if a.Error == executionConflictNotice {
		return false
	}
	for _, state := range []string{a.State, a.RunState} {
		switch state {
		case "ready", "blocked", "waiting", "interrupted", "failed", "cancelled", "completed":
			return false
		}
	}
	return workingState(a.State) && (a.RunState == "" || workingState(a.RunState))
}

func planningWorking(detail PlanningDetail) bool {
	if planningDisplayState(detail) == "resuming" {
		return true
	}
	if !workingState(detail.Session.State) {
		return false
	}
	turn, ok := latestPlanningTurn(detail)
	return ok && (workingState(turn.State) || (turn.State == "waiting" && detail.Session.State == "running"))
}

func projectWork(s uiSnapshot, projectID string) workSummary {
	var summary workSummary
	for _, c := range s.cards {
		if c.ProjectID != projectID {
			continue
		}
		switch c.Status {
		case "Planning":
			if detail, ok := s.planning[c.ID]; ok && planningWorking(detail) {
				summary.Planning++
			}
		case "Building", "Fix":
			latest := latestAttempt(c.ID, s.attempts)
			if len(latest) == 0 || !attemptWorking(latest[0]) {
				continue
			}
			if c.Status == "Building" {
				summary.Building++
			} else {
				summary.Fix++
			}
		}
	}
	return summary
}

func cardsIn(cards []Card, projectID, status string) []Card {
	var out []Card
	for _, c := range cards {
		if !c.Archived && c.ProjectID == projectID && c.Status == status {
			out = append(out, c)
		}
	}
	return out
}

// relativeTime is the server-rendered text; relTimeExpr mirrors its thresholds.
func relativeTime(t, now time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := now.Sub(t)
	switch {
	case d < 45*time.Second:
		return "just now"
	case d < 90*time.Second:
		return "1m ago"
	case d < 45*time.Minute:
		return strconv.Itoa(int(d.Minutes())) + "m ago"
	case d < 90*time.Minute:
		return "1h ago"
	case d < 36*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h ago"
	}
	return strconv.Itoa(int(d.Hours()/24)) + "d ago"
}

// relTimeExpr is the client mirror of relativeTime; thresholds must match.
func relTimeExpr(t time.Time) string {
	return fmt.Sprintf(`(s => s<45?'just now':s<90?'1m ago':s<2700?Math.floor(s/60)+'m ago':s<5400?'1h ago':s<129600?Math.floor(s/3600)+'h ago':Math.floor(s/86400)+'d ago')(($_now-%d)/1000)`, t.UnixMilli())
}

func timeAttr(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// agentCurrentRun returns the active attempt a agent is executing, if any.
func agentCurrentRun(r presence.Record, attempts []Attempt) (Attempt, bool) {
	for _, a := range slices.Backward(attempts) {
		if a.AgentID == r.Identity.Agent && attemptActive(a) {
			return a, true
		}
	}
	return Attempt{}, false
}

func agentReady(r presence.Record) bool {
	for _, e := range r.Endpoints {
		if r.State == presence.Ready && e.Input && e.Ready {
			return true
		}
	}
	return false
}

func readyAgents(agents []presence.Record) int {
	n := 0
	for _, w := range agents {
		if agentReady(w) {
			n++
		}
	}
	return n
}

// providerConfig reports only whether credentials are configured, never values.
type providerConfig struct {
	Name, Detail string
	Configured   bool
}

func dashboardValue(d pageData, key, env, fallback string) string {
	if value, ok := d.dashboard[key]; ok {
		return value
	}
	if value, ok := d.defaults[key]; ok {
		return value
	}
	if value := os.Getenv(env); value != "" {
		return value
	}
	return fallback
}

func providerConfigs(overrides map[string]string, defaults ...map[string]string) []providerConfig {
	values := map[string]string{}
	if len(defaults) > 0 {
		maps.Copy(values, defaults[0])
	}
	maps.Copy(values, overrides)
	return []providerConfig{
		{"GitHub", "--github-token / JAWA_GITHUB_TOKEN or GITHUB_TOKEN", strings.TrimSpace(effectiveProviderSetting(values, "github-token", "JAWA_GITHUB_TOKEN")) != ""},
		{"Forgejo", "--forgejo-token / JAWA_FORGEJO_TOKEN or FORGEJO_TOKEN", strings.TrimSpace(effectiveProviderSetting(values, "forgejo-token", "JAWA_FORGEJO_TOKEN")) != ""},
	}
}

func connectSnippet(natsURL, username string) string {
	if natsURL == "" {
		natsURL = "nats://127.0.0.1:4222"
	}
	if username == "" {
		username = "agent"
	}
	return "NATS_URL=" + natsURL + "\nNATS_USERNAME=" + username + "\nNATS_PASSWORD=<agent password>\nJAWA_NATS_AGENT_ID=jawa-1\n./bin/jawa"
}

func providerLabel(p string) string {
	switch p {
	case "github":
		return "GitHub"
	case "forgejo":
		return "Forgejo"
	}
	return "Git"
}

// repoLabel shortens a repository URL to owner/name when it can.
func repoLabel(repo string) string {
	if _, owner, name, err := providerRepository(repo); err == nil {
		return owner + "/" + name
	}
	return repo
}

// repoURL returns a browsable https URL, or "" for ssh and other schemes.
func repoURL(repo string) string {
	u, err := url.Parse(repo)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return ""
	}
	u.Path = strings.TrimSuffix(u.Path, ".git")
	return u.String()
}

func laneName(status string) string {
	if status == "Done" {
		return "Ready for review"
	}
	return status
}

func laneHint(status string) string {
	switch status {
	case "Planning":
		return "Investigate ideas and approve a plan before building."
	case "Fix":
		return "Repairs to existing pull requests appear here."
	case "Building":
		return "Cards an agent is working on appear here."
	case "Done":
		return "Verified pull requests arrive here."
	}
	return "Your next idea starts here."
}

// filterExpr is the Datastar expression that hides cards not matching $filter.
func filterExpr(projects []Project, c Card) string {
	return strconv.Quote(c.Title+" "+c.Description+" "+cardKey(projects, c)) + ".toLowerCase().includes($filter.toLowerCase())"
}

// cardUpdated is when the card last changed: its newest attempt, else creation.
func cardUpdated(c Card, attempts []Attempt) time.Time {
	t := c.CreatedAt
	for _, a := range attempts {
		if a.CardID == c.ID && a.UpdatedAt.After(t) {
			t = a.UpdatedAt
		}
	}
	return t
}

func tabLabel(state string) string {
	switch state {
	case "all":
		return "All"
	case "ready":
		return "Ready"
	}
	return strings.ToUpper(state[:1]) + state[1:]
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func displayDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func natsDisplayURL(u string) string {
	if u == "" {
		return "nats://127.0.0.1:4222"
	}
	return u
}

func planningPath(cardID string) string {
	return viewParams{View: viewPlanning, Card: cardID}.Path()
}

func agentModelLabel(r presence.Record, key string) string {
	if value := r.Labels[key]; value != "" {
		return value
	}
	return "Unknown"
}
