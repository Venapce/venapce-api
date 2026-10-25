package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"

	"github.com/Venapce/venapce-api/internal/db"
	"github.com/Venapce/venapce-api/internal/flomorphic"
	tagutil "github.com/Venapce/venapce-api/internal/tags"
)

// Activities — the history of a pipeline row and the outcome of every flow run
// on it. A row of stage / findings / issues is the SUBJECT; each activity says
// what happened to it: a manual edit (`edit`, with the field diff in ref), a
// promotion (`promote` / `create`), or a FloMorphic run (`run`).
//
// A run works like this: the subject row is sent to FloMorphic as the run's
// context document (see contextFor), the flow reads it through {{$.row.…}}
// tokens, loops / calls LLMs / queries nodes as its author designed, and
// leaves what it concluded in `$.outcome`. When the process ends, venapce reads
// the document back and lifts `outcome` into the activity's typed columns —
// title, description, remediation, proof, facts [{k,v}], tags — with the whole
// document kept in `data`. The flow may also write those columns directly with
// the plugin's db.activities.update node (id = {{$.activity.id}}). Either way
// the outcome is queryable and the front knows what each field means.

// Activity kinds.
const (
	actRun     = "run"
	actEdit    = "edit"
	actPromote = "promote"
	actCreate  = "create"
	actNote    = "note"
)

// Activity statuses mirror FloMorphic's process lifecycle; a manual activity
// is simply `finished`.
const (
	actFinished = flomorphic.ProcessFinished
	actFailed   = flomorphic.ProcessFailed
	actRunning  = flomorphic.ProcessRunning
)

// subjectKinds are the tables an activity can be about. `operation` is the
// subject of an originating run — an operation's entry flow started from the
// Operations page, with no pipeline row behind it.
var subjectKinds = map[string]bool{"stage": true, "finding": true, "issue": true, subjectOperation: true}

// fact is one typed key/value of an outcome: [{"k":"severity","v":"low"}].
// `v` is any JSON value — a string usually, but a number, list or object is
// kept as-is.
type fact struct {
	K string          `json:"k"`
	V json.RawMessage `json:"v"`
}

// ---- HTTP: flows (picker) ----

// GET /api/flows?search=&tags= — the FloMorphic flows an operator can run a
// row through. Needs the FloMorphic API access from Settings.
//
// `tags` is a comma-separated match-ANY label filter, applied by FloMorphic
// (GET /flow?tags=), not here — so it narrows the page FloMorphic returns
// rather than a page already truncated at 200. It is how the run picker shows
// the flows one operation installed instead of every flow on a shared canvas:
// an operation stamps its labels on its flows at install time (see
// Manifest.FlowTagsFor) and the picker asks for them by name. An absent or
// empty `tags` is the unfiltered list — clearing the filter is the same call
// without the parameter.
func (s *Server) listFlows(c fiber.Ctx) error {
	client := s.flo.Get()
	if client == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "FloMorphic API access is not configured (Settings → FloMorphic)")
	}
	flows, err := client.ListFlows(c.Context(), c.Query("search"), tagutil.Split(c.Query("tags")), 200)
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, err.Error())
	}
	return c.JSON(flows)
}

// flowTagFacets is what GET /api/flows/tags answers: the installed operations
// and the labels each one stamped on its flows, plus the union of them all.
//
// It exists so the run picker's filter can offer "this operation" by name
// without the browser reading every installed package's manifest. `tags` is
// there for a picker that wants to offer bare labels too — including the ones
// an author put on a single flow inside a package.
type flowTagFacets struct {
	Operations []flowTagOperation `json:"operations"`
	Tags       []string           `json:"tags"`
}

type flowTagOperation struct {
	ID   int64    `json:"id"`
	Name string   `json:"name"`
	Slug string   `json:"slug"`
	Tags []string `json:"tags"`
}

// GET /api/flows/tags — the operation filter's menu. Read-only and derived
// entirely from installed packages, so it answers even when FloMorphic is
// unreachable: the picker can still show its filter beside the error that says
// the flow list could not be loaded.
func (s *Server) flowTags(c fiber.Ctx) error {
	ops, err := s.q.ListOperations(c.Context())
	if err != nil {
		return err
	}
	out := flowTagFacets{Operations: []flowTagOperation{}, Tags: []string{}}
	var all [][]string
	for _, op := range ops {
		b, err := s.loadBundle(op)
		if err != nil {
			// An unreadable manifest is already reported by the operations
			// list; here it just means one operation cannot be offered.
			log.Printf("flows: operation #%d unreadable, no tag facet: %v", op.ID, err)
			continue
		}
		labels := b.Manifest.AllFlowTags()
		if len(labels) == 0 {
			continue
		}
		out.Operations = append(out.Operations, flowTagOperation{
			ID: op.ID, Name: b.Manifest.Name, Slug: b.Manifest.ID, Tags: labels,
		})
		all = append(all, labels)
	}
	out.Tags = tagutil.Union(all...)
	return c.JSON(out)
}

