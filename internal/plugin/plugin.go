// Package plugin embeds venapce's Inflowenger/FloMorphic plugin in the API
// process. Unlike a standalone plugin it does not run on its own: it connects to
// infra using the plugin env venapce already stores (from the FloMorphic panel,
// see internal/httpapi/flomorphic.go), and its handlers use venapce's own
// database pool and osctrl client — so there is no settings profile to fill.
//
// Two modules hang off it: db (db.stages.* / db.findings.* / db.issues.* writes) and osquery
// (osquery.query / osquery.queryByTags plus their node/tag/environment pickers).
package plugin

import (
	"github.com/Inflowenger/go-plugin-sdk/jobstop"
	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Venapce/venapce-api/internal/osctrl"
	pdb "github.com/Venapce/venapce-api/internal/plugin/db"
	"github.com/Venapce/venapce-api/internal/plugin/osquery"
)

// version is the plugin's advertised version on the node palette.
const version = "v0.1.0"

// register wires the intro, the signal port and every module's actions and metas
// onto a plugin. It declares no settings form: the connections are venapce's,
// injected here, so there is nothing for an operator to configure per node.
//
// stops is the plugin's stop capability (SDK v0.2.8's jobstop): its signal
// handler goes on the port here, and its middleware on the actions that must not
// outlive their flow — the osquery queries. Registering the port is not optional
// book-keeping: without it the port has no subscription, no stop ever reaches a
// job, and the SDK says so on Start.
func register(p *sdkv1.Plugin, stops *jobstop.Registry, pool *pgxpool.Pool, osc *osctrl.Manager) {
	p.Intro(sdkv1.PluginIntro{
		Name:    "VENAPCE",
		Author:  "Venapce",
		Version: version,
	})
	// LogSignals keeps the line-per-signal that OnSignal(nil) would have given
	// us; registering a handler of our own would otherwise silence the port just
	// as we start acting on it. Every process signal of the plugin lands here,
	// including jobs of other flows, so a logged jobId with no jobstop line
	// beside it was not work this process was running.
	p.OnSignal(sdkv1.ChainSignals(sdkv1.LogSignals("venapce plugin"), stops.OnSignal))
	p.AddAction(pdb.Actions(pool)...)
	p.AddAction(osquery.Actions(osc, stops)...)
	p.AddMeta(osquery.Metas(osc)...)
}
