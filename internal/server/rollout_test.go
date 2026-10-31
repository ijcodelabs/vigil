package server_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fakeAgent polls for config the way an agent does, and remembers what it runs.
type fakeAgent struct {
	id, hash, config string
	refuse           bool // behaves as if it could not apply what it is given
}

func (a *fakeAgent) poll(t *testing.T, base string) {
	t.Helper()
	report := map[string]any{"labels": map[string]string{"role": "web"}, "facts": map[string]string{"os": "linux"}, "config_hash": a.hash}
	if a.refuse {
		report["config_error"] = "cannot apply: disk on fire"
	}
	body, _ := json.Marshal(report)
	req, _ := http.NewRequest("POST", base+"/api/v1/agent/config", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer ingest")
	req.Header.Set("X-Vigil-Agent", a.id)
	if a.hash != "" {
		req.Header.Set("If-None-Match", `"`+a.hash+`"`)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		buf := new(strings.Builder)
		fmt.Fprint(buf, readAll(resp))
		if !a.refuse {
			a.config, a.hash = buf.String(), strings.Trim(resp.Header.Get("ETag"), `"`)
		}
	case http.StatusNoContent:
		a.config, a.hash = "", ""
	case http.StatusNotModified:
	default:
		t.Fatalf("poll of %s: %d", a.id, resp.StatusCode)
	}
}

func readAll(resp *http.Response) string {
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			return b.String()
		}
	}
}

