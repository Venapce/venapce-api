package httpapi

import (
	"encoding/json"
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"

	"github.com/Venapce/venapce-api/internal/db"
)

// Findings — the middle level of the (optional) stage → findings → issues
// pipeline: what a process concluded from data, with a severity, a confidence
// and a target, still to be validated. Every finding carries where it came
// from (source), which process made it (origin) and how (ref).

// GET /api/findings?tags=&match=&status=&severity=&category=&source=&target=&search=
func (s *Server) listFindings(c fiber.Ctx) error {
	items, err := s.q.ListFindings(c.Context(), db.ListFindingsParams{
		Tags:     splitTags(c.Query("tags")),
		MatchAll: c.Query("match") == "all",
		Status:   c.Query("status"),
		Severity: c.Query("severity"),
		Category: c.Query("category"),
		Source:   c.Query("source"),
		Target:   c.Query("target"),
		Search:   c.Query("search"),
	})
	if err != nil {
		return err
	}
	return c.JSON(items)
}

// GET /api/findings/tags — distinct tags, for filter chips / pickers.
func (s *Server) findingTags(c fiber.Ctx) error {
	tags, err := s.q.FindingTags(c.Context())
	if err != nil {
		return err
	}
	return c.JSON(orEmpty(tags))
}

// GET /api/findings/:id — the finding plus its neighbours in the pipeline (the
// staged row it came from, the issue it became) for the detail page.
func (s *Server) getFinding(c fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	f, err := s.q.GetFinding(c.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fiber.NewError(fiber.StatusNotFound, "finding not found")
	}
	if err != nil {
		return err
	}
	out := fiber.Map{"item": f, "stage": nil, "issue": nil}
	if f.StageID != 0 {
		if st, err := s.q.GetStage(c.Context(), f.StageID); err == nil {
			out["stage"] = st
		}
	}
	if f.IssueID != 0 {
		if is, err := s.q.GetIssue(c.Context(), f.IssueID); err == nil {
			out["issue"] = is
		}
	}
	return c.JSON(out)
}

// findingBody is the create/update payload; pointers make partial updates
// possible (absent = keep). It is also the override body for promotions.
type findingBody struct {
	Title       *string         `json:"title"`
	Summary     *string         `json:"summary"`
	Status      *string         `json:"status"`
	Severity    *string         `json:"severity"`
	Confidence  *string         `json:"confidence"`
	Category    *string         `json:"category"`
	Tags        *[]string       `json:"tags"`
	Source      *string         `json:"source"`
	Origin      *string         `json:"origin"`
	Target      *string         `json:"target"`
	Fingerprint *string         `json:"fingerprint"`
	Assignee    *string         `json:"assignee"` // only meaningful when promoting to an issue
	StageID     *int64          `json:"stageId"`
	IssueID     *int64          `json:"issueId"`
	Ref         json.RawMessage `json:"ref"`
	Data        json.RawMessage `json:"data"`
	Meta        json.RawMessage `json:"meta"`
}

func (b findingBody) jsonFields() map[string]json.RawMessage {
	return map[string]json.RawMessage{"ref": b.Ref, "data": b.Data, "meta": b.Meta}
}