// GET /api/flows/:id/entry — the entry form one flow declares, resolved for a
// launch: the form itself, the context path its answers land at, and the values
// the fields should start at.
//
// A flow designer can declare the input a flow needs (FloMorphic's
// `preferences.entry`) instead of wiring a node that seeds the context by hand.
// This is what the run dialog renders in place of the raw-JSON box an operator
// used to have to know the shape for, and `entry: null` is the answer for a
// flow that asks for nothing — a picker's cue to run it the way it always did,
// not an error.
//
// The resolve is FloMorphic's and is proxied rather than recomputed: the
// dialog, an agent over MCP and the designer's own run button have to start the
// fields in the same place. No contextId travels with it because the run's
// context document does not exist until the launch (see startRun), so a `live`
// prefill resolves to the form's defaults.
//
// `last` is this app's own addition: what the previous run of this flow was
// actually given (see lastEntryRun). FloMorphic's `live` prefill cannot answer
// that here — it reads the context the run will go against, and venapce builds
// a fresh one per run — so the previous answers would otherwise be lost to the
// operator even though the engine kept them. `subjectKind` / `subjectId` narrow
// it to one row's own history first, which is the re-run case.
func (s *Server) flowEntry(c fiber.Ctx) error {
	client := s.flo.Get()
	if client == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "FloMorphic API access is not configured (Settings → FloMorphic)")
	}
	view, err := client.FlowEntry(c.Context(), c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, "flow entry: "+err.Error())
	}
	res := flowEntryResponse{EntryView: view}
	// Only worth looking for a flow that asks for something: a flow with no
	// form has no values to have been given.
	if view.Entry != nil {
		subjectID, _ := strconv.ParseInt(c.Query("subjectId"), 10, 64)
		res.Last = s.lastEntryRun(c.Context(), client, c.Params("id"), c.Query("subjectKind"), subjectID)
	}
	return c.JSON(res)
}

// flowEntryResponse is GET /api/flows/:id/entry: FloMorphic's resolved view,
// plus what the last run of the flow was given. The view is embedded so its
// fields stay at the top level — a client reads `entry` / `values` / `target`
// exactly as FloMorphic names them.
type flowEntryResponse struct {
	flomorphic.EntryView
	Last *lastEntryValues `json:"last,omitempty"`
}

// lastEntryValues is what one earlier run of a flow was launched with, and
// enough about that run for an operator to decide whether they want it back.
type lastEntryValues struct {
	Values     map[string]any `json:"values"`
	ActivityID int64          `json:"activityId"`
	Title      string         `json:"title,omitempty"`
	// At is when that run started — what a launcher dates the offer by.
	At *time.Time `json:"at,omitempty"`
	// The row it ran on, and whether that is the row being launched now: values
	// borrowed from another row are still useful, but not the same offer.
	SubjectKind string `json:"subjectKind,omitempty"`
	SubjectID   int64  `json:"subjectId,omitempty"`
	SameSubject bool   `json:"sameSubject"`
}

// entryProbeRuns bounds how far back a lookup walks. The newest run of a flow
// normally has what we want on it; the budget exists because each candidate
// costs one FloMorphic round trip, and a dialog opening is not the place to
// spend ten of them hunting for an answer that is plainly not there.
const entryProbeRuns = 5

// lastEntryRun finds the values the most recent run of this flow was given.
//
// The engine keeps them on the process row (`meta.entry`) precisely so they
// survive the context moving on — that is the record of what a run was actually
// launched with, and it is read from there rather than from the run's final
// context document, where a later node may have overwritten the seed or the
// flow may have replaced `data` wholesale.
//
// This row's own history is searched first and any row's second: re-running a
// flow on the row you are looking at usually means "the same answers as last
// time", while the first run on a new row still benefits from what the previous
// one was given. Nothing here is load-bearing — every failure returns nil and
// the dialog simply does not make the offer.
func (s *Server) lastEntryRun(ctx context.Context, client *flomorphic.Client, flowID, subjectKind string, subjectID int64) *lastEntryValues {
	if strings.TrimSpace(flowID) == "" {
		return nil
	}
	scoped := subjectKind != "" && subjectID != 0
	if scoped {
		if found := s.probeEntryRuns(ctx, client, flowID, subjectKind, subjectID); found != nil {
			found.SameSubject = true
			return found
		}
	}
	return s.probeEntryRuns(ctx, client, flowID, "", 0)
}

// probeEntryRuns walks this flow's runs newest-first and returns the first one
// FloMorphic still holds entry values for.
func (s *Server) probeEntryRuns(ctx context.Context, client *flomorphic.Client, flowID, subjectKind string, subjectID int64) *lastEntryValues {
	rows, err := s.q.ListActivities(ctx, db.ListActivitiesParams{
		SubjectKind: subjectKind,
		SubjectID:   subjectID,
		Kind:        actRun,
		FlowID:      flowID,
		Tags:        []string{},
		Lim:         entryProbeRuns,
	})
	if err != nil {
		log.Printf("flows: last entry values for %s: %v", flowID, err)
		return nil
	}
	for _, a := range rows {
		if a.ProcessID == 0 {
			// A run that never reached the engine was never given anything.
			continue
		}
		proc, err := client.GetProcess(ctx, a.ProcessID)
		if err != nil || proc == nil {
			continue
		}
		values, ok := proc.Meta[flomorphic.MetaEntryKey].(map[string]any)
		if !ok || len(values) == 0 {
			continue
		}
		at := a.StartedAt
		if at == nil {
			at = &a.CreatedAt
		}
		return &lastEntryValues{
			Values:      values,
			ActivityID:  a.ID,
			Title:       a.Title,
			At:          at,
			SubjectKind: a.SubjectKind,
			SubjectID:   a.SubjectID,
		}
	}
	return nil
}

