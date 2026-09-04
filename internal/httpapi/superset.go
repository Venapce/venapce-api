package httpapi

import (
	"encoding/json"
	"net/url"

	"github.com/gofiber/fiber/v3"

	"github.com/Venapce/venapce-api/internal/superset"
)

// client resolves the live Superset client or fails with a clear 400 so the front
// can steer the operator to the Settings screen.
func (s *Server) client(c fiber.Ctx) (*superset.Client, error) {
	cl := s.sup.Get()
	if cl == nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, "Superset is not configured — set it in Settings")
	}
	return cl, nil
}

// writeRaw sends pre-serialized JSON straight through.
func writeRaw(c fiber.Ctx, raw json.RawMessage) error {
	c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	return c.Send(raw)
}

// GET /api/superset/databases
func (s *Server) supersetDatabases(c fiber.Ctx) error {
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
func (s *Server) supersetDatasets(c fiber.Ctx) error {
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
func (s *Server) supersetDataset(c fiber.Ctx) error {
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

// POST /api/superset/datasets — register a physical dataset (table) on an
// existing Superset database connection. Body: {database, schema?, table_name}.
func (s *Server) supersetCreateDataset(c fiber.Ctx) error {
	cl, err := s.client(c)
	if err != nil {
		return err
	}
	var in struct {
		Database  int    `json:"database"`
		Schema    string `json:"schema"`
		TableName string `json:"table_name"`
	}
	if err := json.Unmarshal(c.Body(), &in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if in.Database == 0 || in.TableName == "" {
		return fiber.NewError(fiber.StatusBadRequest, "database and table_name are required")
	}
	payload := map[string]any{"database": in.Database, "table_name": in.TableName}
	if in.Schema != "" {
		payload["schema"] = in.Schema
	}
	body, _ := json.Marshal(payload)
	raw, err := cl.Post(c.Context(), "/dataset/", body)
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, err.Error())
	}
	c.Status(fiber.StatusCreated)
	return writeRaw(c, raw)
}

// GET /api/superset/databases/:id/schemas — schema names on a connection, for the
// Add Dataset picker.
func (s *Server) supersetDatabaseSchemas(c fiber.Ctx) error {
	cl, err := s.client(c)
	if err != nil {
		return err
	}
	raw, err := cl.Result(c.Context(), "/database/"+c.Params("id")+"/schemas/?q=(force:!f)")
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, err.Error())
	}
	return writeRaw(c, raw)
}

// GET /api/superset/databases/:id/tables?schema= — tables in a schema, for the
// Add Dataset picker.
func (s *Server) supersetDatabaseTables(c fiber.Ctx) error {
	cl, err := s.client(c)
	if err != nil {
		return err
	}
	q := "(force:!f"
	if schema := c.Query("schema"); schema != "" {
		q += ",schema_name:'" + risonEscape(schema) + "'"
	}
	q += ")"
	raw, err := cl.Result(c.Context(), "/database/"+c.Params("id")+"/tables/?q="+url.QueryEscape(q))
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, err.Error())
	}
	return writeRaw(c, raw)
}

// GET /api/superset/dashboards — Superset's own dashboards (metadata list).
func (s *Server) supersetDashboards(c fiber.Ctx) error {
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
func (s *Server) supersetChartData(c fiber.Ctx) error {
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
