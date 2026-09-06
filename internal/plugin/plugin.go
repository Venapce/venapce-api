// Package plugin embeds venapce's Inflowenger/FloMorphic plugin in the API
// process. Unlike a standalone plugin it does not run on its own: it connects to
// infra using the plugin env venapce already stores (from the FloMorphic panel,
// see internal/httpapi/flomorphic.go), and its handlers use venapce's own
// database pool and osctrl client — so there is no settings profile to fill.
//
// Two modules hang off it: db (db.issues.* / db.stages.* writes) and osquery
// (osquery.query plus its node/environment pickers).
package plugin

import (
	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Venapce/venapce-api/internal/osctrl"
	pdb "github.com/Venapce/venapce-api/internal/plugin/db"
	"github.com/Venapce/venapce-api/internal/plugin/osquery"
)

// version is the plugin's advertised version on the node palette.
const version = "v0.1.0"

// register wires the intro and every module's actions and metas onto a plugin.
// It declares no settings form: the connections are venapce's, injected here, so
// there is nothing for an operator to configure per node.
func register(p *sdkv1.Plugin, pool *pgxpool.Pool, osc *osctrl.Manager) {
	p.Intro(sdkv1.PluginIntro{
		Name:    "VENAPCE",
		Author:  "Venapce",
		Version: version,
	})
	p.AddAction(pdb.Actions(pool)...)
	p.AddAction(osquery.Actions(osc)...)
	p.AddMeta(osquery.Metas(osc)...)
}