// ---- HTTP: activities ----

// GET /api/activities?subjectKind=&subjectId=&kind=&status=&flowId=&tags=&match=&search=&limit=
func (s *Server) listActivities(c fiber.Ctx) error {
	subjectID, _ := strconv.ParseInt(c.Query("subjectId"), 10, 64)
	limit, _ := strconv.Atoi(c.Query("limit"))
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	items, err := s.q.ListActivities(c.Context(), db.ListActivitiesParams{
		SubjectKind: c.Query("subjectKind"),
		SubjectID:   subjectID,
		Kind:        c.Query("kind"),
		Status:      c.Query("status"),
		FlowID:      c.Query("flowId"),
		Tags:        splitTags(c.Query("tags")),
		MatchAll:    c.Query("match") == "all",
		Search:      c.Query("search"),
		Lim:         int32(limit),
	})
	if err != nil {
		return err
	}
	// A list of a single subject's timeline is what the detail pages poll while a
	// run is in flight, so bring those rows up to date on the way out.
	if subjectID != 0 {
		for i := range items {
			if a, err := s.syncActivity(c.Context(), items[i]); err == nil {
				items[i] = a
			}
		}
	}
	return c.JSON(items)
}

// GET /api/activities/tags
func (s *Server) activityTags(c fiber.Ctx) error {
	tags, err := s.q.ActivityTags(c.Context())
	if err != nil {
		return err
	}
	return c.JSON(orEmpty(tags))
}

// GET /api/activities/:id — the activity (synced with FloMorphic if still in
// flight) plus its subject row for the header link.
func (s *Server) getActivity(c fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	a, err := s.q.GetActivity(c.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fiber.NewError(fiber.StatusNotFound, "activity not found")
	}
	if err != nil {
		return err
	}
	if synced, err := s.syncActivity(c.Context(), a); err == nil {
		a = synced
	}
	subject, _ := s.loadSubject(c.Context(), a.SubjectKind, a.SubjectID)
	return c.JSON(fiber.Map{"item": a, "subject": subject})
}

// activityBody is the create/update payload — partial, like the pipeline
// tables: an absent field keeps its stored value.
type activityBody struct {
	SubjectKind *string         `json:"subjectKind"`
	SubjectID   *int64          `json:"subjectId"`
	Kind        *string         `json:"kind"`
	Status      *string         `json:"status"`
	Title       *string         `json:"title"`
	Description *string         `json:"description"`
	Remediation *string         `json:"remediation"`
	Proof       *string         `json:"proof"`
	Facts       json.RawMessage `json:"facts"`
	Tags        *[]string       `json:"tags"`
	Origin      *string         `json:"origin"`
	Ref         json.RawMessage `json:"ref"`
	Data        json.RawMessage `json:"data"`
	Meta        json.RawMessage `json:"meta"`
}

func (b activityBody) jsonFields() map[string]json.RawMessage {
	return map[string]json.RawMessage{"facts": b.Facts, "ref": b.Ref, "data": b.Data, "meta": b.Meta}
}

