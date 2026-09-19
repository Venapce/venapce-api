package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"

	"github.com/Venapce/venapce-api/internal/db"
	"github.com/Venapce/venapce-api/internal/flomorphic"
	"github.com/Venapce/venapce-api/internal/operations"
)

// Operations — the package manager. An operation is a folder of FloMorphic
// workflow exports plus an operation.json manifest; installing one stores the
// whole package here, and from here each manifest flow is pushed into
// FloMorphic (POST /flow/import, with the operator's params substituted) and
// bound to the flow it became. Entry flows are then run from the operation
// page as activities on the operation (subject_kind "operation"), so an
// operation's own history sits next to the rows its runs produced.
//
// Readiness is recomputed on every read against the live estate: the osctrl
// connection, the plugin actions FloMorphic's extension table exposes (the
// portable key — a plugin id is per-install), the other operations installed,
// the flows bound, and the required params set.

const subjectOperation = "operation"

// binding is what a manifest flow key became in FloMorphic.
type binding struct {
	FlowID      string    `json:"flowId"`
	FlowTitle   string    `json:"flowTitle,omitempty"`
	InstalledAt time.Time `json:"installedAt"`
	// Problems / missing actions from the last install, for the flow row.
	MissingActions []flomorphic.MissingAction `json:"missingActions,omitempty"`
	CompileError   string                     `json:"compileError,omitempty"`
}

type readinessCheck struct {
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	Status string `json:"status"` // ok | missing | unknown
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
	Href   string `json:"href,omitempty"`
}

type readiness struct {
	Ready  bool             `json:"ready"`
	Checks []readinessCheck `json:"checks"`
}

// operationView is the list item.
type operationView struct {
	ID            int64                      `json:"id"`
	Key           string                     `json:"key"`
	Name          string                     `json:"name"`
	Version       string                     `json:"version"`
	Description   string                     `json:"description,omitempty"`
	Tags          []string                   `json:"tags"`
	Scale         []string                   `json:"scale"`
	Source        json.RawMessage            `json:"source"`
	Params        map[string]json.RawMessage `json:"params"`
	Ready         bool                       `json:"ready"`
	FlowsBound    int                        `json:"flowsBound"`
	FlowsTotal    int                        `json:"flowsTotal"`
	LastRunAt     *time.Time                 `json:"lastRunAt,omitempty"`
	LastRunStatus string                     `json:"lastRunStatus,omitempty"`
	InstalledAt   time.Time                  `json:"installedAt"`
	UpdatedAt     time.Time                  `json:"updatedAt"`
}

// flowState is one manifest flow with its binding and last run.
type flowState struct {
	operations.FlowSpec
	FlowID         string                     `json:"flowId,omitempty"`
	FlowTitle      string                     `json:"flowTitle,omitempty"`
	InstalledAt    *time.Time                 `json:"installedAt,omitempty"`
	MissingActions []flomorphic.MissingAction `json:"missingActions,omitempty"`
	CompileError   string                     `json:"compileError,omitempty"`
	LastRun        *lastRun                   `json:"lastRun,omitempty"`
}

