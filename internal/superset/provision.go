package superset

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// DBConnection is a Postgres connection described by discrete fields, mirroring
// what Superset's "add database" form collects. Passing the password as its own
// field (rather than embedded in a URI) means characters like `@` or `:` in it
// are never mis-parsed.
type DBConnection struct {
	Name     string
	Host     string
	Port     int
	Database string
	Username string
	Password string
	SSLMode  string // libpq sslmode, e.g. "disable" (optional)
}

// EnsureDatabase makes sure a Superset database connection named conn.Name exists
// for the given Postgres. It returns the connection id and whether it was created
// just now (false = it already existed). Idempotent: safe to call on every boot.
//
// It uses Superset's dynamic_form configuration method — the same path the "add
// database" UI uses — so Superset assembles and escapes the SQLAlchemy URI itself
// from the discrete parameters.
func (c *Client) EnsureDatabase(ctx context.Context, conn DBConnection) (id int, created bool, err error) {
	q := "(filters:!((col:database_name,opr:eq,value:'" + risonEscape(conn.Name) + "')))"
	raw, err := c.Result(ctx, "/database/?q="+url.QueryEscape(q))
	if err != nil {
		return 0, false, err
	}
	var found []struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(raw, &found); err != nil {
		return 0, false, err
	}
	if len(found) > 0 {
		return found[0].ID, false, nil
	}

	params := map[string]any{
		"host":     conn.Host,
		"port":     conn.Port,
		"database": conn.Database,
		"username": conn.Username,
		"password": conn.Password,
	}
	if conn.SSLMode != "" {
		params["query"] = map[string]string{"sslmode": conn.SSLMode}
	}
	payload, _ := json.Marshal(map[string]any{
		"database_name":        conn.Name,
		"engine":               "postgresql",
		"configuration_method": "dynamic_form",
		"parameters":           params,
		"expose_in_sqllab":     true,
	})
	var out struct {
		ID int `json:"id"`
	}
	if err := c.call(ctx, http.MethodPost, "/database/", json.RawMessage(payload), &out); err != nil {
		return 0, false, err
	}
	if out.ID == 0 {
		return 0, false, fmt.Errorf("superset created database %q but returned no id", conn.Name)
	}
	return out.ID, true, nil
}

// EnsureDataset makes sure a physical dataset for `schema.table` on the given
// database connection exists, returning its id and whether it was just created.
// Idempotent.
func (c *Client) EnsureDataset(ctx context.Context, databaseID int, schema, table string) (id int, created bool, err error) {
	q := "(filters:!((col:table_name,opr:eq,value:'" + risonEscape(table) + "')))"
	raw, err := c.Result(ctx, "/dataset/?q="+url.QueryEscape(q))
	if err != nil {
		return 0, false, err
	}
	var found []struct {
		ID       int `json:"id"`
		Database struct {
			ID int `json:"id"`
		} `json:"database"`
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(raw, &found); err != nil {
		return 0, false, err
	}
	for _, d := range found {
		if d.Database.ID == databaseID && (schema == "" || d.Schema == schema) {
			return d.ID, false, nil
		}
	}

	body := map[string]any{"database": databaseID, "table_name": table}
	if schema != "" {
		body["schema"] = schema
	}
	payload, _ := json.Marshal(body)
	var out struct {
		ID int `json:"id"`
	}
	if err := c.call(ctx, http.MethodPost, "/dataset/", json.RawMessage(payload), &out); err != nil {
		return 0, false, err
	}
	if out.ID == 0 {
		return 0, false, fmt.Errorf("superset created dataset %q but returned no id", table)
	}
	return out.ID, true, nil
}

// risonEscape quotes single quotes for Superset's Rison string form so a value
// with an apostrophe can't break the filter query.
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