// POST /api/activities — record a manual activity (a note, a finding of one's
// own) on a subject row.
func (s *Server) createActivity(c fiber.Ctx) error {
	var body activityBody
	if err := c.Bind().Body(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if err := validJSONFields(body.jsonFields()); err != nil {
		return err
	}
	kind, id := pick("", body.SubjectKind), pickInt(0, body.SubjectID)
	if !subjectKinds[kind] || id == 0 {
		return fiber.NewError(fiber.StatusBadRequest, "subjectKind (stage|finding|issue) and subjectId are required")
	}
	if _, err := s.loadSubject(c.Context(), kind, id); err != nil {
		return fiber.NewError(fiber.StatusNotFound, kind+" #"+strconv.FormatInt(id, 10)+" not found")
	}
	now := time.Now()
	a, err := s.q.CreateActivity(c.Context(), db.CreateActivityParams{
		SubjectKind: kind,
		SubjectID:   id,
		Kind:        orDefault(pick("", body.Kind), actNote),
		Status:      orDefault(pick("", body.Status), actFinished),
		Title:       pick("", body.Title),
		Description: pick("", body.Description),
		Remediation: pick("", body.Remediation),
		Proof:       pick("", body.Proof),
		Facts:       normalizeFacts(body.Facts),
		Tags:        pickTags(nil, body.Tags),
		Origin:      orDefault(pick("", body.Origin), "manual"),
		Errors:      json.RawMessage("{}"),
		Ref:         pickJSON(nil, body.Ref),
		Data:        pickJSON(nil, body.Data),
		Meta:        pickJSON(nil, body.Meta),
		StartedAt:   &now,
		FinishedAt:  &now,
	})
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(a)
}

// PUT /api/activities/:id — partial update of the outcome fields (an operator
// correcting or completing what a flow concluded). The run bookkeeping
// (process / pid / context / timestamps) is not editable here.
func (s *Server) updateActivity(c fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	var body activityBody
	if err := c.Bind().Body(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if err := validJSONFields(body.jsonFields()); err != nil {
		return err
	}
	cur, err := s.q.GetActivity(c.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fiber.NewError(fiber.StatusNotFound, "activity not found")
	}
	if err != nil {
		return err
	}
	facts := cur.Facts
	if len(body.Facts) > 0 {
		facts = normalizeFacts(body.Facts)
	}
	a, err := s.q.UpdateActivity(c.Context(), db.UpdateActivityParams{
		ID:          id,
		SubjectKind: cur.SubjectKind,
		SubjectID:   cur.SubjectID,
		Kind:        orDefault(pick(cur.Kind, body.Kind), cur.Kind),
		Status:      orDefault(pick(cur.Status, body.Status), cur.Status),
		Title:       pick(cur.Title, body.Title),
		Description: pick(cur.Description, body.Description),
		Remediation: pick(cur.Remediation, body.Remediation),
		Proof:       pick(cur.Proof, body.Proof),
		Facts:       facts,
		Tags:        pickTags(cur.Tags, body.Tags),
		Origin:      pick(cur.Origin, body.Origin),
		FlowID:      cur.FlowID,
		FlowTitle:   cur.FlowTitle,
		ProcessID:   cur.ProcessID,
		Pid:         cur.Pid,
		ContextID:   cur.ContextID,
		Error:       cur.Error,
		Errors:      rawOr(cur.Errors, "{}"),
		Ref:         pickJSON(cur.Ref, body.Ref),
		Data:        pickJSON(cur.Data, body.Data),
		Meta:        pickJSON(cur.Meta, body.Meta),
		StartedAt:   cur.StartedAt,
		FinishedAt:  cur.FinishedAt,
	})
	if err != nil {
		return err
	}
	return c.JSON(a)
}

// DELETE /api/activities/:id
func (s *Server) deleteActivity(c fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	if err := s.q.DeleteActivity(c.Context(), id); err != nil {
		return err
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// POST /api/activities/:id/sync — pull the run's current state from FloMorphic
// now (the reconciler does this in the background too).
func (s *Server) syncActivityNow(c fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	a, err := s.q.GetActivity(c.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fiber.NewError(fiber.StatusNotFound, "activity not found")
	}
	if err != nil {
		return err
	}
	synced, err := s.syncActivity(c.Context(), a)
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, err.Error())
	}
	return c.JSON(synced)
}

// POST /api/activities/:id/stop — stop the run on the engine.
func (s *Server) stopActivity(c fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	a, err := s.q.GetActivity(c.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fiber.NewError(fiber.StatusNotFound, "activity not found")
	}
	if err != nil {
		return err
	}
	client := s.flo.Get()
	if client == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "FloMorphic API access is not configured")
	}
	if a.ProcessID == 0 || !flomorphic.ProcessOpen(a.Status) {
		return fiber.NewError(fiber.StatusConflict, "this activity is not a running flow")
	}
	if _, err := client.StopProcess(c.Context(), a.ProcessID); err != nil {
		return fiber.NewError(fiber.StatusBadGateway, err.Error())
	}
	synced, err := s.syncActivity(c.Context(), a)
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, err.Error())
	}
	return c.JSON(synced)
}

// ---- running a flow on a row ----

// subjectRef names one pipeline row.
type subjectRef struct {
	Kind string `json:"kind"`
	ID   int64  `json:"id"`
}

type runFlowBody struct {
	FlowID string `json:"flowId"`
	// One subject, or several (one activity / run per subject).
	Subject  *subjectRef  `json:"subject"`
	Subjects []subjectRef `json:"subjects"`
	// Optional activity title; the flow's title otherwise.
	Title string `json:"title"`
	// Optional extra input handed to the flow as $.input.
	Input json.RawMessage `json:"input"`
	// Entry are the values filled into the flow's own entry form — the input
	// the flow declares it needs (GET /api/flows/:id/entry). They are not part
	// of the context document venapce builds: FloMorphic validates them
	// against the flow's schema and seeds them at the one path the entry names
	// (`$._entry` by default), so a flow reads its form answers where its
	// designer said they would be. One form, N subjects — every run of this
	// call is seeded with the same values.
	//
	// `input` is the older, untyped sibling: venapce's own extra input at
	// `$.input`, with nothing declaring the slot and nothing checking what
	// lands in it. It stays on the wire — an operation's entry run builds its
	// manifest params there, and an API caller may still use it — but the run
	// dialog no longer offers it to an operator: a flow that states what it
	// needs is asked for exactly that.
	Entry map[string]any `json:"entry"`
}