type lastRun struct {
	ID         int64      `json:"id"`
	Status     string     `json:"status"`
	Title      string     `json:"title"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Error      string     `json:"error,omitempty"`
}

type operationDetail struct {
	operationView
	Manifest  json.RawMessage   `json:"manifest"`
	Flows     []flowState       `json:"flows"`
	Readiness readiness         `json:"readiness"`
	Readme    string            `json:"readme,omitempty"`
	FlowDocs  map[string]string `json:"flowDocs,omitempty"`
}

// ---- storage helpers ----

func (s *Server) loadBundle(op db.Operation) (*operations.Bundle, error) {
	m, errs := operations.ParseManifest(op.Manifest)
	if m == nil {
		return nil, errors.New(strings.Join(errs, "; "))
	}
	files := map[string]string{}
	_ = json.Unmarshal(op.Files, &files)
	return &operations.Bundle{Manifest: m, Files: files}, nil
}

func bindingsOf(op db.Operation) map[string]binding {
	out := map[string]binding{}
	_ = json.Unmarshal(op.Bindings, &out)
	if out == nil {
		out = map[string]binding{}
	}
	return out
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}

// allParams merges the plain params with the decrypted secrets — what the
// substitution and the run input use. Never serve this.
func (s *Server) allParams(op db.Operation) map[string]json.RawMessage {
	out := operations.ParamsDoc(op.Params)
	secrets := map[string]string{}
	_ = json.Unmarshal(op.Secrets, &secrets)
	for k, enc := range secrets {
		if plain, err := s.box.Decrypt(enc); err == nil {
			out[k] = json.RawMessage(plain)
		}
	}
	return out
}

// storeParams splits the operator's values into plain params and encrypted
// secrets per the manifest.
func (s *Server) storeParams(m *operations.Manifest, values map[string]json.RawMessage) (plain, secrets json.RawMessage, err error) {
	p, sec := operations.SplitSecrets(m, operations.TrimEmpty(values))
	encs := map[string]string{}
	for k, v := range sec {
		enc, err := s.box.Encrypt(string(v))
		if err != nil {
			return nil, nil, err
		}
		encs[k] = enc
	}
	return mustJSON(p), mustJSON(encs), nil
}

// ---- readiness ----

// estate is what the checks compare the manifest against, gathered once per
// request (the plugin action list is a FloMorphic round trip).
type estate struct {
	osctrl        bool
	floConfigured bool
	actions       map[string]flomorphic.PluginAction
	actionsKnown  bool
	pluginRunning bool
	installed     map[string]bool
}

func (s *Server) gatherEstate(ctx context.Context) estate {
	e := estate{osctrl: s.osc.Configured(), installed: map[string]bool{}, actions: map[string]flomorphic.PluginAction{}}
	e.pluginRunning = s.plg.Status().Running
	if client := s.flo.Get(); client != nil {
		e.floConfigured = true
		if list, err := client.ListPluginActions(ctx); err == nil {
			e.actionsKnown = true
			for _, a := range list {
				e.actions[a.Action] = a
			}
		}
	}
	if ops, err := s.q.ListOperations(ctx); err == nil {
		for _, o := range ops {
			e.installed[o.Key] = true
		}
	}
	return e
}

func (s *Server) computeReadiness(b *operations.Bundle, values map[string]json.RawMessage, binds map[string]binding, e estate) readiness {
	m := b.Manifest
	var checks []readinessCheck
	req := m.Requires
	if req == nil {
		req = &operations.Requires{}
	}
	referenced := b.ReferencedActions()
	allActions := map[string]bool{}
	for _, list := range referenced {
		for _, a := range list {
			allActions[a] = true
		}
	}
	usesOsquery := false
	for a := range allActions {
		if strings.HasPrefix(a, "osquery.") || strings.HasPrefix(a, "osctrl.") {
			usesOsquery = true
		}
	}

	// osctrl
	needOsctrl := req.Osctrl != nil && (req.Osctrl.Required == nil || *req.Osctrl.Required)
	if req.Osctrl == nil && usesOsquery {
		needOsctrl = true
	}
	if needOsctrl {
		c := readinessCheck{Kind: "osctrl", Label: "osctrl fleet manager", Fix: "settings"}
		if e.osctrl {
			c.Status = "ok"
			c.Detail = "Connected"
			if req.Osctrl != nil && req.Osctrl.Note != "" {
				c.Detail = req.Osctrl.Note
			}
		} else {
			c.Status = "missing"
			c.Detail = "Not configured — run your own osctrl or create a managed space (Settings → FloMorphic)"
		}
		checks = append(checks, c)
		if e.osctrl && req.Osctrl != nil && req.Osctrl.MinNodes > 0 {
			d := ""
			if len(req.Osctrl.Targets) > 0 {
				d = "Targets: " + strings.Join(req.Osctrl.Targets, ", ")
			}
			checks = append(checks, readinessCheck{Kind: "osctrl", Label: fmt.Sprintf("At least %d enrolled node(s)", req.Osctrl.MinNodes), Status: "unknown", Detail: d, Fix: "nodes"})
		}
	}

	// plugins: declared ∪ referenced, grouped by plugin name (venapce's own
	// actions collapse into one check).
	type group struct {
		name, repo, href string
		actions          []string
	}
	groups := map[string]*group{}
	var order []string
	add := func(name, repo, action string) {
		g := groups[name]
		if g == nil {
			g = &group{name: name, repo: repo}
			groups[name] = g
			order = append(order, name)
		}
		if repo != "" && g.repo == "" {
			g.repo = repo
		}
		for _, a := range g.actions {
			if a == action {
				return
			}
		}
		g.actions = append(g.actions, action)
	}
	declared := map[string]bool{}
	for _, p := range req.Plugins {
		for _, a := range p.Actions {
			declared[a] = true
			name := p.Name
			if operations.IsVenapceAction(a) {
				name = "venapce"
			}
			add(name, p.Repo, a)
		}
	}
	for a := range allActions {
		if declared[a] {
			continue
		}
		name := "venapce"
		if !operations.IsVenapceAction(a) {
			name = strings.SplitN(a, ".", 2)[0]
		}
		add(name, "", a)
	}
	for _, name := range order {
		g := groups[name]
		sort.Strings(g.actions)
		c := readinessCheck{Kind: "plugin", Label: name + " plugin", Href: g.repo}
		if name == "venapce" {
			c.Label = "venapce plugin in FloMorphic"
			c.Fix = "settings"
		}
		switch {
		case !e.floConfigured:
			c.Status = "missing"
			c.Detail = "FloMorphic API access is not configured (Settings → FloMorphic)"
			c.Fix = "settings"
		case !e.actionsKnown:
			c.Status = "unknown"
			c.Detail = "FloMorphic did not answer — could not list its plugins"
		default:
			var missing []string
			for _, a := range g.actions {
				if _, ok := e.actions[a]; !ok {
					missing = append(missing, a)
				}
			}
			if len(missing) == 0 {
				c.Status = "ok"
				c.Detail = "Provides " + strings.Join(g.actions, ", ")
				if name == "venapce" && !e.pluginRunning {
					c.Status = "missing"
					c.Detail = "Registered in FloMorphic but the in-process plugin is not running — restart it from Settings"
				}
			} else {
				c.Status = "missing"
				c.Detail = "Missing " + strings.Join(missing, ", ")
				if name == "venapce" {
					c.Detail += " — register venapce as a FloMorphic plugin and refresh it (Settings → FloMorphic)"
				} else if g.repo != "" {
					c.Detail += " — install the plugin from " + g.repo + " and sync it in FloMorphic"
				} else {
					c.Detail += " — install the plugin in FloMorphic → Extensions and sync its actions"
				}
			}
		}
		checks = append(checks, c)
	}

	for _, st := range req.Settings {
		label := st.Kind + " settings profile"
		if st.For != "" {
			label += " for " + st.For
		}
		d := st.Note
		if d == "" {
			d = "Pick a Node Settings profile on the node after the flow is installed in FloMorphic"
		}
		checks = append(checks, readinessCheck{Kind: "settings", Label: label, Status: "unknown", Detail: d})
	}
	for _, dep := range req.Operations {
		key := strings.SplitN(dep, "@", 2)[0]
		c := readinessCheck{Kind: "operation", Label: fmt.Sprintf("Operation %q", dep), Fix: "operations"}
		if e.installed[key] {
			c.Status, c.Detail = "ok", "Installed"
		} else {
			c.Status, c.Detail = "missing", "Install it first"
		}
		checks = append(checks, c)
	}
	for _, ds := range req.Datasets {
		checks = append(checks, readinessCheck{Kind: "dataset", Label: fmt.Sprintf("Dataset %q", ds), Status: "unknown", Detail: "Registered under Visualizations → Datasets", Fix: "datasets"})
	}
	missingParams := operations.Missing(m, values)
	sort.Strings(missingParams)
	for _, name := range missingParams {
		label := name
		if spec, ok := m.Params[name]; ok && spec.Label != "" {
			label = spec.Label
		}
		checks = append(checks, readinessCheck{Kind: "param", Label: fmt.Sprintf("Parameter %q", label), Status: "missing", Detail: "Required, and it has no default — set it in Parameters"})
	}
	for _, f := range m.Flows {
		c := readinessCheck{Kind: "flow", Label: fmt.Sprintf("Flow %q", f.Key)}
		if bnd, ok := binds[f.Key]; ok && bnd.FlowID != "" {
			c.Status = "ok"
			c.Label += " bound"
			c.Detail = bnd.FlowID
			if len(bnd.MissingActions) > 0 {
				c.Status = "missing"
				var names []string
				for _, ma := range bnd.MissingActions {
					names = append(names, ma.Action)
				}
				c.Detail = "Installed, but its nodes call actions no plugin provides: " + strings.Join(names, ", ") + " — re-install after installing the plugin"
			}
		} else {
			c.Status = "missing"
			c.Label += " not installed"
			c.Detail = "Install it into FloMorphic or link an existing flow"
		}
		checks = append(checks, c)
	}
	r := readiness{Ready: true, Checks: checks}
	if r.Checks == nil {
		r.Checks = []readinessCheck{}
	}
	for _, c := range checks {
		if c.Status == "missing" {
			r.Ready = false
		}
	}
	return r
}

// ---- views ----

func (s *Server) view(ctx context.Context, op db.Operation, e *estate) (operationView, *operations.Bundle, readiness, error) {
	b, err := s.loadBundle(op)
	if err != nil {
		return operationView{}, nil, readiness{}, err
	}
	binds := bindingsOf(op)
	values := s.allParams(op)
	var rd readiness
	if e != nil {
		rd = s.computeReadiness(b, values, binds, *e)
	}
	bound := 0
	for _, f := range b.Manifest.Flows {
		if bnd, ok := binds[f.Key]; ok && bnd.FlowID != "" {
			bound++
		}
	}
	v := operationView{
		ID: op.ID, Key: op.Key, Name: op.Name, Version: op.Version, Description: op.Description,
		Tags: orEmpty(op.Tags), Scale: orEmpty(op.Scale), Source: rawOr(op.Source, "{}"),
		Params: operations.ParamsDoc(op.Params), Ready: rd.Ready, FlowsBound: bound, FlowsTotal: len(b.Manifest.Flows),
		InstalledAt: op.InstalledAt, UpdatedAt: op.UpdatedAt,
	}
	// Last run across the operation's entry flows.
	if acts, err := s.q.ListActivities(ctx, db.ListActivitiesParams{SubjectKind: subjectOperation, SubjectID: op.ID, Kind: actRun, Tags: []string{}, Lim: 1}); err == nil && len(acts) > 0 {
		a := acts[0]
		t := a.CreatedAt
		if a.StartedAt != nil {
			t = *a.StartedAt
		}
		v.LastRunAt, v.LastRunStatus = &t, a.Status
	}
	return v, b, rd, nil
}

func (s *Server) detail(ctx context.Context, op db.Operation) (*operationDetail, error) {
	e := s.gatherEstate(ctx)
	v, b, rd, err := s.view(ctx, op, &e)
	if err != nil {
		return nil, err
	}
	binds := bindingsOf(op)
	// Latest run per flow, from the operation's activity history.
	acts, _ := s.q.ListActivities(ctx, db.ListActivitiesParams{SubjectKind: subjectOperation, SubjectID: op.ID, Kind: actRun, Tags: []string{}, Lim: 200})
	lastByFlow := map[string]*lastRun{}
	for _, a := range acts {
		if _, seen := lastByFlow[a.FlowID]; seen || a.FlowID == "" {
			continue
		}
		if synced, err := s.syncActivity(ctx, a); err == nil {
			a = synced
		}
		lastByFlow[a.FlowID] = &lastRun{ID: a.ID, Status: a.Status, Title: a.Title, StartedAt: a.StartedAt, FinishedAt: a.FinishedAt, Error: a.Error}
	}
	flows := make([]flowState, 0, len(b.Manifest.Flows))
	docs := map[string]string{}
	for _, f := range b.Manifest.Flows {
		st := flowState{FlowSpec: f}
		if bnd, ok := binds[f.Key]; ok && bnd.FlowID != "" {
			t := bnd.InstalledAt
			st.FlowID, st.FlowTitle, st.InstalledAt = bnd.FlowID, bnd.FlowTitle, &t
			st.MissingActions, st.CompileError = bnd.MissingActions, bnd.CompileError
			st.LastRun = lastByFlow[bnd.FlowID]
		}
		if f.Doc != "" {
			if text, ok := b.Files[f.Doc]; ok {
				docs[f.Key] = text
			}
		}
		flows = append(flows, st)
	}
	d := &operationDetail{operationView: v, Manifest: rawOr(op.Manifest, "{}"), Flows: flows, Readiness: rd, Readme: b.Files[b.Manifest.ReadmePath()]}
	if len(docs) > 0 {
		d.FlowDocs = docs
	}
	return d, nil
}

func (s *Server) getOp(c fiber.Ctx) (db.Operation, error) {
	id, err := idParam(c)
	if err != nil {
		return db.Operation{}, err
	}
	op, err := s.q.GetOperation(c.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Operation{}, fiber.NewError(fiber.StatusNotFound, "operation not found")
	}
	return op, err
}

func (s *Server) respondDetail(c fiber.Ctx, op db.Operation) error {
	d, err := s.detail(c.Context(), op)
	if err != nil {
		return err
	}
	return c.JSON(d)
}

// ---- HTTP ----

// GET /api/operations
func (s *Server) listOperations(c fiber.Ctx) error {
	ops, err := s.q.ListOperations(c.Context())
	if err != nil {
		return err
	}
	e := s.gatherEstate(c.Context())
	out := make([]operationView, 0, len(ops))
	for _, op := range ops {
		v, _, _, err := s.view(c.Context(), op, &e)
		if err != nil {
			log.Printf("operations: #%d unreadable: %v", op.ID, err)
			continue
		}
		out = append(out, v)
	}
	return c.JSON(out)
}

// GET /api/operations/:id
func (s *Server) getOperation(c fiber.Ctx) error {
	op, err := s.getOp(c)
	if err != nil {
		return err
	}
	return s.respondDetail(c, op)
}

type importBody struct {
	URL    string `json:"url"`
	Path   string `json:"path"`
	Bundle *struct {
		Manifest json.RawMessage   `json:"manifest"`
		Files    map[string]string `json:"files"`
	} `json:"bundle"`
	Source *operations.Source         `json:"source"`
	Params map[string]json.RawMessage `json:"params"`
	DryRun bool                       `json:"dryRun"`
}

// readImport turns the body into a bundle: either the one the browser read,
// or the package fetched from the URL.
func (s *Server) readImport(ctx context.Context, body importBody) (*operations.Bundle, operations.Source, []string, error) {
	if body.Bundle != nil {
		m, errs := operations.ParseManifest(body.Bundle.Manifest)
		if m == nil {
			return nil, operations.Source{}, nil, errors.New(strings.Join(errs, "; "))
		}
		files := body.Bundle.Files
		if files == nil {
			files = map[string]string{}
		}
		src := operations.Source{Kind: "paste"}
		if body.Source != nil {
			src = *body.Source
		}
		return &operations.Bundle{Manifest: m, Files: files}, src, errs, nil
	}
	if strings.TrimSpace(body.URL) == "" {
		return nil, operations.Source{}, nil, errors.New("url or bundle is required")
	}
	return s.fetchFromURL(ctx, body.URL, body.Path)
}

func (s *Server) fetchFromURL(ctx context.Context, rawURL, path string) (*operations.Bundle, operations.Source, []string, error) {
	r, err := operations.ResolveURL(rawURL, "main")
	if err != nil {
		return nil, operations.Source{}, nil, err
	}
	f := operations.NewFetcher()
	base := r.RawBase
	src := r.Source
	if p := strings.Trim(path, "/"); p != "" {
		base += p + "/"
		src.Path = p
	} else if r.CatalogAt != "" {
		// A repository root with a catalog: the caller must pick an entry.
		if name, entries, err := f.Catalog(ctx, r); err == nil && len(entries) > 0 {
			var ids []string
			for _, e := range entries {
				ids = append(ids, e.ID+" ("+e.Path+")")
			}
			return nil, src, nil, fmt.Errorf("%s is a catalog of %d operations — pass `path` to pick one: %s", orDefault(name, "the repository"), len(entries), strings.Join(ids, ", "))
		}
	}
	b, errs, err := f.Bundle(ctx, base)
	if err != nil {
		return nil, src, nil, err
	}
	return b, src, errs, nil
}

// POST /api/operations/import
func (s *Server) importOperation(c fiber.Ctx) error {
	var body importBody
	if err := c.Bind().Body(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	b, src, errs, err := s.readImport(c.Context(), body)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	errs = append(errs, b.Check()...)
	m := b.Manifest
	values := operations.TrimEmpty(body.Params)
	existing, err := s.q.GetOperationByKey(c.Context(), m.ID)
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}

	if body.DryRun {
		e := s.gatherEstate(c.Context())
		binds := map[string]binding{}
		if exists {
			binds = bindingsOf(existing)
		}
		res := fiber.Map{
			"manifest":  m.Raw,
			"readiness": s.computeReadiness(b, values, binds, e),
			"readme":    b.Files[m.ReadmePath()],
			"source":    src,
			"problems":  orEmpty(errs),
		}
		if exists {
			res["existing"] = fiber.Map{"id": existing.ID, "version": existing.Version}
		}
		return c.JSON(res)
	}
	if len(errs) > 0 {
		return fiber.NewError(fiber.StatusUnprocessableEntity, strings.Join(errs, "; "))
	}

	var op db.Operation
	if exists {
		// Upgrade in place: new manifest + files, keep the bindings; params are
		// the stored ones overlaid with anything sent.
		merged := s.allParams(existing)
		for k, v := range values {
			merged[k] = v
		}
		plain, secrets, err := s.storeParams(m, merged)
		if err != nil {
			return err
		}
		op, err = s.q.UpdateOperation(c.Context(), db.UpdateOperationParams{
			ID: existing.ID, Name: m.Name, Version: m.Version, Description: m.Description,
			Tags: orEmpty(m.Tags), Scale: orEmpty(m.Scale), Manifest: m.Raw, Files: mustJSON(b.Files),
			Source: mustJSON(src), Params: plain, Secrets: secrets, Bindings: rawOr(existing.Bindings, "{}"),
		})
		if err != nil {
			return err
		}
		s.recordChange(c.Context(), subjectOperation, op.ID, actEdit, fmt.Sprintf("Upgraded to v%s", m.Version), "manual",
			map[string]any{"changed": map[string]any{"version": map[string]any{"from": existing.Version, "to": m.Version}}})
	} else {
		plain, secrets, err := s.storeParams(m, values)
		if err != nil {
			return err
		}
		op, err = s.q.CreateOperation(c.Context(), db.CreateOperationParams{
			Key: m.ID, Name: m.Name, Version: m.Version, Description: m.Description,
			Tags: orEmpty(m.Tags), Scale: orEmpty(m.Scale), Manifest: m.Raw, Files: mustJSON(b.Files),
			Source: mustJSON(src), Params: plain, Secrets: secrets, Bindings: json.RawMessage("{}"),
		})
		if err != nil {
			return err
		}
		s.recordChange(c.Context(), subjectOperation, op.ID, actCreate, fmt.Sprintf("Installed v%s", m.Version), "manual", nil)
	}
	return s.respondDetail(c, op)
}

// POST /api/operations/:id/update — re-read the package from its source.
func (s *Server) updateOperation(c fiber.Ctx) error {
	op, err := s.getOp(c)
	if err != nil {
		return err
	}
	var src operations.Source
	_ = json.Unmarshal(op.Source, &src)
	if src.Kind != "url" || src.URL == "" {
		return fiber.NewError(fiber.StatusBadRequest, "this operation was not installed from a URL — import the folder again to update it")
	}
	b, newSrc, errs, err := s.fetchFromURL(c.Context(), src.URL, src.Path)
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, err.Error())
	}
	if src.Ref != "" && src.Ref != newSrc.Ref {
		// Keep the ref the operator installed from.
		b, newSrc, errs, err = s.fetchFromURL(c.Context(), src.URL+"/tree/"+src.Ref+"/"+src.Path, "")
		if err != nil {
			return fiber.NewError(fiber.StatusBadGateway, err.Error())
		}
	}
	errs = append(errs, b.Check()...)
	if len(errs) > 0 {
		return fiber.NewError(fiber.StatusUnprocessableEntity, strings.Join(errs, "; "))
	}
	m := b.Manifest
	if m.ID != op.Key {
		return fiber.NewError(fiber.StatusConflict, fmt.Sprintf("the source now holds operation %q, not %q", m.ID, op.Key))
	}
	plain, secrets, err := s.storeParams(m, s.allParams(op))
	if err != nil {
		return err
	}
	updated, err := s.q.UpdateOperation(c.Context(), db.UpdateOperationParams{
		ID: op.ID, Name: m.Name, Version: m.Version, Description: m.Description,
		Tags: orEmpty(m.Tags), Scale: orEmpty(m.Scale), Manifest: m.Raw, Files: mustJSON(b.Files),
		Source: mustJSON(newSrc), Params: plain, Secrets: secrets, Bindings: rawOr(op.Bindings, "{}"),
	})
	if err != nil {
		return err
	}
	if op.Version != m.Version {
		s.recordChange(c.Context(), subjectOperation, op.ID, actEdit, fmt.Sprintf("Updated to v%s", m.Version), "manual",
			map[string]any{"changed": map[string]any{"version": map[string]any{"from": op.Version, "to": m.Version}}})
	}
	return s.respondDetail(c, updated)
}

// PUT /api/operations/:id/params
func (s *Server) putOperationParams(c fiber.Ctx) error {
	op, err := s.getOp(c)
	if err != nil {
		return err
	}
	var body struct {
		Params map[string]json.RawMessage `json:"params"`
	}
	if err := c.Bind().Body(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	b, err := s.loadBundle(op)
	if err != nil {
		return err
	}
	// Secrets the form did not resend stay as they are.
	merged := s.allParams(op)
	for k := range merged {
		if spec, ok := b.Manifest.Params[k]; ok && spec.Secret {
			continue
		}
		delete(merged, k)
	}
	for k, v := range operations.TrimEmpty(body.Params) {
		merged[k] = v
	}
	plain, secrets, err := s.storeParams(b.Manifest, merged)
	if err != nil {
		return err
	}
	updated, err := s.q.UpdateOperation(c.Context(), db.UpdateOperationParams{
		ID: op.ID, Name: op.Name, Version: op.Version, Description: op.Description, Tags: op.Tags, Scale: op.Scale,
		Manifest: op.Manifest, Files: op.Files, Source: op.Source, Params: plain, Secrets: secrets, Bindings: op.Bindings,
	})
	if err != nil {
		return err
	}
	return s.respondDetail(c, updated)
}

// DELETE /api/operations/:id — the package and its history; the flows stay
// in FloMorphic.
func (s *Server) deleteOperation(c fiber.Ctx) error {
	op, err := s.getOp(c)
	if err != nil {
		return err
	}
	if err := s.q.DeleteOperation(c.Context(), op.ID); err != nil {
		return err
	}
	s.forgetSubject(c.Context(), subjectOperation, op.ID)
	return c.SendStatus(fiber.StatusNoContent)
}

// flowFile returns a manifest flow's file with the params substituted.
func (s *Server) flowFile(op db.Operation, key string) (*operations.Bundle, *operations.FlowSpec, []byte, error) {
	b, err := s.loadBundle(op)
	if err != nil {
		return nil, nil, nil, err
	}
	f := b.Manifest.Flow(key)
	if f == nil {
		return nil, nil, nil, fiber.NewError(fiber.StatusNotFound, "no such flow in the manifest")
	}
	text, ok := b.Files[f.File]
	if !ok {
		return b, f, nil, fiber.NewError(fiber.StatusUnprocessableEntity, fmt.Sprintf("the package holds no %q — link an existing FloMorphic flow instead", f.File))
	}
	out, err := operations.Substitute([]byte(text), operations.Effective(b.Manifest, s.allParams(op)))
	if err != nil {
		return b, f, nil, fiber.NewError(fiber.StatusUnprocessableEntity, f.File+": "+err.Error())
	}
	return b, f, out, nil
}

// GET /api/operations/:id/flows/:key/file
func (s *Server) getOperationFlowFile(c fiber.Ctx) error {
	op, err := s.getOp(c)
	if err != nil {
		return err
	}
	_, f, out, err := s.flowFile(op, c.Params("key"))
	if err != nil {
		return err
	}
	c.Set("Content-Type", "application/json; charset=utf-8")
	c.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, strings.ReplaceAll(f.File, "/", "_")))
	return c.Send(out)
}

func (s *Server) saveBindings(ctx context.Context, op db.Operation, binds map[string]binding) (db.Operation, error) {
	return s.q.UpdateOperation(ctx, db.UpdateOperationParams{
		ID: op.ID, Name: op.Name, Version: op.Version, Description: op.Description, Tags: op.Tags, Scale: op.Scale,
		Manifest: op.Manifest, Files: op.Files, Source: op.Source, Params: op.Params, Secrets: op.Secrets, Bindings: mustJSON(binds),
	})
}

// POST /api/operations/:id/flows/:key/install — push the flow into FloMorphic
// (re-using the bound flow id on a re-install) and bind it.
func (s *Server) installOperationFlow(c fiber.Ctx) error {
	client := s.flo.Get()
	if client == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "FloMorphic API access is not configured (Settings → FloMorphic)")
	}
	op, err := s.getOp(c)
	if err != nil {
		return err
	}
	key := c.Params("key")
	_, f, file, err := s.flowFile(op, key)
	if err != nil {
		return err
	}
	binds := bindingsOf(op)
	prev := binds[key]
	res, err := client.ImportFlow(c.Context(), prev.FlowID, f.Title, file, false)
	if err != nil {
		var he *flomorphic.HTTPError
		if errors.As(err, &he) && he.NotFound() && prev.FlowID != "" {
			// The bound flow was deleted on FloMorphic — create a fresh one.
			res, err = client.ImportFlow(c.Context(), "", f.Title, file, false)
		}
		if err != nil {
			return fiber.NewError(fiber.StatusBadGateway, "FloMorphic import: "+err.Error())
		}
	}
	binds[key] = binding{FlowID: res.Flow.ID, FlowTitle: res.Flow.Title, InstalledAt: time.Now(), MissingActions: res.MissingActions, CompileError: res.CompileError}
	updated, err := s.saveBindings(c.Context(), op, binds)
	if err != nil {
		return err
	}
	verb := "Installed"
	if prev.FlowID != "" {
		verb = "Re-installed"
	}
	ref := map[string]any{"flowKey": key, "flowId": res.Flow.ID, "problems": res.Problems, "missingActions": res.MissingActions}
	s.recordChange(c.Context(), subjectOperation, op.ID, actEdit, fmt.Sprintf("%s flow %q into FloMorphic", verb, key), "manual", ref)
	return s.respondDetail(c, updated)
}

// PUT /api/operations/:id/flows/:key — bind to an existing FloMorphic flow.
func (s *Server) linkOperationFlow(c fiber.Ctx) error {
	client := s.flo.Get()
	if client == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "FloMorphic API access is not configured (Settings → FloMorphic)")
	}
	op, err := s.getOp(c)
	if err != nil {
		return err
	}
	var body struct {
		FlowID string `json:"flowId"`
	}
	if err := c.Bind().Body(&body); err != nil || strings.TrimSpace(body.FlowID) == "" {
		return fiber.NewError(fiber.StatusBadRequest, "flowId is required")
	}
	key := c.Params("key")
	b, err := s.loadBundle(op)
	if err != nil {
		return err
	}
	if b.Manifest.Flow(key) == nil {
		return fiber.NewError(fiber.StatusNotFound, "no such flow in the manifest")
	}
	flow, err := client.GetFlow(c.Context(), strings.TrimSpace(body.FlowID))
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, "flow: "+err.Error())
	}
	binds := bindingsOf(op)
	binds[key] = binding{FlowID: flow.ID, FlowTitle: flow.Title, InstalledAt: time.Now()}
	updated, err := s.saveBindings(c.Context(), op, binds)
	if err != nil {
		return err
	}
	s.recordChange(c.Context(), subjectOperation, op.ID, actEdit, fmt.Sprintf("Linked flow %q to %s", key, flow.Title), "manual",
		map[string]any{"flowKey": key, "flowId": flow.ID})
	return s.respondDetail(c, updated)
}

// POST /api/operations/:id/run {flowKey, input?} — start an entry flow. The
// run is an activity on the operation; the flow gets the params as
// $.input.params (plus anything sent as input).
func (s *Server) runOperation(c fiber.Ctx) error {
	client := s.flo.Get()
	if client == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "FloMorphic API access is not configured (Settings → FloMorphic)")
	}
	op, err := s.getOp(c)
	if err != nil {
		return err
	}
	var body struct {
		FlowKey string          `json:"flowKey"`
		Input   json.RawMessage `json:"input"`
	}
	if err := c.Bind().Body(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	b, err := s.loadBundle(op)
	if err != nil {
		return err
	}
	f := b.Manifest.Flow(body.FlowKey)
	if f == nil {
		return fiber.NewError(fiber.StatusNotFound, "no such flow in the manifest")
	}
	if f.Role != "entry" {
		return fiber.NewError(fiber.StatusBadRequest, "only an entry flow runs from the operation; an on-row flow is run from a pipeline row")
	}
	bnd, ok := bindingsOf(op)[f.Key]
	if !ok || bnd.FlowID == "" {
		return fiber.NewError(fiber.StatusConflict, "the flow is not installed in FloMorphic yet")
	}
	flow, err := client.GetFlow(c.Context(), bnd.FlowID)
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, "flow: "+err.Error())
	}
	input := map[string]any{"params": operations.Effective(b.Manifest, s.allParams(op)), "operation": map[string]any{"id": op.ID, "key": op.Key, "flowKey": f.Key}}
	if len(body.Input) > 0 {
		var extra map[string]any
		if err := json.Unmarshal(body.Input, &extra); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "input must be a JSON object")
		}
		for k, v := range extra {
			input[k] = v
		}
	}
	a, err := s.startRun(c.Context(), client, flow, subjectRef{Kind: subjectOperation, ID: op.ID}, f.Title, mustJSON(input))
	if err != nil {
		if a == nil {
			return fiber.NewError(fiber.StatusBadGateway, err.Error())
		}
		return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"activities": []db.Activity{*a}, "error": err.Error()})
	}
	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"activities": []db.Activity{*a}})
}

// operationDoc is the operation as a run's context row ($.row).
func (s *Server) operationDoc(ctx context.Context, id int64) (map[string]any, error) {
	op, err := s.q.GetOperation(ctx, id)
	if err != nil {
		return nil, err
	}
	b, err := s.loadBundle(op)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"id": op.ID, "key": op.Key, "name": op.Name, "version": op.Version, "description": op.Description,
		"tags": op.Tags, "scale": op.Scale, "params": operations.Effective(b.Manifest, s.allParams(op)),
		"bindings": bindingsOf(op),
	}, nil
}
