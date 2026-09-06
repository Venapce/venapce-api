// Package db is the venapce plugin's database module. It exposes upsert and
// update actions for venapce's two data tables — issues and stage — under the
// `db.issues.*` and `db.stages.*` method prefixes.
//
// Unlike a general Postgres plugin, this one carries no connection settings: the
// venapce pgx pool is injected, because the database venapce owns is the only one
// these actions ever write to.
//
// The writable column set of each table is not hardcoded here — it is derived by
// reflecting over the sqlc-generated model in internal/db (model.Issue,
// model.Stage). So when the issues/stage schema changes and sqlc regenerates
// those structs, this module follows automatically: a new column of a supported
// type becomes a writable field with no edit here, and RETURNING * carries it
// back in the output. Column names still come only from the generated model,
// never from user input, so the generated SQL never interpolates an untrusted
// identifier; every value is a bound parameter.
package db

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Inflowenger/go-plugin-sdk/formkit"
	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
	"github.com/jackc/pgx/v5/pgxpool"

	model "github.com/Venapce/venapce-api/internal/db"
	"github.com/Venapce/venapce-api/internal/plugin/flow"
)

// jobTimeout bounds one action's database traffic, well under any realistic flow
// timeout so a hung server fails the node instead of the flow.
const jobTimeout = 30 * time.Second

// kind is how a column's form value is turned into a bound argument.
type kind int

const (
	kindText  kind = iota // TEXT
	kindArray             // TEXT[]  (from a JSON/string list)
	kindJSON              // JSONB   (from a JSON object/string)
	kindInt               // BIGINT
)

// column is one writable column of a table. The name comes from the generated
// model's json tag, never user input, so it is safe to place in the SQL text.
type column struct {
	name string
	kind kind
}

// table describes one venapce data table the module can write. Its writable
// columns are derived from the sqlc model (see tableFrom), not written by hand.
type table struct {
	method       string // method prefix, e.g. "db.issues"
	title        string // node-drawer title stem, e.g. "issue"
	name         string // SQL table name
	columns      []column
	hasUpdatedAt bool // the model has an updated_at we should stamp with now()
	form         sdkv1.FormBuilder
	updateFm     sdkv1.FormBuilder
}

// The tables the module writes, each bound to its sqlc-generated model so the
// writable columns track the schema. tableFrom reflects the model at init.
var (
	issues = tableFrom("db.issues", "issue", "issues", model.Issue{})
	stages = tableFrom("db.stages", "stage row", "stage", model.Stage{})
)

// tableFrom builds a table by reflecting over a generated model value. Every
// field maps to a database column (via its json tag, camelCase → snake_case);
// the id column and the DB-managed timestamps (any time.Time) are recognised but
// not exposed as writable fields. A field whose type the module cannot bind is
// skipped rather than guessed at.
func tableFrom(method, title, name string, mdl any) table {
	t := table{method: method, title: title, name: name}
	rt := reflect.TypeOf(mdl)
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		col := columnName(f)
		if col == "" {
			continue
		}
		// DB-managed timestamps: not writable, but note updated_at so writes can
		// stamp it. This also future-proofs created_at/received_at additions.
		if f.Type == reflect.TypeOf(time.Time{}) {
			if col == "updated_at" {
				t.hasUpdatedAt = true
			}
			continue
		}
		if col == "id" {
			continue // the primary key is handled by readID, never a plain field
		}
		k, ok := kindOf(f.Type)
		if !ok {
			continue
		}
		t.columns = append(t.columns, column{name: col, kind: k})
	}
	return t
}

// columnName reads a model field's database column name from its json tag,
// converting the sqlc camelCase tag back to the snake_case column (issueId →
// issue_id). Falls back to the Go field name when a tag is absent.
func columnName(f reflect.StructField) string {
	tag := strings.SplitN(f.Tag.Get("json"), ",", 2)[0]
	if tag == "" || tag == "-" {
		tag = f.Name
	}
	return camelToSnake(tag)
}

// kindOf maps a generated model field's Go type to how the module binds it, and
// reports false for a type it does not handle.
func kindOf(t reflect.Type) (kind, bool) {
	if t == reflect.TypeOf(json.RawMessage{}) {
		return kindJSON, true // json.RawMessage is []byte — check before the slice case
	}
	switch t.Kind() {
	case reflect.String:
		return kindText, true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return kindInt, true
	case reflect.Slice:
		if t.Elem().Kind() == reflect.String {
			return kindArray, true
		}
	}
	return 0, false
}