// POST /api/activities/run — send one or more rows through a FloMorphic flow.
// Each row becomes one run and one `run` activity; the activities are returned
// immediately (status running) and complete in the background.
func (s *Server) runFlow(c fiber.Ctx) error {
	client := s.flo.Get()
	if client == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "FloMorphic API access is not configured (Settings → FloMorphic)")
	}
	var body runFlowBody
	if err := c.Bind().Body(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	body.FlowID = strings.TrimSpace(body.FlowID)
	if body.FlowID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "flowId is required")
	}
	subjects := body.Subjects
	if body.Subject != nil {
		subjects = append([]subjectRef{*body.Subject}, subjects...)
	}
	if len(subjects) == 0 {
		return fiber.NewError(fiber.StatusBadRequest, "subject {kind,id} (or subjects[]) is required")
	}
	if len(subjects) > 200 {
		return fiber.NewError(fiber.StatusBadRequest, "at most 200 subjects per call")
	}
	for _, sub := range subjects {
		if !subjectKinds[sub.Kind] || sub.ID == 0 {
			return fiber.NewError(fiber.StatusBadRequest, "subject kind must be stage|finding|issue|operation with a non-zero id")
		}
	}
	if len(body.Input) > 0 && !json.Valid(body.Input) {
		return fiber.NewError(fiber.StatusBadRequest, "input must be valid JSON")
	}

	flow, err := client.GetFlow(c.Context(), body.FlowID)
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, "flow: "+err.Error())
	}

	out := make([]db.Activity, 0, len(subjects))
	var firstErr error
	for _, sub := range subjects {
		a, err := s.startRun(c.Context(), client, flow, sub, body.Title, body.Input, body.Entry)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if a == nil {
				continue
			}
		}
		out = append(out, *a)
	}
	if len(out) == 0 && firstErr != nil {
		return fiber.NewError(fiber.StatusBadGateway, firstErr.Error())
	}
	res := fiber.Map{"activities": out}
	if firstErr != nil {
		res["error"] = firstErr.Error()
	}
	return c.Status(fiber.StatusAccepted).JSON(res)
}

// startRun records the activity first (so its id can travel in the context
// document), then creates the document and launches the process, and finally
// writes the run's identity back onto the activity. A launch failure leaves a
// `failed` activity with the error, so the attempt itself is in the history.
//
// entry are the answers to the flow's own entry form, handed to the engine with
// the launch rather than written into the document here: FloMorphic validates
// them against the flow's schema and seeds them at the path the entry declares.
// nil is "nothing supplied", which is what a flow with no entry form gets and
// what makes an optional form fall back to its defaults; a flow whose entry is
// required refuses the launch, and that refusal lands on the activity like any
// other start failure.
func (s *Server) startRun(ctx context.Context, client *flomorphic.Client, flow flomorphic.Flow, sub subjectRef, title string, input json.RawMessage, entry map[string]any) (*db.Activity, error) {
	row, err := s.loadSubject(ctx, sub.Kind, sub.ID)
	if err != nil {
		return nil, fmt.Errorf("%s #%d: %w", sub.Kind, sub.ID, err)
	}
	now := time.Now()
	a, err := s.q.CreateActivity(ctx, db.CreateActivityParams{
		SubjectKind: sub.Kind,
		SubjectID:   sub.ID,
		Kind:        actRun,
		Status:      actRunning,
		Title:       orDefault(strings.TrimSpace(title), flow.Title),
		Facts:       json.RawMessage("[]"),
		Tags:        []string{},
		Origin:      "flow:" + flow.ID,
		FlowID:      flow.ID,
		FlowTitle:   flow.Title,
		Errors:      json.RawMessage("{}"),
		Ref:         json.RawMessage("{}"),
		Data:        json.RawMessage("{}"),
		Meta:        json.RawMessage("{}"),
		StartedAt:   &now,
	})
	if err != nil {
		return nil, err
	}

	doc := contextFor(a, row, input)
	ctxTitle := fmt.Sprintf("venapce %s #%d · %s", sub.Kind, sub.ID, flow.Title)
	contextID, err := client.CreateContext(ctx, ctxTitle, doc)
	if err != nil {
		return s.failRun(ctx, a, "", "", 0, fmt.Errorf("create context: %w", err))
	}
	proc, err := client.StartProcess(ctx, flow.ID, contextID, map[string]any{
		"venapce": map[string]any{"activityId": a.ID, "subjectKind": sub.Kind, "subjectId": sub.ID},
	}, entry)
	if err != nil {
		pid, idx := "", int64(0)
		if proc != nil {
			pid, idx = proc.PID, proc.IndexID
		}
		return s.failRun(ctx, a, contextID, pid, idx, fmt.Errorf("start process: %w", err))
	}
	a.ContextID, a.Pid, a.ProcessID = contextID, proc.PID, proc.IndexID
	a.Status = orDefault(proc.Status, actRunning)
	a.Error = proc.Error
	a.Ref = mergeJSON(a.Ref, map[string]any{"process": map[string]any{"indexId": proc.IndexID, "pid": proc.PID, "contextId": contextID}})
	saved, err := s.saveActivity(ctx, a)
	if err != nil {
		return &a, err
	}
	if !flomorphic.ProcessOpen(saved.Status) {
		// Already terminal (a synchronous failure): collect what there is.
		if synced, err := s.syncActivity(ctx, saved); err == nil {
			saved = synced
		}
	}
	return &saved, nil
}

// failRun marks a run that never got off the ground.
func (s *Server) failRun(ctx context.Context, a db.Activity, contextID, pid string, idx int64, cause error) (*db.Activity, error) {
	now := time.Now()
	a.Status, a.Error = actFailed, cause.Error()
	a.ContextID, a.Pid, a.ProcessID = contextID, pid, idx
	a.FinishedAt = &now
	saved, err := s.saveActivity(ctx, a)
	if err != nil {
		return &a, err
	}
	return &saved, cause
}

