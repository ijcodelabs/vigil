package fleet

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ijcodelabs/vigil/internal/config"
)

// Rollout says how a change to a profile spreads:
//
//	rollout:
//	  steps: [1, 10%, 50%]   # agents that get the change at each step; after the last, everyone
//	  wait: 10m              # healthy for this long before the next step;
//	                         # left out, each step waits for someone to promote it
type Rollout struct {
	Steps []Step          `yaml:"steps" json:"steps"`
	Wait  config.Duration `yaml:"wait" json:"wait"`
}

// Step is a number of agents or a share of the agents the change affects.
type Step struct {
	Count   int
	Percent float64
}

func (s *Step) UnmarshalYAML(n *yaml.Node) error {
	parsed, err := ParseStep(n.Value)
	if err != nil || n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: a step is a number of agents (3) or a share of them (25%%)", n.Line)
	}
	*s = parsed
	return nil
}

func ParseStep(text string) (Step, error) {
	text = strings.TrimSpace(text)
	if rest, ok := strings.CutSuffix(text, "%"); ok {
		v, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
		if err != nil || v <= 0 || v >= 100 {
			return Step{}, errors.New("a share is more than 0% and less than 100%")
		}
		return Step{Percent: v}, nil
	}
	v, err := strconv.Atoi(text)
	if err != nil || v < 1 {
		return Step{}, errors.New("a step is a number of agents or a share of them")
	}
	return Step{Count: v}, nil
}

func (s Step) String() string {
	if s.Percent > 0 {
		return strconv.FormatFloat(s.Percent, 'f', -1, 64) + "%"
	}
	return strconv.Itoa(s.Count)
}

func (s Step) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(s.String())), nil }

func (s *Step) UnmarshalJSON(b []byte) error {
	text, err := strconv.Unquote(string(b))
	if err != nil {
		return err
	}
	*s, err = ParseStep(text)
	return err
}

// Target is how many of n affected agents the step asks for: at least one,
// so a small share of a small fleet still tries the change somewhere.
func (s Step) Target(n int) int {
	t := s.Count
	if s.Percent > 0 {
		t = int(math.Ceil(float64(n) * s.Percent / 100))
	}
	return max(1, min(t, n))
}

func (r *Rollout) check() error {
	if r == nil {
		return nil
	}
	if len(r.Steps) == 0 || len(r.Steps) > 10 {
		return errors.New("give one to ten steps, for example steps: [1, 25%]")
	}
	if r.Wait < 0 {
		return errors.New("wait cannot be negative")
	}
	return nil
}
