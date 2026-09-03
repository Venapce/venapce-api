package httpapi

import (
	"encoding/json"
	"net/url"

	"github.com/gofiber/fiber/v2"

	"github.com/inflowenger/venapce-api/internal/superset"
)

// client resolves the live Superset client or fails with a clear 400 so the front
// can steer the operator to the Settings screen.
func (s *Server) client(c *fiber.Ctx) (*superset.Client, error) {
	cl := s.sup.Get()
	if cl == nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, "Superset is not configured — set it in Settings")
	}
	return cl, nil
}

// writeRaw sends pre-serialized JSON straight through.
func writeRaw(c *fiber.Ctx, raw json.RawMessage) error {
	c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	return c.Send(raw)
}

// GET /api/superset/databases
func (s *Server) supersetDatabases(c *fiber.Ctx) error {
	cl, err := s.client(c)
	if err != nil {
		return err
	}
	raw, err := cl.Result(c.Context(), "/database/?q=(page_size:200)")
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, err.Error())
	}
	return writeRaw(c, raw)
}

// GET /api/superset/datasets?search=
func (s *Server) supersetDatasets(c *fiber.Ctx) error {
	cl, err := s.client(c)
	if err != nil {
		return err
	}
	q := "(page_size:200,order_column:changed_on_delta_humanized,order_direction:desc"
	if search := c.Query("search"); search != "" {
		q += ",filters:!((col:table_name,opr:ct,value:'" + risonEscape(search) + "'))"
	}
	q += ")"
	raw, err := cl.Result(c.Context(), "/dataset/?q="+url.QueryEscape(q))
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, err.Error())
	}
	return writeRaw(c, raw)
}

// GET /api/superset/datasets/:id
func (s *Server) supersetDataset(c *fiber.Ctx) error {
	cl, err := s.client(c)
	if err != nil {
		return err
	}
	raw, err := cl.Result(c.Context(), "/dataset/"+c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, err.Error())
	}
	return writeRaw(c, raw)
}

// GET /api/superset/dashboards — Superset's own dashboards (metadata list).
func (s *Server) supersetDashboards(c *fiber.Ctx) error {
	cl, err := s.client(c)
	if err != nil {
		return err
	}
	raw, err := cl.Result(c.Context(), "/dashboard/?q=(page_size:100,order_column:changed_on,order_direction:desc)")
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, err.Error())
	}
	return writeRaw(c, raw)
}

// POST /api/superset/chart/data — proxy a query_context and return computed rows.
func (s *Server) supersetChartData(c *fiber.Ctx) error {
	cl, err := s.client(c)
	if err != nil {
		return err
	}
	body := c.Body()
	if len(body) == 0 {
		return fiber.NewError(fiber.StatusBadRequest, "empty query_context")
	}
	raw, err := cl.ChartData(c.Context(), json.RawMessage(body))
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, err.Error())
	}
	return writeRaw(c, raw)
}

// risonEscape quotes single quotes for Superset's Rison string form.
func risonEscape(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\'' {
			out = append(out, '!', '\'')
			continue
		}
		out = append(out, r)
	}
	return string(out)
}
