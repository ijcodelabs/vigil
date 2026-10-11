package server

import (
	"cmp"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ijcodelabs/vigil/internal/fleet"
)

// A rollout spreads one change of one profile in steps. While it runs the
// profile exists twice: agents admitted so far get the new version, the
// others the version from before (or, for a new profile, none).
//
// The rollout stops by itself when an admitted agent cannot apply the
// change or stops reporting after getting it.
type rollout struct {
	Profile string       `json:"profile"`
	From    int          `json:"from"` // version the others stay on; 0: the profile is new
	To      int          `json:"to"`
	Steps   []fleet.Step `json:"steps"`
	Wait    float64      `json:"wait_seconds"` // 0: each step waits to be promoted
	Stage   int          `json:"stage"`        // index into Steps
	Status  string       `json:"status"`       // running or paused
	Reason  string       `json:"reason,omitempty"`
	Author  string       `json:"author"`
	Started time.Time    `json:"started"`
	// HealthySince is when every agent of the current step had applied the change.
	HealthySince time.Time `json:"healthy_since"`
	Admitted     []string  `json:"admitted"`

	in        map[string]bool   // Admitted as a set
	sincePoll map[string]uint64 // poll count of each agent when it was admitted
	stable    *fleet.Profile    // nil: the profile did not exist before
	stableDoc string
}

const (
	rolloutRunning = "running"
	rolloutPaused  = "paused"
)

func (ro *rollout) wait() time.Duration { return time.Duration(ro.Wait * float64(time.Second)) }

func (ro *rollout) admit(id string, polls uint64) {
	if !ro.in[id] {
		ro.in[id] = true
		ro.sincePoll[id] = polls
		ro.Admitted = append(ro.Admitted, id)
	}
}

func (ro *rollout) drop(id string) {
	delete(ro.in, id)
	delete(ro.sincePoll, id)
	ro.Admitted = slices.DeleteFunc(ro.Admitted, func(a string) bool { return a == id })
}

// profilesFor returns the profiles as this agent sees them: for a profile
// that is being rolled out, the new version if the agent was admitted and
// the earlier one otherwise. Called with f.mu held.
func (f *fleetState) profilesFor(id string) []*fleet.Profile {
	if len(f.rollouts) == 0 {
		return f.profiles
	}
	return f.profilesWith(id, "", false)
}

// profilesWith is profilesFor with the choice for one profile forced.
func (f *fleetState) profilesWith(id, profile string, next bool) []*fleet.Profile {
	out := make([]*fleet.Profile, 0, len(f.profiles))
	for _, p := range f.profiles {
		ro := f.rollouts[p.Name]
		useNext := ro == nil || ro.in[id]
		if p.Name == profile {
			useNext = next
		}
		switch {
		case useNext:
			out = append(out, p)
		case ro != nil && ro.stable != nil:
			out = append(out, ro.stable)
		}
	}
	return out
}

// current resolves the config an agent gets right now. Called with f.mu held.
func (f *fleetState) current(id string, attrs map[string]string) (*fleet.Resolved, error) {
	return f.resolveFor(f.profilesFor(id), f.resolved, attrs)
}

// rolloutOrder spreads agents evenly and differently for every change, so
// the same hosts are not the first to get everything.
func rolloutOrder(ro *rollout, id string) uint64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%s\x00%d\x00%s", ro.Profile, ro.From, id)
	return h.Sum64()
}

// RolloutAgent is one agent of a rollout as the interface shows it.
type RolloutAgent struct {
	ID    string `json:"id"`
	State string `json:"state"` // applied, pending, failed or silent
	Error string `json:"error,omitempty"`
}

// RolloutInfo describes a rollout for the interface.
type RolloutInfo struct {
	*rollout
	StepNumber int            `json:"step"` // 1-based
	StepCount  int            `json:"steps_total"`
	Affected   int            `json:"affected"` // agents whose config the change alters
	Target     int            `json:"target"`   // how many the current step asks for
	Applied    int            `json:"applied"`
	Ready      bool           `json:"ready"`             // the step is healthy and waits to be promoted
	NextAt     *time.Time     `json:"next_at,omitempty"` // when the next step starts by itself
	Agents     []RolloutAgent `json:"agents"`            // admitted agents, the first hundred
}

