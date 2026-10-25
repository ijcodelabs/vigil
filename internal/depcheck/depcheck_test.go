package depcheck

import "testing"

func TestMatches(t *testing.T) {
	for _, c := range []struct {
		dep, prefix string
		want        bool
	}{
		{"github.com/prometheus/alertmanager/pkg/labels", "github.com/prometheus/alertmanager", true},
		{"github.com/prometheus/alertmanager", "github.com/prometheus/alertmanager", true},
		{"github.com/ijcodelabs/vigil/internal/serverless", "github.com/ijcodelabs/vigil/internal/server", false},
		{"github.com/aws/smithy-go", "github.com/aws/", true},
		{"github.com/awsome/x", "github.com/aws/", false},
	} {
		if got := matches(c.dep, c.prefix); got != c.want {
			t.Errorf("matches(%q, %q) = %v", c.dep, c.prefix, got)
		}
	}
}
