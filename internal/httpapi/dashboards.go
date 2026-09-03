package httpapi

import (
	"encoding/json"
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"

	"github.com/Venapce/venapce-api/internal/db"
)

type dashboardBody struct {
	Title  string          `json:"title"`
	Slug   string          `json:"slug"`
	Layout json.RawMessage `json:"layout"`
}

func (s *Server) listDashboards(c fiber.Ctx) error {
	dashboards, err := s.q.ListDashboards(c.Context())
	if err != nil {
		return err
	}
	return c.JSON(dashboards)
}

func (s *Server) getDashboard(c fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	d, err := s.q.GetDashboard(c.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fiber.NewError(fiber.StatusNotFound, "dashboard not found")
	}
	if err != nil {
		return err
	}
	return c.JSON(d)
}

func (s *Server) createDashboard(c fiber.Ctx) error {
	var body dashboardBody
	if err := c.Bind().Body(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if body.Title == "" {
		return fiber.NewError(fiber.StatusBadRequest, "title is required")
	}
	d, err := s.q.CreateDashboard(c.Context(), db.CreateDashboardParams{
		Title:  body.Title,
		Slug:   body.Slug,
		Layout: rawOr(body.Layout, "[]"),
	})
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(d)
}

func (s *Server) updateDashboard(c fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	var body dashboardBody
	if err := c.Bind().Body(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	d, err := s.q.UpdateDashboard(c.Context(), db.UpdateDashboardParams{
		ID:     id,
		Title:  body.Title,
		Slug:   body.Slug,
		Layout: rawOr(body.Layout, "[]"),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return fiber.NewError(fiber.StatusNotFound, "dashboard not found")
	}
	if err != nil {
		return err
	}
	return c.JSON(d)
}

func (s *Server) deleteDashboard(c fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	if err := s.q.DeleteDashboard(c.Context(), id); err != nil {
		return err
	}
	return c.SendStatus(fiber.StatusNoContent)
}
