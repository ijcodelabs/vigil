package fleet

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// Query is a query worth keeping: Explore offers it by name under "Saved".
// The same three fields are what a person saves there, what an integration
// brings along and what a file under queries/ holds for `vigil apply`.
type Query struct {
	Name  string `yaml:"name" json:"name"`
	Type  string `yaml:"type,omitempty" json:"type"` // metrics (PromQL, the default) or logs (LogsQL)
	Query string `yaml:"query" json:"query"`
}

const (
	QueryMetrics = "metrics"
	QueryLogs    = "logs"

	maxQueryName = 100
	maxQueryText = 10000
)

// Check tidies a query and says what is wrong with it. Whether the
// expression means anything is for the store to say when it is run.
func (q *Query) Check() error {
	q.Name, q.Query = strings.TrimSpace(q.Name), strings.TrimSpace(q.Query)
	switch q.Type {
	case "":
		q.Type = QueryMetrics
	case QueryMetrics, QueryLogs:
	default:
		return fmt.Errorf("query %q: type must be %q or %q", q.Name, QueryMetrics, QueryLogs)
	}
	if q.Name == "" || len(q.Name) > maxQueryName || strings.ContainsAny(q.Name, "\r\n") {
		return fmt.Errorf("a query needs a name of one line, up to %d characters", maxQueryName)
	}
	if q.Query == "" || len(q.Query) > maxQueryText {
		return fmt.Errorf("query %q: the query is empty or longer than %d characters", q.Name, maxQueryText)
	}
	return nil
}

// Key is what makes two queries the same one: the name, within its type.
func (q *Query) Key() string { return q.Type + "\x00" + q.Name }

// CheckQueries checks a list and refuses two of the same name and type.
func CheckQueries(list []Query) error {
	seen := map[string]bool{}
	for i := range list {
		if err := list[i].Check(); err != nil {
			return err
		}
		if seen[list[i].Key()] {
			return fmt.Errorf("query %q (%s) is defined twice", list[i].Name, list[i].Type)
		}
		seen[list[i].Key()] = true
	}
	return nil
}

// ParseQueries reads a file of queries:
//
//	queries:
//	  - name: Slow responses by path
//	    type: logs
//	    query: app:nginx status:~"5.." | stats by (path) count() as n
func ParseQueries(text string) ([]Query, error) {
	var doc struct {
		Queries []Query `yaml:"queries"`
	}
	dec := yaml.NewDecoder(bytes.NewReader([]byte(text)))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if err := CheckQueries(doc.Queries); err != nil {
		return nil, err
	}
	return doc.Queries, nil
}