// contextFor builds the context document a run starts from — the contract a
// flow author designs against:
//
//	subject   {kind, id}                 which row this is
//	row       {…}                        the row itself (data / meta / ref / tags …)
//	activity  {id, flowId, flowTitle}    this run's record (db.activities.update id)
//	input     any                        extra input given at launch, if any
//	outcome   {}                         where the flow leaves its conclusion:
//	                                     title, description, remediation, proof,
//	                                     facts [{k,v}] | {k:v}, tags, data, meta
//
// The row's earlier activities are deliberately NOT in it. They used to ride
// along as `history` — the last 25 finished ones, each with its own prose,
// facts and `data` document — on the theory that a run could build on earlier
// runs. The cost is not theoretical: a row a dozen runs old carries more
// history than row, and every byte of it is published into the engine, held in
// the process's context for the length of the run and read back at the end, for
// data no node asked for. A flow that does want the row's past can read it —
// GET /api/activities?subjectKind=…&subjectId=… — which is a call the flows
// that need it make and the ones that do not no longer pay for.
func contextFor(a db.Activity, row map[string]any, input json.RawMessage) map[string]any {
	doc := map[string]any{
		"subject":  map[string]any{"kind": a.SubjectKind, "id": a.SubjectID},
		"row":      row,
		"activity": map[string]any{"id": a.ID, "flowId": a.FlowID, "flowTitle": a.FlowTitle},
		"outcome":  map[string]any{},
	}
	if len(input) > 0 {
		doc["input"] = json.RawMessage(input)
	}
	return doc
}

// ---- syncing a run with FloMorphic ----

// syncActivity brings a run activity up to date with its FloMorphic process:
// a no-op for anything that is not an in-flight run. When the process has
// ended, the context document is read back and its `outcome` lifted into the
// typed columns. Errors reaching FloMorphic leave the row as it was.
func (s *Server) syncActivity(ctx context.Context, a db.Activity) (db.Activity, error) {
	if a.Kind != actRun || a.ProcessID == 0 || !flomorphic.ProcessOpen(a.Status) {
		return a, nil
	}
	client := s.flo.Get()
	if client == nil {
		return a, errors.New("FloMorphic API access is not configured")
	}
	proc, err := client.GetProcess(ctx, a.ProcessID)
	if err != nil {
		var he *flomorphic.HTTPError
		if errors.As(err, &he) && he.NotFound() {
			// The run was deleted on the FloMorphic side; nothing more will come.
			now := time.Now()
			a.Status, a.Error, a.FinishedAt = actFailed, "process no longer exists on FloMorphic", &now
			return s.saveActivity(ctx, a)
		}
		return a, err
	}
	if proc.Status == a.Status || proc.Status == "" {
		return a, nil
	}
	a.Status, a.Error = proc.Status, proc.Error
	if proc.PID != "" {
		a.Pid = proc.PID
	}
	if flomorphic.ProcessOpen(proc.Status) {
		return s.saveActivity(ctx, a)
	}

	// Terminal: stamp the end and collect the outcome.
	end := time.Now()
	if proc.FinishedAt > 0 {
		end = time.UnixMilli(proc.FinishedAt)
	}
	a.FinishedAt = &end
	a.Ref = mergeJSON(a.Ref, map[string]any{"process": map[string]any{
		"indexId": proc.IndexID, "pid": proc.PID, "contextId": a.ContextID,
		"status": proc.Status, "durationMs": proc.DurationMs,
	}})
	ledger := proc.Ledger()
	if a.ContextID != "" {
		if doc, err := client.GetContext(ctx, a.ContextID); err == nil {
			liftOutcome(&a, doc.Document())
			if ledger == nil {
				ledger = flomorphic.LedgerFromHeader(doc.Header)
			}
		} else {
			a.Ref = mergeJSON(a.Ref, map[string]any{"contextError": err.Error()})
		}
	}
	liftErrors(&a, ledger)
	return s.saveActivity(ctx, a)
}

// liftErrors records the run's error ledger on the activity, and makes sure a
// run that hit something never reads as a clean one.
//
// A flow does not stop for a node error: fractal records it and carries on, so
// FloMorphic can report `finished` on a run that failed at half its nodes with
// `error` — the single fatal cause — left empty. When it is, the ledger's
// summary stands in, so every list, badge and API caller that already reads
// `error` says that something went wrong, and `errors` says what, where and
// whose it was.
func liftErrors(a *db.Activity, errs *flomorphic.RunErrors) {
	if errs == nil {
		a.Errors = json.RawMessage("{}")
		return
	}
	b, err := json.Marshal(errs)
	if err != nil {
		return
	}
	a.Errors = b
	if a.Error == "" {
		a.Error = errs.Summary()
	}
}

// activityErrors decodes the ledger stored on an activity, or nil when the run
// recorded nothing. Every view that reports a run — not only the activity's own
// page — reads it through this, so none of them has to know that a clean run is
// stored as `{}`.
func activityErrors(raw json.RawMessage) *flomorphic.RunErrors {
	if len(raw) == 0 {
		return nil
	}
	var e flomorphic.RunErrors
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil
	}
	return e.Normalized("")
}