// POST /api/findings — a flow (or an operator) records a finding directly.
func (s *Server) createFinding(c fiber.Ctx) error {
	var body findingBody
	if err := c.Bind().Body(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if pick("", body.Title) == "" {
		return fiber.NewError(fiber.StatusBadRequest, "title is required")
	}
	if err := validJSONFields(body.jsonFields()); err != nil {
		return err
	}
	f, err := s.q.CreateFinding(c.Context(), db.CreateFindingParams{
		Title:       *body.Title,
		Summary:     pick("", body.Summary),
		Status:      orDefault(pick("", body.Status), "new"),
		Severity:    orDefault(pick("", body.Severity), "info"),
		Confidence:  pick("", body.Confidence),
		Category:    pick("", body.Category),
		Tags:        pickTags(nil, body.Tags),
		Source:      pick("", body.Source),
		Origin:      pick("", body.Origin),
		Target:      pick("", body.Target),
		Fingerprint: pick("", body.Fingerprint),
		StageID:     pickInt(0, body.StageID),
		IssueID:     pickInt(0, body.IssueID),
		Ref:         pickJSON(nil, body.Ref),
		Data:        pickJSON(nil, body.Data),
		Meta:        pickJSON(nil, body.Meta),
	})
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(f)
}

// PUT /api/findings/:id — partial update.
func (s *Server) updateFinding(c fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	var body findingBody
	if err := c.Bind().Body(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if err := validJSONFields(body.jsonFields()); err != nil {
		return err
	}
	cur, err := s.q.GetFinding(c.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fiber.NewError(fiber.StatusNotFound, "finding not found")
	}
	if err != nil {
		return err
	}
	f, err := s.q.UpdateFinding(c.Context(), db.UpdateFindingParams{
		ID:          id,
		Title:       orDefault(pick(cur.Title, body.Title), cur.Title),
		Summary:     pick(cur.Summary, body.Summary),
		Status:      orDefault(pick(cur.Status, body.Status), "new"),
		Severity:    orDefault(pick(cur.Severity, body.Severity), "info"),
		Confidence:  pick(cur.Confidence, body.Confidence),
		Category:    pick(cur.Category, body.Category),
		Tags:        pickTags(cur.Tags, body.Tags),
		Source:      pick(cur.Source, body.Source),
		Origin:      pick(cur.Origin, body.Origin),
		Target:      pick(cur.Target, body.Target),
		Fingerprint: pick(cur.Fingerprint, body.Fingerprint),
		StageID:     pickInt(cur.StageID, body.StageID),
		IssueID:     pickInt(cur.IssueID, body.IssueID),
		Ref:         pickJSON(cur.Ref, body.Ref),
		Data:        pickJSON(cur.Data, body.Data),
		Meta:        pickJSON(cur.Meta, body.Meta),
	})
	if err != nil {
		return err
	}
	return c.JSON(f)
}

// DELETE /api/findings/:id
func (s *Server) deleteFinding(c fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	if err := s.q.DeleteFinding(c.Context(), id); err != nil {
		return err
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// POST /api/findings/:id/promote — the finding was validated: open an issue
// from it. The body may override the issue's fields; the rest is inherited,
// and the issue's `ref` records the finding it came from. The finding is
// marked promoted and linked to the issue.
func (s *Server) promoteFinding(c fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	f, err := s.q.GetFinding(c.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fiber.NewError(fiber.StatusNotFound, "finding not found")
	}
	if err != nil {
		return err
	}
	var body findingBody
	_ = c.Bind().Body(&body) // optional overrides

	tags := append(append([]string{}, f.Tags...), pickTags(nil, body.Tags)...)
	issue, err := s.q.CreateIssue(c.Context(), db.CreateIssueParams{
		Title:     orDefault(pick("", body.Title), f.Title),
		Summary:   orDefault(pick("", body.Summary), f.Summary),
		Status:    orDefault(pick("", body.Status), "open"),
		Severity:  orDefault(pick("", body.Severity), f.Severity),
		Tags:      tags,
		Source:    orDefault(pick("", body.Source), f.Source),
		Origin:    orDefault(pick("", body.Origin), "finding:promote"),
		Assignee:  pick("", body.Assignee),
		FindingID: f.ID,
		StageID:   f.StageID,
		Ref:       pickJSON(provenance("finding", f.ID, f.Origin, f.Ref), body.Ref),
		Data:      pickJSON(f.Data, body.Data),
		Meta:      pickJSON(f.Meta, body.Meta),
	})
	if err != nil {
		return err
	}
	promoted, err := s.q.PromoteFinding(c.Context(), db.PromoteFindingParams{ID: id, IssueID: issue.ID})
	if err != nil {
		return err
	}
	// Keep the staged row's forward link complete when the chain started there.
	if f.StageID != 0 {
		_, _ = s.q.PromoteStage(c.Context(), db.PromoteStageParams{ID: f.StageID, IssueID: issue.ID})
	}
	return c.JSON(fiber.Map{"finding": promoted, "issue": issue})
}