// camelToSnake converts a camelCase json tag to a snake_case column name. The
// sqlc tags are clean camelCase (issueId, createdAt, id), so a leading-uppercase
// rule is enough — no acronym special-casing is needed.
func camelToSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r - 'A' + 'a')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Actions returns every db.* action, bound to the venapce pool. Each table gets
// an upsert (insert, or update on an id clash) and an update (by id).
func Actions(pool *pgxpool.Pool) []sdkv1.Action {
	issues.form, issues.updateFm = buildForms(issues)
	stages.form, stages.updateFm = buildForms(stages)
	return []sdkv1.Action{
		upsertAction(pool, issues),
		updateAction(pool, issues),
		upsertAction(pool, stages),
		updateAction(pool, stages),
	}
}

func upsertAction(pool *pgxpool.Pool, t table) sdkv1.Action {
	return sdkv1.Action{
		Method:      t.method + ".upsert",
		Title:       "Upsert " + t.title,
		Description: fmt.Sprintf("Insert a %s, or update it in place if an id is supplied and already exists. Blank fields are left to their defaults on insert. Values accept {{$.path}} tokens.", t.title),
		Icon:        sdkv1.Icon{Icon: "mdi-table-row-plus-after"},
		Form:        t.form,
		RequestHandler: func(job sdkv1.Job) {
			runWrite(pool, &job, func(in map[string]any) (string, []any, error) {
				return buildUpsert(t, in)
			})
		},
	}
}

func updateAction(pool *pgxpool.Pool, t table) sdkv1.Action {
	return sdkv1.Action{
		Method:      t.method + ".update",
		Title:       "Update " + t.title,
		Description: fmt.Sprintf("Update an existing %s by id. Only the fields you fill are changed; the rest keep their current values. Values accept {{$.path}} tokens.", t.title),
		Icon:        sdkv1.Icon{Icon: "mdi-table-edit"},
		Form:        t.updateFm,
		RequestHandler: func(job sdkv1.Job) {
			runWrite(pool, &job, func(in map[string]any) (string, []any, error) {
				return buildUpdate(t, in)
			})
		},
	}
}

// runWrite is the shared job runner: decode the flat body, resolve {{$.path}}
// tokens in it, build the statement, run it, and finish the job exactly once on
// every path.
func runWrite(pool *pgxpool.Pool, job *sdkv1.Job, build func(map[string]any) (string, []any, error)) {
	req, err := sdkv1.CastRequestTo[map[string]any](job.Req.Data)
	if err != nil {
		job.DoneWithError("invalid request body: " + err.Error())
		return
	}
	in := req.Body
	resolveMap(job, in)

	sql, args, err := build(in)
	if err != nil {
		job.DoneWithError(err.Error())
		return
	}

	job.Progress(50, sdkv1.Frame{Title: "writing", Content: preview(sql)})

	ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
	defer cancel()

	row, err := queryRow(ctx, pool, sql, args)
	if err != nil {
		job.DoneWithError(err.Error())
		return
	}
	out := map[string]any{"row": row}
	if id, ok := row["id"]; ok {
		out["id"] = id
	}
	job.Done(out)
}

// resolveMap rewrites {{$.path}} tokens in the string and []any-of-string values
// of a decoded flat body against the flow scope.
func resolveMap(job *sdkv1.Job, in map[string]any) {
	r := flow.NewResolver(job)
	for k, v := range in {
		switch t := v.(type) {
		case string:
			in[k] = r.Resolve(t)
		case []any:
			for i, e := range t {
				if s, ok := e.(string); ok {
					t[i] = r.Resolve(s)
				}
			}
		}
	}
}

