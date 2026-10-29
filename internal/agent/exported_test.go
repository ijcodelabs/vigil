package agent

import (
	"testing"

	"github.com/ijcodelabs/vigil/internal/model"
)

func TestExportAgentLabel(t *testing.T) {
	shared := map[string]string{"agent": "agt-1", "gateway": "gw-1"}
	batch := []model.Sample{
		{Name: "boop_jump_agent_connected", Labels: shared},
		{Name: "boop_jump_agent_streams_open", Labels: shared},
		{Name: "already", Labels: map[string]string{"agent": "b", "exported_agent": "a"}},
		{Name: "none", Labels: map[string]string{"job": "x"}},
	}
	exportAgentLabel(batch)
	if l := batch[0].Labels; l["exported_agent"] != "agt-1" || l["gateway"] != "gw-1" || l["agent"] != "" {
		t.Errorf("renamed: %v", l)
	}
	if shared["agent"] != "agt-1" {
		t.Error("a map shared with the caller was changed")
	}
	if l := batch[2].Labels; l["exported_agent"] != "a" || len(l) != 1 {
		t.Errorf("existing exported_agent: %v", l)
	}
	if l := batch[3].Labels; len(l) != 1 {
		t.Errorf("untouched: %v", l)
	}
}
