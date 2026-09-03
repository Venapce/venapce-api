package httpapi

import (
	"encoding/json"
	"errors"
	"strconv"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"

	"github.com/Venapce/venapce-api/internal/db"
)

type chartBody struct {
	Title        string          `json:"title"`
	VizType      string          `json:"vizType"`
	QueryContext json.RawMessage `json:"queryContext"`
	BuilderState json.RawMessage `json:"builderState"`
}

func idParam(c fiber.Ctx) (int64, error) {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil {
		return 0, fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	return id, nil
}

func (s *Server) listCharts(c fiber.Ctx) error {
	charts, err := s.q.ListCharts(c.Context())
	if err != nil {
		return err
	}
	return c.JSON(charts)
}

func (s *Server) getChart(c fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	chart, err := s.q.GetChart(c.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fiber.NewError(fiber.StatusNotFound, "chart not found")
	}
	if err != nil {
		return err
	}
	return c.JSON(chart)
}

func (s *Server) createChart(c fiber.Ctx) error {
	var body chartBody
	if err := c.Bind().Body(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if body.Title == "" {
		return fiber.NewError(fiber.StatusBadRequest, "title is required")
	}
	if body.VizType == "" {
		body.VizType = "bar"
	}
	chart, err := s.q.CreateChart(c.Context(), db.CreateChartParams{
		Title:        body.Title,
		VizType:      body.VizType,
		QueryContext: rawOr(body.QueryContext, "{}"),
		BuilderState: rawOr(body.BuilderState, "{}"),
	})
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(chart)
}

func (s *Server) updateChart(c fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	var body chartBody
	if err := c.Bind().Body(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if body.VizType == "" {
		body.VizType = "bar"
	}
	chart, err := s.q.UpdateChart(c.Context(), db.UpdateChartParams{
		ID:           id,
		Title:        body.Title,
		VizType:      body.VizType,
		QueryContext: rawOr(body.QueryContext, "{}"),
		BuilderState: rawOr(body.BuilderState, "{}"),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return fiber.NewError(fiber.StatusNotFound, "chart not found")
	}
	if err != nil {
		return err
	}
	return c.JSON(chart)
}

func (s *Server) deleteChart(c fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	if err := s.q.DeleteChart(c.Context(), id); err != nil {
		return err
	}
	return c.SendStatus(fiber.StatusNoContent)
}