type rolloutView struct {
	affected []string // sorted by rolloutOrder
	agents   []RolloutAgent
	applied  int
	failure  string
	waiting  []string // affected, reporting, not admitted yet
}

// look works out where a rollout stands. Called with f.mu held.
func (s *Server) look(ro *rollout, now time.Time) rolloutView {
	f := &s.fleet
	var v rolloutView
	quiet := max(s.cfg.HeartbeatTimeout.Std(), 2*time.Minute)
	cache := map[string]*resolveResult{}
	for id, attrs := range f.attrs {
		before, errB := f.resolveWith(f.profilesWith(id, ro.Profile, false), f.catalog, cache, attrs)
		after, errA := f.resolveWith(f.profilesWith(id, ro.Profile, true), f.catalog, cache, attrs)
		if errA == nil && errB == nil && before.Hash == after.Hash {
			continue // the change makes no difference to this agent
		}
		v.affected = append(v.affected, id)
	}
	slices.SortFunc(v.affected, func(a, b string) int {
		return cmp.Or(cmp.Compare(rolloutOrder(ro, a), rolloutOrder(ro, b)), cmp.Compare(a, b))
	})
	for _, id := range v.affected {
		seen, known := f.seen[id]
		silent := (known && now.Sub(seen) > quiet) || (!known && now.Sub(s.started) > quiet)
		if !ro.in[id] {
			if known && !silent {
				v.waiting = append(v.waiting, id)
			}
			continue
		}
		a := RolloutAgent{ID: id, State: "pending"}
		want, err := f.current(id, f.attrs[id])
		hash, problem, _ := strings.Cut(f.reports[id], "\x00")
		// The poll during which an agent receives the change still reports
		// what it ran before; only the one after tells how it went.
		judged := f.polls[id] >= ro.sincePoll[id]+2
		switch {
		case err != nil:
			a.State, a.Error = "failed", err.Error()
		case want != nil && hash == want.Hash:
			a.State = "applied"
			v.applied++
		case judged && problem != "":
			a.State, a.Error = "failed", problem
		}
		if a.State == "failed" && v.failure == "" {
			v.failure = fmt.Sprintf("agent %s could not apply the change: %s", id, a.Error)
		}
		if silent {
			a.State = "silent"
			if v.failure == "" {
				v.failure = fmt.Sprintf("agent %s stopped reporting after it got the change", id)
			}
		}
		v.agents = append(v.agents, a)
	}
	return v
}

// stepRollout moves one rollout forward if it can. done means the change
// has reached everyone it is going to reach. Called with f.mu held.
func (s *Server) stepRollout(ro *rollout, now time.Time) (dirty, done bool) {
	v := s.look(ro, now)
	if len(v.affected) == 0 || ro.Stage >= len(ro.Steps) {
		return true, true
	}
	if ro.Status != rolloutRunning {
		return false, false
	}
	if v.failure != "" {
		ro.Status, ro.Reason, ro.HealthySince = rolloutPaused, v.failure, time.Time{}
		s.log.Warn("rollout paused", "profile", ro.Profile, "reason", v.failure)
		return true, false
	}
	target := ro.Steps[ro.Stage].Target(len(v.affected))
	if missing := target - len(v.agents); missing > 0 && len(v.waiting) > 0 {
		for _, id := range v.waiting[:min(missing, len(v.waiting))] {
			ro.admit(id, s.fleet.polls[id])
		}
		ro.HealthySince = time.Time{}
		return true, false
	}
	// The step is healthy when everyone in it has applied the change. If
	// fewer agents are reporting than the step asks for, those there are count.
	if len(v.agents) == 0 || v.applied < len(v.agents) {
		if !ro.HealthySince.IsZero() {
			ro.HealthySince = time.Time{}
			return true, false
		}
		return false, false
	}
	if ro.HealthySince.IsZero() {
		ro.HealthySince = now
		dirty = true
	}
	if ro.Wait > 0 && now.Sub(ro.HealthySince) >= ro.wait() {
		ro.Stage++
		ro.HealthySince = time.Time{}
		if len(v.agents) == len(v.affected) {
			return true, true // everyone it affects already has it; further steps would only wait
		}
		s.log.Info("rollout moves to the next step", "profile", ro.Profile, "step", ro.Stage+1, "of", len(ro.Steps)+1)
		return true, ro.Stage >= len(ro.Steps)
	}
	return dirty, false
}

