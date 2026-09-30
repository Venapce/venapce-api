package httpapi

import (
	"encoding/json"
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"

	"github.com/Venapce/venapce-api/internal/db"
)

// GET /api/stage?tags=&match=&disposition=&source=&search=
func (s *Server) listStage(c fiber.Ctx) error {
	items, err := s.q.ListStage(c.Context(), db.ListStageParams{
		Tags:        splitTags(c.Query("tags")),
		MatchAll:    c.Query("match") == "all",
		Disposition: c.Query("disposition"),
		Source:      c.Query("source"),
		Search:      c.Query("search"),
	})
	if err != nil {
		return err
	}
	return c.JSON(items)
}

// GET /api/stage/tags — distinct tags for the view-builder tag picker.
func (s *Server) stageTags(c fiber.Ctx) error {
	tags, err := s.q.StageTags(c.Context())
	if err != nil {
		return err
	}
	return c.JSON(orEmpty(tags))
}

// GET /api/stage/:id — one staged row plus what it turned into, so the detail
// page can link forward without extra round-trips.
func (s *Server) getStage(c fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	item, err := s.q.GetStage(c.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fiber.NewError(fiber.StatusNotFound, "stage item not found")
	}
	if err != nil {
		return err
	}
	findings, err := s.q.ListFindingsByStage(c.Context(), id)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"item": item, "findings": findings})
}

// stageBody is the create/update payload. Pointers make a partial update
// possible: an absent field keeps its stored value.
type stageBody struct {
	Title       *string         `json:"title"`
	Summary     *string         `json:"summary"`
	Source      *string         `json:"source"`
	Origin      *string         `json:"origin"`
	Disposition *string         `json:"disposition"`
	FindingID   *int64          `json:"findingId"`
	IssueID     *int64          `json:"issueId"`
	Tags        *[]string       `json:"tags"`
	Ref         json.RawMessage `json:"ref"`
	Data        json.RawMessage `json:"data"`
	Meta        json.RawMessage `json:"meta"`
}

func (b stageBody) jsonFields() map[string]json.RawMessage {
	return map[string]json.RawMessage{"ref": b.Ref, "data": b.Data, "meta": b.Meta}
}

