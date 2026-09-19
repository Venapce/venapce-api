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

// subjectKinds are the tables an activity can be about.
var subjectKinds = map[string]bool{"stage": true, "finding": true, "issue": true}

// fact is one typed key/value of an outcome: [{"k":"severity","v":"low"}].
// `v` is any JSON value — a string usually, but a number, list or object is
// kept as-is.
type fact struct {
	K string          `json:"k"`
	V json.RawMessage `json:"v"`
}

// ---- HTTP: flows (picker) ----

// GET /api/flows?search= — the FloMorphic flows an operator can run a row
// through. Needs the FloMorphic API access from Settings.
func (s *Server) listFlows(c fiber.Ctx) error {
	client := s.flo.Get()
	if client == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "FloMorphic API access is not configured (Settings → FloMorphic)")
	}
	flows, err := client.ListFlows(c.Context(), c.Query("search"), 200)
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, err.Error())
	}
	return c.JSON(flows)
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
			return fiber.NewError(fiber.StatusBadRequest, "subject kind must be stage|finding|issue with a non-zero id")
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
		a, err := s.startRun(c.Context(), client, flow, sub, body.Title, body.Input)
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
func (s *Server) startRun(ctx context.Context, client *flomorphic.Client, flow flomorphic.Flow, sub subjectRef, title string, input json.RawMessage) (*db.Activity, error) {
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
		Ref:         json.RawMessage("{}"),
		Data:        json.RawMessage("{}"),
		Meta:        json.RawMessage("{}"),
		StartedAt:   &now,
	})
	if err != nil {
		return nil, err
	}

	doc, err := s.contextFor(ctx, a, row, input)
	if err != nil {
		return s.failRun(ctx, a, "", "", 0, err)
	}
	ctxTitle := fmt.Sprintf("venapce %s #%d · %s", sub.Kind, sub.ID, flow.Title)
	contextID, err := client.CreateContext(ctx, ctxTitle, doc)
	if err != nil {
		return s.failRun(ctx, a, "", "", 0, fmt.Errorf("create context: %w", err))
	}
	proc, err := client.StartProcess(ctx, flow.ID, contextID, map[string]any{
		"venapce": map[string]any{"activityId": a.ID, "subjectKind": sub.Kind, "subjectId": sub.ID},
	})
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
//	history   [{…}]                      earlier finished activities on the row,
//	                                     newest first (so runs can build on runs)
//	input     any                        extra input given at launch, if any
//	outcome   {}                         where the flow leaves its conclusion:
//	                                     title, description, remediation, proof,
//	                                     facts [{k,v}] | {k:v}, tags, data, meta
func (s *Server) contextFor(ctx context.Context, a db.Activity, row map[string]any, input json.RawMessage) (map[string]any, error) {
	prior, err := s.q.ListActivitiesBySubject(ctx, db.ListActivitiesBySubjectParams{SubjectKind: a.SubjectKind, SubjectID: a.SubjectID})
	if err != nil {
		return nil, err
	}
	history := make([]map[string]any, 0, len(prior))
	for _, p := range prior {
		if p.ID == a.ID || flomorphic.ProcessOpen(p.Status) {
			continue
		}
		history = append(history, map[string]any{
			"id": p.ID, "kind": p.Kind, "status": p.Status, "title": p.Title,
			"description": p.Description, "remediation": p.Remediation, "proof": p.Proof,
			"facts": p.Facts, "tags": p.Tags, "origin": p.Origin, "flowId": p.FlowID,
			"flowTitle": p.FlowTitle, "finishedAt": p.FinishedAt, "data": p.Data,
		})
		if len(history) >= 25 {
			break
		}
	}
	doc := map[string]any{
		"subject":  map[string]any{"kind": a.SubjectKind, "id": a.SubjectID},
		"row":      row,
		"activity": map[string]any{"id": a.ID, "flowId": a.FlowID, "flowTitle": a.FlowTitle},
		"history":  history,
		"outcome":  map[string]any{},
	}
	if len(input) > 0 {
		doc["input"] = json.RawMessage(input)
	}
	return doc, nil
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
	if a.ContextID != "" {
		if doc, err := client.GetContext(ctx, a.ContextID); err == nil {
			liftOutcome(&a, doc.Document())
		} else {
			a.Ref = mergeJSON(a.Ref, map[string]any{"contextError": err.Error()})
		}
	}
	return s.saveActivity(ctx, a)
}

// liftOutcome maps the run's final context document onto the activity: the
// `outcome` object's known keys become columns, and the document (minus the
// input the run started with — the row and history venapce already holds) is
// kept whole in `data`. A key the flow did not set leaves the column as it is,
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
		Error: a.Error, Ref: rawOr(a.Ref, "{}"), Data: rawOr(a.Data, "{}"), Meta: rawOr(a.Meta, "{}"),
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
		Ref: refDoc, Data: json.RawMessage("{}"), Meta: json.RawMessage("{}"),
		StartedAt: &now, FinishedAt: &now,
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
