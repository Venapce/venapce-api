package httpapi

import (
	"errors"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"

	"github.com/Venapce/venapce-api/internal/db"
)

// Saved views: a named tag filter over one of the pipeline tables. This is how
// an operator adds a sidebar entry without a new table — "Untrusted" over
// issues, "Audit runs" over activities — and the list endpoints already take
// the same tags/match parameters the view stores.
//
// The panel speaks of a view's `table` and `match`; the columns are `target`
// and `match_mode` (`table` is reserved SQL), so the row is mapped on the way
// out rather than leaking column names into the API.

// The tables a view may filter — the pipeline levels plus their history.
var viewTargets = map[string]bool{"stage": true, "findings": true, "issues": true, "activities": true}

type viewView struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Table     string    `json:"table"`
	Tags      []string  `json:"tags"`
	Match     string    `json:"match"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func toViewView(v db.View) viewView {
	return viewView{
		ID:        v.ID,
		Name:      v.Name,
		Table:     v.Target,
		Tags:      orEmpty(v.Tags),
		Match:     v.MatchMode,
		CreatedAt: v.CreatedAt,
		UpdatedAt: v.UpdatedAt,
	}
}

// viewBody is the create/update payload. Pointers make a partial update
// possible: an absent field keeps its stored value.
type viewBody struct {
	Name  *string   `json:"name"`
	Table *string   `json:"table"`
	Tags  *[]string `json:"tags"`
	Match *string   `json:"match"`
}

// validate checks the two closed vocabularies a view carries.
func validateView(target, match string) error {
	if !viewTargets[target] {
		return fiber.NewError(fiber.StatusBadRequest, "table must be one of stage, findings, issues, activities")
	}
	if match != "any" && match != "all" {
		return fiber.NewError(fiber.StatusBadRequest, "match must be 'any' or 'all'")
	}
	return nil
}

// GET /api/views?table= — every saved view, or those of one table.
func (s *Server) listViews(c fiber.Ctx) error {
	target := c.Query("table")
	if target != "" && !viewTargets[target] {
		return fiber.NewError(fiber.StatusBadRequest, "unknown table")
	}
	rows, err := s.q.ListViews(c.Context(), target)
	if err != nil {
		return err
	}
	out := make([]viewView, 0, len(rows))
	for _, v := range rows {
		out = append(out, toViewView(v))
	}
	return c.JSON(out)
}

// GET /api/views/:id
func (s *Server) getView(c fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	v, err := s.q.GetView(c.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fiber.NewError(fiber.StatusNotFound, "view not found")
	}
	if err != nil {
		return err
	}
	return c.JSON(toViewView(v))
}

// POST /api/views — name + table + tags define the view.
func (s *Server) createView(c fiber.Ctx) error {
	var body viewBody
	if err := c.Bind().Body(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	name := pick("", body.Name)
	if name == "" {
		return fiber.NewError(fiber.StatusBadRequest, "name is required")
	}
	tags := pickTags(nil, body.Tags)
	if len(tags) == 0 {
		return fiber.NewError(fiber.StatusBadRequest, "at least one tag is required")
	}
	target := orDefault(pick("", body.Table), "issues")
	match := orDefault(pick("", body.Match), "any")
	if err := validateView(target, match); err != nil {
		return err
	}
	v, err := s.q.CreateView(c.Context(), db.CreateViewParams{
		Name:      name,
		Target:    target,
		Tags:      tags,
		MatchMode: match,
	})
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(toViewView(v))
}

// PUT /api/views/:id — partial: absent fields keep their stored value.
func (s *Server) updateView(c fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	var body viewBody
	if err := c.Bind().Body(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	cur, err := s.q.GetView(c.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fiber.NewError(fiber.StatusNotFound, "view not found")
	}
	if err != nil {
		return err
	}
	name := pick(cur.Name, body.Name)
	if name == "" {
		return fiber.NewError(fiber.StatusBadRequest, "name is required")
	}
	tags := pickTags(cur.Tags, body.Tags)
	if len(tags) == 0 {
		return fiber.NewError(fiber.StatusBadRequest, "at least one tag is required")
	}
	target := pick(cur.Target, body.Table)
	match := pick(cur.MatchMode, body.Match)
	if err := validateView(target, match); err != nil {
		return err
	}
	v, err := s.q.UpdateView(c.Context(), db.UpdateViewParams{
		ID:        id,
		Name:      name,
		Target:    target,
		Tags:      tags,
		MatchMode: match,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return fiber.NewError(fiber.StatusNotFound, "view not found")
	}
	if err != nil {
		return err
	}
	return c.JSON(toViewView(v))
}

// DELETE /api/views/:id
func (s *Server) deleteView(c fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	if err := s.q.DeleteView(c.Context(), id); err != nil {
		return err
	}
	return c.SendStatus(fiber.StatusNoContent)
}