// POST /api/stage — a pipeline drops raw data into the inbox (FloMorphic ingest).
func (s *Server) createStage(c fiber.Ctx) error {
	var body stageBody
	if err := c.Bind().Body(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if err := validJSONFields(body.jsonFields()); err != nil {
		return err
	}
	item, err := s.q.CreateStage(c.Context(), db.CreateStageParams{
		Title:       pick("", body.Title),
		Summary:     pick("", body.Summary),
		Source:      pick("", body.Source),
		Origin:      pick("", body.Origin),
		Disposition: orDefault(pick("", body.Disposition), "pending"),
		Tags:        pickTags(nil, body.Tags),
		Ref:         pickJSON(nil, body.Ref),
		Data:        pickJSON(nil, body.Data),
		Meta:        pickJSON(nil, body.Meta),
	})
	if err != nil {
		return err
	}
	s.recordCreate(c, "stage", item.ID)
	return c.Status(fiber.StatusCreated).JSON(item)
}

// PUT /api/stage/:id — partial update: only the fields in the body change.
func (s *Server) updateStage(c fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	var body stageBody
	if err := c.Bind().Body(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if err := validJSONFields(body.jsonFields()); err != nil {
		return err
	}
	cur, err := s.q.GetStage(c.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fiber.NewError(fiber.StatusNotFound, "stage item not found")
	}
	if err != nil {
		return err
	}
	item, err := s.q.UpdateStage(c.Context(), db.UpdateStageParams{
		ID:          id,
		Title:       pick(cur.Title, body.Title),
		Summary:     pick(cur.Summary, body.Summary),
		Source:      pick(cur.Source, body.Source),
		Origin:      pick(cur.Origin, body.Origin),
		Disposition: orDefault(pick(cur.Disposition, body.Disposition), "pending"),
		FindingID:   pickInt(cur.FindingID, body.FindingID),
		IssueID:     pickInt(cur.IssueID, body.IssueID),
		Tags:        pickTags(cur.Tags, body.Tags),
		Ref:         pickJSON(cur.Ref, body.Ref),
		Data:        pickJSON(cur.Data, body.Data),
		Meta:        pickJSON(cur.Meta, body.Meta),
	})
	if err != nil {
		return err
	}
	s.recordEdit(c, "stage", id, cur, item)
	return c.JSON(item)
}

// DELETE /api/stage/:id
func (s *Server) deleteStage(c fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	if err := s.q.DeleteStage(c.Context(), id); err != nil {
		return err
	}
	s.forgetSubject(c.Context(), "stage", id)
	return c.SendStatus(fiber.StatusNoContent)
}

// POST /api/stage/:id/promote?to=finding|issue — the flow (or an operator)
// decided this row meets its criteria. `to=finding` (the usual next level)
// creates a finding from it; `to=issue` (default, the historical behaviour)
// jumps straight to an issue. Either way the staged row is marked promoted and
// linked. The body may override any field of the new row; everything else is
// inherited from the staged row, and provenance (`ref`) records the stage id.
func (s *Server) promoteStage(c fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	stage, err := s.q.GetStage(c.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fiber.NewError(fiber.StatusNotFound, "stage item not found")
	}
	if err != nil {
		return err
	}

	var body struct {
		To string `json:"to"`
		findingBody
	}
	_ = c.Bind().Body(&body) // body is optional
	to, err := promoteTarget(c, body.To)
	if err != nil {
		return err
	}

	tags := append(append([]string{}, stage.Tags...), pickTags(nil, body.Tags)...)
	ref := pickJSON(provenance("stage", stage.ID, stage.Origin, stage.Ref), body.Ref)

	if to == "finding" {
		finding, err := s.q.CreateFinding(c.Context(), db.CreateFindingParams{
			Title:       orDefault(pick("", body.Title), orDefault(stage.Title, "Promoted from stage")),
			Summary:     orDefault(pick("", body.Summary), stage.Summary),
			Status:      orDefault(pick("", body.Status), "new"),
			Severity:    orDefault(pick("", body.Severity), "info"),
			Confidence:  pick("", body.Confidence),
			Category:    pick("", body.Category),
			Tags:        tags,
			Source:      orDefault(pick("", body.Source), stage.Source),
			Origin:      orDefault(pick("", body.Origin), "stage:promote"),
			Target:      pick("", body.Target),
			Fingerprint: pick("", body.Fingerprint),
			StageID:     stage.ID,
			Ref:         ref,
			Data:        pickJSON(stage.Data, body.Data),
			Meta:        pickJSON(stage.Meta, body.Meta),
		})
		if err != nil {
			return err
		}
		promoted, err := s.q.PromoteStage(c.Context(), db.PromoteStageParams{ID: id, FindingID: finding.ID})
		if err != nil {
			return err
		}
		s.recordPromote(c, "stage", id, "finding", finding.ID)
		return c.JSON(fiber.Map{"stage": promoted, "finding": finding})
	}

	issue, err := s.q.CreateIssue(c.Context(), db.CreateIssueParams{
		Title:    orDefault(pick("", body.Title), orDefault(stage.Title, "Promoted from stage")),
		Summary:  orDefault(pick("", body.Summary), stage.Summary),
		Status:   orDefault(pick("", body.Status), "open"),
		Severity: orDefault(pick("", body.Severity), "info"),
		Tags:     tags,
		Source:   orDefault(pick("", body.Source), stage.Source),
		Origin:   orDefault(pick("", body.Origin), "stage:promote"),
		Assignee: pick("", body.Assignee),
		StageID:  stage.ID,
		Ref:      ref,
		Data:     pickJSON(stage.Data, body.Data),
		Meta:     pickJSON(stage.Meta, body.Meta),
	})
	if err != nil {
		return err
	}
	promoted, err := s.q.PromoteStage(c.Context(), db.PromoteStageParams{ID: id, IssueID: issue.ID})
	if err != nil {
		return err
	}
	s.recordPromote(c, "stage", id, "issue", issue.ID)
	return c.JSON(fiber.Map{"stage": promoted, "issue": issue})
}

// provenance builds the `ref` document of a promoted row: which row it was
// made from, that row's own origin, and that row's own ref (so the chain is
// walkable from any level without a join).
func provenance(kind string, id int64, origin string, parentRef json.RawMessage) json.RawMessage {
	doc := map[string]any{
		"promotedFrom": map[string]any{"kind": kind, "id": id, "origin": origin},
	}
	if len(parentRef) > 0 && string(parentRef) != "{}" && json.Valid(parentRef) {
		doc["promotedFrom"].(map[string]any)["ref"] = json.RawMessage(parentRef)
	}
	b, _ := json.Marshal(doc)
	return b
}