// liftOutcome maps the run's final context document onto the activity: the
// `outcome` object's known keys become columns, and the document (minus the
// input the run started with, which venapce already holds) is kept whole in
// `data`. `history` is still trimmed although runs no longer carry one: a run
// launched before it went can still be in flight across a deploy. A key the
// flow did not set leaves the column as it is,
// so a flow that wrote the columns itself through db.activities.update is not
// overwritten with blanks.
func liftOutcome(a *db.Activity, doc map[string]any) {
	outcome, _ := doc["outcome"].(map[string]any)
	if s, ok := stringOf(outcome["title"]); ok && s != "" {
		a.Title = s
	}
	if s, ok := stringOf(outcome["description"]); ok {
		a.Description = s
	}
	if s, ok := stringOf(outcome["remediation"]); ok {
		a.Remediation = s
	}
	if s, ok := stringOf(outcome["proof"]); ok {
		a.Proof = s
	}
	// A node that failed inside an otherwise completed run leaves its error on
	// the outcome (the engine writes `error` there); surface it as the run's.
	if s, ok := outcome["error"].(string); ok && s != "" && a.Error == "" {
		a.Error = s
	}
	if v, ok := outcome["facts"]; ok && v != nil {
		if b, err := json.Marshal(v); err == nil {
			a.Facts = normalizeFacts(b)
		}
	}
	if v, ok := outcome["tags"]; ok {
		if tags := stringList(v); len(tags) > 0 {
			a.Tags = tags
		}
	}
	if v, ok := outcome["meta"]; ok && v != nil {
		if b, err := json.Marshal(v); err == nil {
			a.Meta = b
		}
	}
	// `data`: what the flow explicitly returned, else the whole document with the
	// launch-time input stripped.
	if v, ok := outcome["data"]; ok && v != nil {
		if b, err := json.Marshal(v); err == nil {
			a.Data = b
		}
		return
	}
	trimmed := make(map[string]any, len(doc))
	for k, v := range doc {
		switch k {
		case "row", "history", "subject", "activity", "_sched":
			continue
		}
		trimmed[k] = v
	}
	if b, err := json.Marshal(trimmed); err == nil {
		a.Data = b
	}
}

// stringOf renders an outcome value as text: strings as-is, anything else as
// compact JSON (a flow may leave proof as a list of CVE objects, say).
func stringOf(v any) (string, bool) {
	switch t := v.(type) {
	case nil:
		return "", false
	case string:
		return t, true
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return "", false
		}
		return string(b), true
	}
}

func stringList(v any) []string {
	out := []string{}
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	case string:
		out = splitTags(t)
	}
	return out
}

// normalizeFacts coerces whatever a producer sent into the canonical
// [{"k":…,"v":…}] list: that list itself, or an object {k: v} (keys sorted so
// the order is stable). Anything else yields an empty list.
func normalizeFacts(raw json.RawMessage) json.RawMessage {
	empty := json.RawMessage("[]")
	if len(raw) == 0 || !json.Valid(raw) {
		return empty
	}
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &list); err == nil {
		out := make([]fact, 0, len(list))
		for _, e := range list {
			var k string
			if err := json.Unmarshal(e["k"], &k); err != nil || k == "" {
				continue
			}
			v := e["v"]
			if len(v) == 0 {
				v = json.RawMessage("null")
			}
			out = append(out, fact{K: k, V: v})
		}
		b, _ := json.Marshal(out)
		return b
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err == nil {
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]fact, 0, len(keys))
		for _, k := range keys {
			out = append(out, fact{K: k, V: obj[k]})
		}
		b, _ := json.Marshal(out)
		return b
	}
	return empty
}

// mergeJSON adds keys to a JSON object document (a non-object is replaced).
func mergeJSON(cur json.RawMessage, add map[string]any) json.RawMessage {
	doc := map[string]any{}
	if len(cur) > 0 {
		_ = json.Unmarshal(cur, &doc)
	}
	for k, v := range add {
		doc[k] = v
	}
	b, _ := json.Marshal(doc)
	return b
}

// saveActivity writes every column of a back (the full-row update).
func (s *Server) saveActivity(ctx context.Context, a db.Activity) (db.Activity, error) {
	return s.q.UpdateActivity(ctx, db.UpdateActivityParams{
		ID: a.ID, SubjectKind: a.SubjectKind, SubjectID: a.SubjectID, Kind: a.Kind, Status: a.Status,
		Title: a.Title, Description: a.Description, Remediation: a.Remediation, Proof: a.Proof,
		Facts: rawOr(a.Facts, "[]"), Tags: orEmpty(a.Tags), Origin: a.Origin, FlowID: a.FlowID,
		FlowTitle: a.FlowTitle, ProcessID: a.ProcessID, Pid: a.Pid, ContextID: a.ContextID,
		Error: a.Error, Errors: rawOr(a.Errors, "{}"), Ref: rawOr(a.Ref, "{}"),
		Data: rawOr(a.Data, "{}"), Meta: rawOr(a.Meta, "{}"),
		StartedAt: a.StartedAt, FinishedAt: a.FinishedAt,
	})
}