// buildUpsert renders an INSERT that becomes an UPDATE on an id clash. When an id
// is supplied it is written explicitly so the conflict target exists; otherwise
// the table's serial id is generated and it is a plain insert.
func buildUpsert(t table, in map[string]any) (string, []any, error) {
	cols, args, err := collect(t, in)
	if err != nil {
		return "", nil, err
	}
	id, hasID, err := readID(in)
	if err != nil {
		return "", nil, err
	}
	if len(cols) == 0 && !hasID {
		return "", nil, fmt.Errorf("nothing to write: fill at least one field")
	}

	names := make([]string, 0, len(cols)+1)
	placeholders := make([]string, 0, len(cols)+1)
	values := make([]any, 0, len(cols)+1)
	if hasID {
		names = append(names, "id")
		values = append(values, id)
		placeholders = append(placeholders, "$1")
	}
	for _, c := range cols {
		names = append(names, c.name)
		values = append(values, args[c.name])
		placeholders = append(placeholders, "$"+strconv.Itoa(len(values)))
	}

	sql := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", t.name, strings.Join(names, ", "), strings.Join(placeholders, ", "))
	if hasID {
		if len(cols) == 0 {
			sql += " ON CONFLICT (id) DO NOTHING"
		} else {
			sets := make([]string, 0, len(cols)+1)
			for _, c := range cols {
				sets = append(sets, c.name+" = EXCLUDED."+c.name)
			}
			if t.hasUpdatedAt {
				sets = append(sets, "updated_at = now()")
			}
			sql += " ON CONFLICT (id) DO UPDATE SET " + strings.Join(sets, ", ")
		}
	}
	sql += " RETURNING *"
	return sql, values, nil
}

// buildUpdate renders an UPDATE of only the supplied columns, by id.
func buildUpdate(t table, in map[string]any) (string, []any, error) {
	id, hasID, err := readID(in)
	if err != nil {
		return "", nil, err
	}
	if !hasID {
		return "", nil, fmt.Errorf("missing required field: id")
	}
	cols, args, err := collect(t, in)
	if err != nil {
		return "", nil, err
	}
	if len(cols) == 0 {
		return "", nil, fmt.Errorf("nothing to update: fill at least one field besides id")
	}

	values := []any{id}
	sets := make([]string, 0, len(cols)+1)
	for _, c := range cols {
		values = append(values, args[c.name])
		sets = append(sets, c.name+" = $"+strconv.Itoa(len(values)))
	}
	if t.hasUpdatedAt {
		sets = append(sets, "updated_at = now()")
	}
	sql := fmt.Sprintf("UPDATE %s SET %s WHERE id = $1 RETURNING *", t.name, strings.Join(sets, ", "))
	return sql, values, nil
}

// collect turns the supplied form fields into the ordered columns to write and
// their bound values. A blank field is omitted, so it keeps its default (insert)
// or its current value (update).
func collect(t table, in map[string]any) ([]column, map[string]any, error) {
	cols := make([]column, 0, len(t.columns))
	args := make(map[string]any, len(t.columns))
	for _, c := range t.columns {
		raw, ok := in[c.name]
		if !ok {
			continue
		}
		v, present, err := coerce(c, raw)
		if err != nil {
			return nil, nil, fmt.Errorf("field %q: %w", c.name, err)
		}
		if !present {
			continue
		}
		cols = append(cols, c)
		args[c.name] = v
	}
	return cols, args, nil
}

// coerce converts one raw form value to its bound argument, reporting present=false
// for a value that should be omitted (a blank string, an empty list).
func coerce(c column, raw any) (any, bool, error) {
	switch c.kind {
	case kindText:
		s := toString(raw)
		if s == "" {
			return nil, false, nil
		}
		return s, true, nil
	case kindInt:
		s := toString(raw)
		if s == "" {
			return nil, false, nil
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, false, fmt.Errorf("must be a whole number, got %q", s)
		}
		return n, true, nil
	case kindArray:
		list := toStringList(raw)
		if len(list) == 0 {
			return nil, false, nil
		}
		return list, true, nil
	case kindJSON:
		return coerceJSON(raw)
	default:
		return nil, false, fmt.Errorf("unsupported column kind")
	}
}

// coerceJSON accepts either a JSON object already decoded (map) or a JSON string,
// and binds it as jsonb. A blank string is omitted.
func coerceJSON(raw any) (any, bool, error) {
	switch v := raw.(type) {
	case nil:
		return nil, false, nil
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return nil, false, nil
		}
		if !json.Valid([]byte(s)) {
			return nil, false, fmt.Errorf("must be a JSON object, e.g. {\"key\": \"value\"}")
		}
		return []byte(s), true, nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, false, fmt.Errorf("not JSON-encodable: %w", err)
		}
		return b, true, nil
	}
}

