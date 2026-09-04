package httpapi

import (
	"encoding/json"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/Venapce/venapce-api/internal/db"
)

// splitTags turns a comma-separated tag list ("untrusted,network") into a slice,
// trimming blanks. Always returns a non-nil slice (sqlc array params want one).
func splitTags(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// GET /api/issues?tags=&match=&status=&search=
func (s *Server) listIssues(c fiber.Ctx) error {
	issues, err := s.q.ListIssues(c.Context(), db.ListIssuesParams{
		Tags:     splitTags(c.Query("tags")),
		MatchAll: c.Query("match") == "all",
		Status:   c.Query("status"),
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
	if tags == nil {
		tags = []string{}
	}
	return c.JSON(tags)
}

type issueBody struct {
	Title    string          `json:"title"`
	Summary  string          `json:"summary"`
	Status   string          `json:"status"`
	Severity string          `json:"severity"`
	Tags     []string        `json:"tags"`
	Source   string          `json:"source"`
	Assignee string          `json:"assignee"`
	Data     json.RawMessage `json:"data"`
}

// POST /api/issues — create an issue directly (FloMorphic, or a manual entry).
func (s *Server) createIssue(c fiber.Ctx) error {
	var body issueBody
	if err := c.Bind().Body(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if body.Title == "" {
		return fiber.NewError(fiber.StatusBadRequest, "title is required")
	}
	issue, err := s.q.CreateIssue(c.Context(), db.CreateIssueParams{
		Title:    body.Title,
		Summary:  body.Summary,
		Status:   orDefault(body.Status, "open"),
		Severity: orDefault(body.Severity, "info"),
		Tags:     orEmpty(body.Tags),
		Source:   body.Source,
		Assignee: body.Assignee,
		Data:     rawOr(body.Data, "{}"),
	})
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(issue)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func orEmpty(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
