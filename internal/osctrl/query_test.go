package osctrl

import "testing"

func TestDistributedQueryDone(t *testing.T) {
	cases := []struct {
		name string
		q    DistributedQuery
		want bool
	}{
		{"completed flag", DistributedQuery{Completed: true}, true},
		{"expired flag", DistributedQuery{Expired: true}, true},
		{"all reported", DistributedQuery{Expected: 2, Executions: 1, Errors: 1}, true},
		{"over-reported", DistributedQuery{Expected: 1, Executions: 2}, true},
		{"still waiting", DistributedQuery{Expected: 3, Executions: 1}, false},
		{"target unresolved", DistributedQuery{Expected: 0, Executions: 0}, false},
	}
	for _, c := range cases {
		if got := c.q.Done(); got != c.want {
			t.Errorf("%s: Done()=%v want %v", c.name, got, c.want)
		}
	}
}