// rolloutTick moves every rollout forward and saves what changed.
func (s *Server) rolloutTick(now time.Time) {
	s.rolloutMu.Lock()
	defer s.rolloutMu.Unlock()
	s.stepRollouts(now)
}

func (s *Server) stepRollouts(now time.Time) {
	f := &s.fleet
	f.mu.Lock()
	var save []*rollout
	var finished []string
	for name, ro := range f.rollouts {
		dirty, done := s.stepRollout(ro, now)
		switch {
		case done:
			finished = append(finished, name)
			f.endRollout(name)
		case dirty:
			save = append(save, ro)
		}
	}
	docs := make(map[string]string, len(save))
	for _, ro := range save {
		b, _ := json.Marshal(ro)
		docs[ro.Profile] = string(b)
	}
	f.mu.Unlock()
	for name, doc := range docs {
		if err := s.store.SetRollout(name, doc); err != nil {
			s.log.Error("saving rollout state failed", "profile", name, "err", err)
		}
	}
	for _, name := range finished {
		s.log.Info("rollout finished: every agent gets the new version", "profile", name)
		if err := s.store.DeleteRollout(name); err != nil {
			s.log.Error("removing finished rollout failed", "profile", name, "err", err)
		}
	}
}

// endRollout forgets a rollout: from now on everyone gets the profile as it
// is stored. Called with f.mu held.
func (f *fleetState) endRollout(name string) {
	if ro := f.rollouts[name]; ro != nil {
		delete(f.isStable, ro.stable)
		delete(f.rollouts, name)
	}
}

// beginRollout registers that the next save of a profile is to be rolled
// out, before the save: agents keep getting what they have until they are
// admitted. A rollout already running for the profile keeps its agents and
// its step, so a fix to a change continues on the same hosts. Called with
// f.mu held.
func (f *fleetState) beginRollout(p *fleet.Profile, author string, now time.Time) *rollout {
	ro := f.rollouts[p.Name]
	if ro == nil {
		ro = &rollout{Profile: p.Name, Author: author, Started: now, in: map[string]bool{}, sincePoll: map[string]uint64{}}
		if row, ok := f.rows[p.Name]; ok {
			ro.From, ro.stableDoc = row.Version, row.Doc
			for _, old := range f.profiles {
				if old.Name == p.Name {
					ro.stable = old
				}
			}
		}
		if ro.stable != nil {
			f.isStable[ro.stable] = true
		}
		f.rollouts[p.Name] = ro
	}
	ro.Steps, ro.Wait = p.Rollout.Steps, p.Rollout.Wait.Std().Seconds()
	ro.Stage = min(ro.Stage, len(ro.Steps)-1)
	ro.Status, ro.Reason, ro.HealthySince = rolloutRunning, "", time.Time{}
	// Agents already admitted are judged afresh on the new text.
	for id := range ro.in {
		ro.sincePoll[id] = f.polls[id]
	}
	return ro
}

// loadRollouts restores the rollouts that were running when the server stopped.
func (s *Server) loadRollouts() error {
	stored, err := s.store.Rollouts()
	if err != nil {
		return err
	}
	f := &s.fleet
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, doc := range stored {
		ro := &rollout{in: map[string]bool{}, sincePoll: map[string]uint64{}}
		if _, ok := f.rows[name]; !ok || json.Unmarshal([]byte(doc), ro) != nil || len(ro.Steps) == 0 {
			s.log.Warn("dropping a stored rollout that no longer makes sense", "profile", name)
			s.store.DeleteRollout(name)
			continue
		}
		if ro.From > 0 {
			text, found, err := s.store.ProfileVersion(name, ro.From)
			if err == nil && found {
				ro.stable, err = fleet.ParseProfile(name, text)
			}
			if err != nil || !found {
				s.log.Warn("dropping a rollout whose earlier version is gone; every agent gets the new one", "profile", name)
				s.store.DeleteRollout(name)
				continue
			}
			ro.stableDoc = text
			f.isStable[ro.stable] = true
		}
		for _, id := range ro.Admitted {
			ro.in[id] = true
		}
		f.rollouts[name] = ro
	}
	return nil
}

