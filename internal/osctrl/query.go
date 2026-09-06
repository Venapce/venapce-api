package osctrl

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
)

// This file adds the distributed-query surface of the osctrl API on top of the
// auth/plumbing in client.go. It backs the venapce plugin's `osquery.query`
// action: dispatch an ad-hoc osquery SQL to selected nodes, then poll for the
// rows each agent reports on its next check-in.
//
// The three calls map one-to-one onto osctrl-api:
//   - RunQuery      POST /api/v1/queries/{env}                — dispatch, returns a name
//   - QueryStatus   GET  /api/v1/queries/{env}/{name}         — per-query progress
//   - QueryResults  GET  /api/v1/queries/{env}/results/{name} — the collected rows

// DistributedQueryRequest is the body of POST /queries/{env}. Only Query and one
// targeting list are needed in practice; the zero value of the rest is omitted by
// osctrl. Targeting is by environment / platform / node UUID / hostname — the
// plugin sends a single UUID (the node the user picked).
type DistributedQueryRequest struct {
	Query           string   `json:"query"`
	EnvironmentList []string `json:"environment_list,omitempty"`
	PlatformList    []string `json:"platform_list,omitempty"`
	UUIDList        []string `json:"uuid_list,omitempty"`
	HostList        []string `json:"host_list,omitempty"`
	TagList         []string `json:"tag_list,omitempty"`
	ExpHours        int      `json:"exp_hours,omitempty"`
	Hidden          bool     `json:"hidden,omitempty"`
}

// DistributedQuery is the subset of osctrl's query record the plugin needs to
// decide when a distributed query is finished. A query is done when every
// targeted node has reported (Executions+Errors >= Expected) or osctrl has marked
// it Completed/Expired.
type DistributedQuery struct {
	Name       string `json:"name"`
	Query      string `json:"query"`
	Target     string `json:"target"`
	Expected   int    `json:"expected"`
	Executions int    `json:"executions"`
	Errors     int    `json:"errors"`
	Active     bool   `json:"active"`
	Completed  bool   `json:"completed"`
	Expired    bool   `json:"expired"`
}

// Done reports whether osctrl considers this distributed query finished: it has
// completed or expired, or every expected node has reported (a success or an
// error). Expected==0 means osctrl has not resolved the target set yet, so that
// case is deliberately not "done".
func (q *DistributedQuery) Done() bool {
	if q.Completed || q.Expired {
		return true
	}
	return q.Expected > 0 && q.Executions+q.Errors >= q.Expected
}

// QueryResults is a page of the rows a distributed query collected. Each item is
// one osquery result row (arbitrary columns), so it stays a free-form map.
type QueryResults struct {
	Items      []map[string]any `json:"items"`
	Page       int              `json:"page"`
	PageSize   int              `json:"page_size"`
	TotalItems int              `json:"total_items"`
	TotalPages int              `json:"total_pages"`
	Since      string           `json:"since"`
}

// RunQuery dispatches a distributed query to an environment and returns the
// generated query name, the handle the status/results calls key off. Dispatch is
// asynchronous: this only queues the query — the rows arrive later, via
// QueryResults, as each agent checks in.
func (c *Client) RunQuery(ctx context.Context, env string, req DistributedQueryRequest) (string, error) {
	// osctrl scopes the query to the path env, but also reads the body's
	// environment_list; keep them in sync so the target set is unambiguous.
	if len(req.EnvironmentList) == 0 && env != "" {
		req.EnvironmentList = []string{env}
	}
	raw, err := c.postRaw(ctx, "/queries/"+env, req)
	if err != nil {
		return "", err
	}
	var out struct {
		QueryName string `json:"query_name"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("osctrl run query: unreadable response: %w", err)
	}
	if out.QueryName == "" {
		return "", fmt.Errorf("osctrl run query: empty query name in response")
	}
	return out.QueryName, nil
}

// QueryStatus reads one distributed query's progress record.
func (c *Client) QueryStatus(ctx context.Context, env, name string) (*DistributedQuery, error) {
	raw, err := c.getRaw(ctx, "/queries/"+env+"/"+url.PathEscape(name))
	if err != nil {
		return nil, err
	}
	var q DistributedQuery
	if err := json.Unmarshal(raw, &q); err != nil {
		return nil, fmt.Errorf("osctrl query status: unreadable response: %w", err)
	}
	return &q, nil
}

// QueryResults reads a page of a distributed query's collected rows. pageSize<=0
// leaves it to osctrl's default; since is an optional RFC3339 lower bound.
func (c *Client) QueryResults(ctx context.Context, env, name string, page, pageSize int, since string) (*QueryResults, error) {
	q := url.Values{}
	if page > 0 {
		q.Set("page", strconv.Itoa(page))
	}
	if pageSize > 0 {
		q.Set("page_size", strconv.Itoa(pageSize))
	}
	if since != "" {
		q.Set("since", since)
	}
	path := "/queries/" + env + "/results/" + url.PathEscape(name)
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	raw, err := c.getRaw(ctx, path)
	if err != nil {
		return nil, err
	}
	var res QueryResults
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("osctrl query results: unreadable response: %w", err)
	}
	return &res, nil
}