// loadSubject reads a pipeline row as a generic document (the shape the row
// has over the API), for the context document and the detail header.
func (s *Server) loadSubject(ctx context.Context, kind string, id int64) (map[string]any, error) {
	var rec any
	var err error
	switch kind {
	case "stage":
		rec, err = s.q.GetStage(ctx, id)
	case "finding":
		rec, err = s.q.GetFinding(ctx, id)
	case "issue":
		rec, err = s.q.GetIssue(ctx, id)
	case subjectOperation:
		return s.operationDoc(ctx, id)
	default:
		return nil, fmt.Errorf("unknown subject kind %q", kind)
	}
	if err != nil {
		return nil, err
	}
	return toDoc(rec), nil
}

// toDoc round-trips a struct through JSON into a map (camelCase keys, as the
// API serves them).
func toDoc(v any) map[string]any {
	b, _ := json.Marshal(v)
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	return out
}

// ---- history of manual / pipeline changes ----

// recordChange writes a non-run activity on a subject. Errors are logged, not
// returned: history must never fail the change it describes.
func (s *Server) recordChange(ctx context.Context, kind string, id int64, actKind, title, origin string, ref map[string]any) {
	now := time.Now()
	refDoc := json.RawMessage("{}")
	if len(ref) > 0 {
		refDoc, _ = json.Marshal(ref)
	}
	_, err := s.q.CreateActivity(ctx, db.CreateActivityParams{
		SubjectKind: kind, SubjectID: id, Kind: actKind, Status: actFinished, Title: title,
		Facts: json.RawMessage("[]"), Tags: []string{}, Origin: origin,
		Errors: json.RawMessage("{}"), Ref: refDoc, Data: json.RawMessage("{}"),
		Meta: json.RawMessage("{}"), StartedAt: &now, FinishedAt: &now,
	})
	if err != nil {
		log.Printf("activities: could not record %s on %s #%d: %v", actKind, kind, id, err)
	}
}

// recordEdit diffs a row before and after a PUT and records an `edit` activity
// naming the changed fields, with each field's old and new value in ref.
func (s *Server) recordEdit(c fiber.Ctx, kind string, id int64, before, after any) {
	b, a := toDoc(before), toDoc(after)
	changed := map[string]any{}
	names := []string{}
	for k, av := range a {
		if k == "updatedAt" {
			continue
		}
		bj, _ := json.Marshal(b[k])
		aj, _ := json.Marshal(av)
		if string(bj) != string(aj) {
			changed[k] = map[string]any{"from": b[k], "to": av}
			names = append(names, k)
		}
	}
	if len(names) == 0 {
		return
	}
	sort.Strings(names)
	s.recordChange(c.Context(), kind, id, actEdit, "Edited "+strings.Join(names, ", "), changeOrigin(c), map[string]any{"changed": changed})
}

// recordCreate records that a row was created through the API.
func (s *Server) recordCreate(c fiber.Ctx, kind string, id int64) {
	s.recordChange(c.Context(), kind, id, actCreate, "Created", changeOrigin(c), nil)
}

// recordPromote records a promotion on both ends: the source row gets a
// `promote` pointing forward, the new row a `create` pointing back.
func (s *Server) recordPromote(c fiber.Ctx, fromKind string, fromID int64, toKind string, toID int64) {
	origin := changeOrigin(c)
	s.recordChange(c.Context(), fromKind, fromID, actPromote,
		fmt.Sprintf("Promoted to %s #%d", toKind, toID), origin,
		map[string]any{"to": map[string]any{"kind": toKind, "id": toID}})
	s.recordChange(c.Context(), toKind, toID, actCreate,
		fmt.Sprintf("Created from %s #%d", fromKind, fromID), origin,
		map[string]any{"from": map[string]any{"kind": fromKind, "id": fromID}})
}

// changeOrigin is who made an API change: a caller may name itself with the
// X-Venapce-Origin header (a script, a flow calling the REST API); the panel
// is "manual".
func changeOrigin(c fiber.Ctx) string {
	return orDefault(strings.TrimSpace(c.Get("X-Venapce-Origin")), "manual")
}

// forgetSubject drops a deleted row's history.
func (s *Server) forgetSubject(ctx context.Context, kind string, id int64) {
	if err := s.q.DeleteActivitiesBySubject(ctx, db.DeleteActivitiesBySubjectParams{SubjectKind: kind, SubjectID: id}); err != nil {
		log.Printf("activities: could not delete history of %s #%d: %v", kind, id, err)
	}
}

// ---- background reconciler ----

// StartActivitySync polls FloMorphic for every run still in flight and
// completes its activity when the process ends, so outcomes land even when
// nobody has the page open. Never blocks boot; idle when FloMorphic is not
// configured or nothing is running.
func (s *Server) StartActivitySync() {
	go func() {
		const every = 4 * time.Second
		for {
			time.Sleep(every)
			if s.flo.Get() == nil {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			open, err := s.q.ListOpenActivities(ctx, 100)
			if err != nil {
				log.Printf("activities: list open runs: %v", err)
				cancel()
				continue
			}
			for _, a := range open {
				if _, err := s.syncActivity(ctx, a); err != nil {
					log.Printf("activities: sync #%d (process %d): %v", a.ID, a.ProcessID, err)
				}
			}
			cancel()
		}
	}()
}