func TestStagedRollout(t *testing.T) {
	_, central := startServer(t, "http://127.0.0.1:1")
	c := newClient(t, central.URL)
	if code := c.login("admin", "admin-token-secret"); code != 204 {
		t.Fatalf("login: %d", code)
	}
	agents := []*fakeAgent{{id: "w1"}, {id: "w2"}, {id: "w3"}, {id: "w4"}}
	pollAll := func() {
		t.Helper()
		for _, a := range agents {
			a.poll(t, central.URL)
		}
	}
	with := func(text string) (n int) {
		for _, a := range agents {
			if strings.Contains(a.config, text) {
				n++
			}
		}
		return n
	}
	type info struct {
		Profile, Status, Reason  string
		From, To, Step, Affected int
		Target, Applied          int
		Ready                    bool
		Agents                   []struct{ ID, State, Error string }
		StepsTotal               int `json:"steps_total"`
	}
	rollouts := func() []info {
		t.Helper()
		code, body, _ := c.do("GET", "/api/v1/fleet/rollouts", "")
		var out []info
		if err := json.Unmarshal([]byte(body), &out); code != 200 || err != nil {
			t.Fatalf("rollouts: %d %s %v", code, body, err)
		}
		return out
	}
	put := func(name, doc string) string {
		t.Helper()
		code, body, _ := c.do("PUT", "/api/v1/fleet/profiles/"+name, doc)
		if code != 200 {
			t.Fatalf("save %s: %d %s", name, code, body)
		}
		return body
	}
	const v1 = "select: ['role=\"web\"']\nconfig: {interval: 30s}\n"
	const staged = "select: ['role=\"web\"']\nrollout: {steps: [1, 50%]}\nconfig: {interval: 10s}\n"

	pollAll()
	if body := put("web", v1); !strings.Contains(body, `"rollout":false`) {
		t.Errorf("plain save: %s", body)
	}
	pollAll()
	if with("interval: 30s") != 4 {
		t.Fatalf("everyone should have the first version")
	}

	// A change with a rollout section reaches one agent first.
	if body := put("web", staged); !strings.Contains(body, `"rollout":true`) {
		t.Errorf("staged save: %s", body)
	}
	pollAll()
	if with("interval: 10s") != 1 || with("interval: 30s") != 3 {
		t.Fatalf("step 1: %d new, %d old", with("interval: 10s"), with("interval: 30s"))
	}
	r := rollouts()
	if len(r) != 1 || r[0].Profile != "web" || r[0].From != 1 || r[0].To != 2 || r[0].Step != 1 || r[0].StepsTotal != 3 ||
		r[0].Affected != 4 || r[0].Target != 1 || len(r[0].Agents) != 1 || r[0].Ready || r[0].Status != "running" {
		t.Fatalf("after admitting: %+v", r)
	}
	first := r[0].Agents[0].ID
	// It stays at one agent however often the others ask.
	pollAll()
	pollAll()
	if r = rollouts(); with("interval: 10s") != 1 || r[0].Applied != 1 || !r[0].Ready || r[0].Agents[0].State != "applied" {
		t.Fatalf("after the first agent applied: %+v", r)
	}
	// An unknown action is refused.
	if code, _, _ := c.do("POST", "/api/v1/fleet/rollouts/web/explode", ""); code != 400 {
		t.Errorf("unknown action: %d", code)
	}
	if code, _, _ := c.do("POST", "/api/v1/fleet/rollouts/nope/promote", ""); code != 404 {
		t.Errorf("promote without a rollout: %d", code)
	}

	// Promote: half of the four.
	if code, body, _ := c.do("POST", "/api/v1/fleet/rollouts/web/promote", ""); code != 204 {
		t.Fatalf("promote: %d %s", code, body)
	}
	if r = rollouts(); r[0].Step != 2 || r[0].Target != 2 || len(r[0].Agents) != 2 {
		t.Fatalf("step 2: %+v", r)
	}
	// The second agent cannot apply the change: the rollout stops.
	var second *fakeAgent
	for _, a := range agents {
		if a.id != first && (a.id == r[0].Agents[0].ID || a.id == r[0].Agents[1].ID) {
			second = a
		}
	}
	second.refuse = true
	pollAll()
	if r = rollouts(); r[0].Status != "running" {
		t.Fatalf("the report sent while receiving the change must not count: %+v", r)
	}
	pollAll()
	if r = rollouts(); r[0].Status != "paused" || !strings.Contains(r[0].Reason, second.id) || !strings.Contains(r[0].Reason, "disk on fire") {
		t.Fatalf("after a refusal: %+v", r)
	}
	// Paused means nobody else is admitted.
	pollAll()
	if with("interval: 10s") != 1 {
		t.Errorf("paused rollout spread: %d", with("interval: 10s"))
	}

	// Abort: everyone is back on the first version, which is saved as a new one.
	if code, body, _ := c.do("POST", "/api/v1/fleet/rollouts/web/abort", ""); code != 204 {
		t.Fatalf("abort: %d %s", code, body)
	}
	second.refuse = false
	pollAll()
	if with("interval: 30s") != 4 || len(rollouts()) != 0 {
		t.Fatalf("after abort: %d on the old version, rollouts %+v", with("interval: 30s"), rollouts())
	}
	if _, body, _ := c.do("GET", "/api/v1/fleet/profiles/web", ""); !strings.Contains(body, `"version":3`) || strings.Contains(body, "rollout") {
		t.Errorf("profile after abort: %s", body)
	}

	// With a wait, steps follow by themselves once the agents are healthy.
	// A step moves on at a tick (each listing is one) once the wait has
	// passed since it became healthy. A round here can take less than the
	// wait, so the test waits for the outcome rather than counting rounds,
	// which failed on fast machines.
	put("web", "select: ['role=\"web\"']\nrollout: {steps: [1, 2], wait: 1ms}\nconfig: {interval: 20s}\n")
	for deadline := time.Now().Add(5 * time.Second); len(rollouts()) > 0 && time.Now().Before(deadline); {
		pollAll()
		pollAll()
		time.Sleep(2 * time.Millisecond)
	}
	pollAll()
	if len(rollouts()) != 0 || with("interval: 20s") != 4 {
		t.Fatalf("automatic rollout: %d agents, rollouts %+v", with("interval: 20s"), rollouts())
	}

	// A fix saved during a rollout continues on the same agents.
	put("web", "select: ['role=\"web\"']\nrollout: {steps: [1]}\nconfig: {interval: 40s}\n")
	pollAll()
	r = rollouts()
	canary := r[0].Agents[0].ID
	put("web", "select: ['role=\"web\"']\nrollout: {steps: [1]}\nconfig: {interval: 45s}\n")
	pollAll()
	if r = rollouts(); len(r) != 1 || r[0].From != 4 || r[0].To != 6 || len(r[0].Agents) != 1 || r[0].Agents[0].ID != canary ||
		with("interval: 45s") != 1 || with("interval: 20s") != 3 {
		t.Fatalf("fix during a rollout: %+v, %d on 45s, %d on 20s", r, with("interval: 45s"), with("interval: 20s"))
	}
	// "Everyone now" ends it.
	if code, _, _ := c.do("POST", "/api/v1/fleet/rollouts/web/complete", ""); code != 204 {
		t.Fatalf("complete: %d", code)
	}
	pollAll()
	if with("interval: 45s") != 4 || len(rollouts()) != 0 {
		t.Fatalf("after complete: %d", with("interval: 45s"))
	}

	// A save can skip the rollout the profile asks for.
	if code, body, _ := c.do("PUT", "/api/v1/fleet/profiles/web?rollout=skip", "select: ['role=\"web\"']\nrollout: {steps: [1]}\nconfig: {interval: 50s}\n"); code != 200 || !strings.Contains(body, `"rollout":false`) {
		t.Fatalf("skip: %d %s", code, body)
	}
	pollAll()
	if with("interval: 50s") != 4 {
		t.Fatalf("skipped rollout: %d", with("interval: 50s"))
	}

	// A new profile is staged too: the others do not get it at all yet.
	put("extra", "select: ['role=\"web\"']\npriority: 5\nrollout: {steps: [1]}\nconfig: {labels: {extra: \"yes\"}}\n")
	pollAll()
	if r = rollouts(); with("extra:") != 1 || len(r) != 1 || r[0].From != 0 {
		t.Fatalf("new profile: %d agents have it, %+v", with("extra:"), r)
	}
	// Aborting a new profile removes it.
	c.do("POST", "/api/v1/fleet/rollouts/extra/abort", "")
	pollAll()
	if code, _, _ := c.do("GET", "/api/v1/fleet/profiles/extra", ""); code != 404 || with("extra:") != 0 {
		t.Errorf("aborted new profile: %d, %d agents still have it", code, with("extra:"))
	}

	// A change that makes no difference to anyone is not a rollout.
	if body := put("web", "select: ['role=\"web\"']\nrollout: {steps: [1, 2]}\nconfig: {interval: 50s}\n"); !strings.Contains(body, `"rollout":false`) {
		t.Errorf("no-op change: %s", body)
	}
	// Bad rollout sections are refused with a reason.
	for want, doc := range map[string]string{
		"one to ten steps": "rollout: {steps: []}",
		"number of agents": "rollout: {steps: [none]}",
		"share of them":    "rollout: {steps: [150%]}",
	} {
		if code, body, _ := c.do("PUT", "/api/v1/fleet/profiles/bad", doc); code != 400 || !strings.Contains(body, want) {
			t.Errorf("%s: %d %s", doc, code, body)
		}
	}
}
