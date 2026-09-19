package httpapi

import (
	"encoding/json"
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"

	"github.com/Venapce/venapce-api/internal/db"
)

// GET /api/issues?tags=&match=&status=&severity=&search=
func (s *Server) listIssues(c fiber.Ctx) error {
	issues, err := s.q.ListIssues(c.Context(), db.ListIssuesParams{
		Tags:     splitTags(c.Query("tags")),
		MatchAll: c.Query("match") == "all",
		Status:   c.Query("status"),
		Severity: c.Query("severity"),
		Search:   c.Query("search"),
	})
	if err != nil {
		return err
	}
	return c.JSON(issues)
}

// GET /api/issues/tags — distinct tags for the view-builder tag picker.
func (s *Server) issueTags(c fiber.Ctx) error {
	tags, err := s.q.IssueTags(c.Context())
	if err != nil {
		return err
	}
	return c.JSON(orEmpty(tags))
}

// GET /api/issues/:id — the issue plus what it was promoted from (the finding
// and/or staged row) and every finding that points at it.
func (s *Server) getIssue(c fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	issue, err := s.q.GetIssue(c.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fiber.NewError(fiber.StatusNotFound, "issue not found")
	}
	if err != nil {
		return err
	}
	findings, err := s.q.ListFindingsByIssue(c.Context(), id)
	if err != nil {
		return err
	}
	out := fiber.Map{"item": issue, "findings": findings, "stage": nil}
	if issue.StageID != 0 {
		if st, err := s.q.GetStage(c.Context(), issue.StageID); err == nil {
			out["stage"] = st
		}
	}
	return c.JSON(out)
}

// issueBody is the create/update payload; pointers make partial updates
// possible (absent = keep).
type issueBody struct {
	Title     *string         `json:"title"`
	Summary   *string         `json:"summary"`
	Status    *string         `json:"status"`
	Severity  *string         `json:"severity"`
	Tags      *[]string       `json:"tags"`
	Source    *string         `json:"source"`
	Origin    *string         `json:"origin"`
	Assignee  *string         `json:"assignee"`
	FindingID *int64          `json:"findingId"`
	StageID   *int64          `json:"stageId"`
	Ref       json.RawMessage `json:"ref"`
	Data      json.RawMessage `json:"data"`
	Meta      json.RawMessage `json:"meta"`
}

func (b issueBody) jsonFields() map[string]json.RawMessage {
	return map[string]json.RawMessage{"ref": b.Ref, "data": b.Data, "meta": b.Meta}
}

// POST /api/issues — create an issue directly (FloMorphic, or a manual entry).
func (s *Server) createIssue(c fiber.Ctx) error {
	var body issueBody
	if err := c.Bind().Body(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if pick("", body.Title) == "" {
		return fiber.NewError(fiber.StatusBadRequest, "title is required")
	}
	if err := validJSONFields(body.jsonFields()); err != nil {
		return err
	}
	issue, err := s.q.CreateIssue(c.Context(), db.CreateIssueParams{
		Title:     *body.Title,
		Summary:   pick("", body.Summary),
		Status:    orDefault(pick("", body.Status), "open"),
		Severity:  orDefault(pick("", body.Severity), "info"),
		Tags:      pickTags(nil, body.Tags),
		Source:    pick("", body.Source),
		Origin:    pick("", body.Origin),
		Assignee:  pick("", body.Assignee),
		FindingID: pickInt(0, body.FindingID),
		StageID:   pickInt(0, body.StageID),
		Ref:       pickJSON(nil, body.Ref),
		Data:      pickJSON(nil, body.Data),
		Meta:      pickJSON(nil, body.Meta),
	})
	if err != nil {
		return err
	}
	s.recordCreate(c, "issue", issue.ID)
	return c.Status(fiber.StatusCreated).JSON(issue)
}

// PUT /api/issues/:id — partial update.
func (s *Server) updateIssue(c fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	var body issueBody
	if err := c.Bind().Body(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if err := validJSONFields(body.jsonFields()); err != nil {
		return err
	}
	cur, err := s.q.GetIssue(c.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fiber.NewError(fiber.StatusNotFound, "issue not found")
	}
	if err != nil {
		return err
	}
	issue, err := s.q.UpdateIssue(c.Context(), db.UpdateIssueParams{
		ID:        id,
		Title:     orDefault(pick(cur.Title, body.Title), cur.Title),
		Summary:   pick(cur.Summary, body.Summary),
		Status:    orDefault(pick(cur.Status, body.Status), "open"),
		Severity:  orDefault(pick(cur.Severity, body.Severity), "info"),
		Tags:      pickTags(cur.Tags, body.Tags),
		Source:    pick(cur.Source, body.Source),
		Origin:    pick(cur.Origin, body.Origin),
		Assignee:  pick(cur.Assignee, body.Assignee),
		FindingID: pickInt(cur.FindingID, body.FindingID),
		StageID:   pickInt(cur.StageID, body.StageID),
		Ref:       pickJSON(cur.Ref, body.Ref),
		Data:      pickJSON(cur.Data, body.Data),
		Meta:      pickJSON(cur.Meta, body.Meta),
	})
	if err != nil {
		return err
	}
	s.recordEdit(c, "issue", id, cur, issue)
	return c.JSON(issue)
}

// DELETE /api/issues/:id
func (s *Server) deleteIssue(c fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	if err := s.q.DeleteIssue(c.Context(), id); err != nil {
		return err
	}
	s.forgetSubject(c.Context(), "issue", id)
	return c.SendStatus(fiber.StatusNoContent)
}