func (s *Server) rolloutRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/fleet/rollouts", s.viewer(func(w http.ResponseWriter, _ *http.Request) {
		now := time.Now()
		s.rolloutTick(now)
		f := &s.fleet
		f.mu.Lock()
		out := []RolloutInfo{}
		for _, ro := range f.rollouts {
			v := s.look(ro, now)
			info := RolloutInfo{rollout: ro, StepNumber: ro.Stage + 1, StepCount: len(ro.Steps) + 1, Affected: len(v.affected),
				Applied: v.applied, Agents: v.agents}
			if ro.Stage < len(ro.Steps) && len(v.affected) > 0 {
				info.Target = ro.Steps[ro.Stage].Target(len(v.affected))
			}
			if !ro.HealthySince.IsZero() && ro.Status == rolloutRunning {
				if ro.Wait == 0 {
					info.Ready = true
				} else {
					at := ro.HealthySince.Add(ro.wait())
					info.NextAt = &at
				}
			}
			if info.Agents == nil {
				info.Agents = []RolloutAgent{}
			}
			if len(info.Agents) > 100 {
				info.Agents = info.Agents[:100]
			}
			out = append(out, info)
		}
		slices.SortFunc(out, func(a, b RolloutInfo) int { return cmp.Compare(a.Profile, b.Profile) })
		// Marshal under the lock: the rollouts are live objects.
		body, _ := json.Marshal(out)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	mux.HandleFunc("POST /api/v1/fleet/rollouts/{profile}/{action}", s.admin(s.rolloutAction))
}

// rolloutAction is what a person does to a rollout: promote it to the next
// step, complete it (everyone now), pause, resume, or abort it (everyone
// back to the version from before).
func (s *Server) rolloutAction(w http.ResponseWriter, r *http.Request) {
	name, action, now := r.PathValue("profile"), r.PathValue("action"), time.Now()
	s.rolloutMu.Lock()
	defer s.rolloutMu.Unlock()
	f := &s.fleet
	f.mu.Lock()
	ro := f.rollouts[name]
	if ro == nil {
		f.mu.Unlock()
		http.Error(w, "no rollout is running for this profile", http.StatusNotFound)
		return
	}
	done, abort := false, false
	switch action {
	case "promote":
		ro.Stage++
		ro.Status, ro.Reason, ro.HealthySince = rolloutRunning, "", time.Time{}
		done = ro.Stage >= len(ro.Steps)
	case "complete":
		done = true
	case "pause":
		ro.Status, ro.Reason, ro.HealthySince = rolloutPaused, "paused by "+author(r), time.Time{}
	case "resume":
		// Agents that went silent leave the rollout; they are back on the
		// earlier version when they return, and others take their place.
		for _, a := range s.look(ro, now).agents {
			if a.State == "silent" {
				ro.drop(a.ID)
			}
		}
		ro.Status, ro.Reason = rolloutRunning, ""
	case "abort":
		abort = true
	default:
		f.mu.Unlock()
		http.Error(w, "the action is promote, complete, pause, resume or abort", http.StatusBadRequest)
		return
	}
	from, stableDoc := ro.From, ro.stableDoc
	origin := f.rows[name].Origin
	var doc []byte
	if done || abort {
		// Aborting: the agents fall back to the earlier version at once;
		// the profile itself is put back below.
		if abort {
			for id := range ro.in {
				ro.drop(id)
			}
		} else {
			f.endRollout(name)
		}
	} else {
		doc, _ = json.Marshal(ro)
	}
	f.mu.Unlock()
	s.log.Info("rollout "+action, "profile", name, "by", author(r))

	var err error
	switch {
	case abort && from == 0:
		err = s.store.DeleteProfile(name, author(r))
	case abort:
		_, err = s.store.PutProfile(name, stableDoc, origin, author(r))
	}
	if err == nil && (done || abort) {
		err = s.store.DeleteRollout(name)
	} else if err == nil {
		err = s.store.SetRollout(name, string(doc))
	}
	if abort && err == nil {
		// The stored profile is the earlier one again; only now may the
		// rollout be forgotten, or agents would briefly get the new one.
		if err = s.reloadProfiles(); err == nil {
			f.mu.Lock()
			f.endRollout(name)
			f.mu.Unlock()
			err = s.syncIntegrations()
		}
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.stepRollouts(now)
	w.WriteHeader(http.StatusNoContent)
}