// readID reads the optional id field. Absent or blank means "no id".
func readID(in map[string]any) (int64, bool, error) {
	raw, ok := in["id"]
	if !ok {
		return 0, false, nil
	}
	s := toString(raw)
	if s == "" {
		return 0, false, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("id must be a whole number, got %q", s)
	}
	return n, true, nil
}

// queryRow runs a single-row-returning write and returns the RETURNING row as a
// JSON-able map. A statement that returns no row (e.g. DO NOTHING, or an update
// that matched nothing) yields a nil map and no error.
func queryRow(ctx context.Context, pool *pgxpool.Pool, sql string, args []any) (map[string]any, error) {
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	fields := rows.FieldDescriptions()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	values, err := rows.Values()
	if err != nil {
		return nil, err
	}
	row := make(map[string]any, len(fields))
	for i, f := range fields {
		row[f.Name] = normalize(values[i])
	}
	return row, rows.Err()
}

// normalize turns a pgx-decoded value into something that marshals to clean JSON.
func normalize(value any) any {
	switch v := value.(type) {
	case nil:
		return nil
	case time.Time:
		return v.Format(time.RFC3339Nano)
	case [16]byte: // uuid
		return fmt.Sprintf("%x-%x-%x-%x-%x", v[0:4], v[4:6], v[6:8], v[8:10], v[10:16])
	case []byte:
		if utf8.Valid(v) {
			return string(v)
		}
		return base64.StdEncoding.EncodeToString(v)
	default:
		return v
	}
}

// toString renders a scalar form value; a float that is whole prints without a
// fractional part, so an id typed as a number does not arrive as "42.000000".
func toString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(t)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", t))
	}
}

// toStringList reads a list field, tolerating both a decoded array and a single
// scalar. Blank entries are dropped.
func toStringList(raw any) []string {
	var out []string
	switch v := raw.(type) {
	case []any:
		for _, e := range v {
			if s := toString(e); s != "" {
				out = append(out, s)
			}
		}
	case []string:
		for _, s := range v {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	case string:
		if s := strings.TrimSpace(v); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// preview trims SQL to one short line for a progress frame.
func preview(sql string) string {
	sql = strings.Join(strings.Fields(sql), " ")
	if len(sql) > 120 {
		return sql[:117] + "…"
	}
	return sql
}

// buildForms produces the upsert and update forms for a table. The update form is
// the same fields with id required; the upsert form has id optional.
func buildForms(t table) (upsert, update sdkv1.FormBuilder) {
	return formFor(t, false), formFor(t, true)
}

func formFor(t table, idRequired bool) sdkv1.FormBuilder {
	// Id is a Text field, not Integer, so the value can be a {{$.path}} token as
	// well as a literal whole number — an integer control would refuse the braces.
	// readID parses the resolved value back to an int64 at write time.
	idField := formkit.Text("id", "Id")
	if idRequired {
		idField = idField.Required().Describe("The " + t.title + " to update. A whole number or a {{$.path}} token.")
	} else {
		idField = idField.Describe("Leave blank to insert a new " + t.title + "; set it to update an existing one in place. A whole number or a {{$.path}} token.")
	}

	fields := []*formkit.Field{idField}
	for _, c := range t.columns {
		fields = append(fields, fieldFor(c))
	}
	title := "Upsert " + t.title
	if idRequired {
		title = "Update " + t.title
	}
	return formkit.New(title).Add(fields...).Build()
}

// fieldFor renders one column as a form field matching its kind.
func fieldFor(c column) *formkit.Field {
	label := titleize(c.name)
	switch c.kind {
	case kindArray:
		return formkit.List(c.name, label).Describe("One tag per row. Blank leaves it unchanged.")
	case kindJSON:
		return formkit.TextArea(c.name, label).Describe("A JSON object, e.g. {\"key\": \"value\"}. Accepts {{$.path}} tokens.")
	case kindInt:
		// Text, not Integer, so a {{$.path}} token is accepted as well as a
		// literal number; coerce parses the resolved value back to an int.
		return formkit.Text(c.name, label).Describe("A whole number or a {{$.path}} token.")
	default:
		return formkit.Text(c.name, label).Describe("Accepts {{$.path}} tokens.")
	}
}

// titleize turns a snake_case column name into a form label ("issue_id" → "Issue id").
func titleize(name string) string {
	s := strings.ReplaceAll(name, "_", " ")
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
