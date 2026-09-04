package httpapi

import (
	"encoding/json"
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"

	"github.com/Venapce/venapce-api/internal/db"
)

// GET /api/stage?disposition=&source=&search=
func (s *Server) listStage(c fiber.Ctx) error {
	items, err := s.q.ListStage(c.Context(), db.ListStageParams{
		Disposition: c.Query("disposition"),
		Source:      c.Query("source"),
		Search:      c.Query("search"),
	})
	if err != nil {
		return err
	}
	return c.JSON(items)
}

type stageBody struct {
	Title       string          `json:"title"`
	Summary     string          `json:"summary"`
	Source      string          `json:"source"`
	Disposition string          `json:"disposition"`
	Tags        []string        `json:"tags"`
	Data        json.RawMessage `json:"data"`
}

// POST /api/stage — a pipeline drops raw data into the inbox (FloMorphic ingest).
func (s *Server) createStage(c fiber.Ctx) error {
	var body stageBody
	if err := c.Bind().Body(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	item, err := s.q.CreateStage(c.Context(), db.CreateStageParams{
		Title:       body.Title,
		Summary:     body.Summary,
		Source:      body.Source,
		Disposition: orDefault(body.Disposition, "pending"),
		Tags:        orEmpty(body.Tags),
		Data:        rawOr(body.Data, "{}"),
	})
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(item)
}

// POST /api/stage/:id/promote — the flow decided this row meets its criteria:
// create an issue from it and mark the staged row promoted. Body may carry the
// issue's severity/status/extra tags; everything else is inherited from the row.
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

	var body issueBody
	_ = c.Bind().Body(&body) // body is optional

	tags := stage.Tags
	tags = append(tags, orEmpty(body.Tags)...)

	issue, err := s.q.CreateIssue(c.Context(), db.CreateIssueParams{
		Title:    orDefault(body.Title, orDefault(stage.Title, "Promoted from stage")),
		Summary:  orDefault(body.Summary, stage.Summary),
		Status:   orDefault(body.Status, "open"),
		Severity: orDefault(body.Severity, "info"),
		Tags:     orEmpty(tags),
		Source:   orDefault(body.Source, stage.Source),
		Assignee: body.Assignee,
		Data:     rawOr(body.Data, string(rawOr(stage.Data, "{}"))),
	})
	if err != nil {
		return err
	}

	promoted, err := s.q.PromoteStage(c.Context(), db.PromoteStageParams{ID: id, IssueID: issue.ID})
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"stage": promoted, "issue": issue})
}
