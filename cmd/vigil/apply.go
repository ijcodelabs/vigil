package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ijcodelabs/vigil/internal/fleet"
)

// runApply makes the server's profiles match a directory of profile files,
// so they can be kept in git and applied from CI:
//
//	vigil apply -f fleet/ -server https://vigil.example.com
//
// A file holds one profile; its name is the `name:` field, or the file
// name. Files in an `integrations` directory directly under DIR are
// integrations and are saved first, since profiles may use them. Unchanged profiles are left alone. With -prune, profiles that were
// applied from files before and are no longer in the directory are deleted;
// profiles made on the server itself are never touched.
//
// Files in a `queries` directory directly under DIR hold saved queries for
// Explore, shared with everyone; see applyQueries.
func runApply(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	dir := fs.String("f", "", "directory of profile files (*.yaml, *.yml)")
	server := fs.String("server", os.Getenv("VIGIL_SERVER"), "server address (or $VIGIL_SERVER)")
	token := fs.String("token", os.Getenv("VIGIL_ADMIN_TOKEN"), "admin token (or $VIGIL_ADMIN_TOKEN)")
	prune := fs.Bool("prune", false, "delete file-managed profiles that are no longer in the directory")
	dry := fs.Bool("dry-run", false, "show what would change without changing it")
	immediate := fs.Bool("immediate", false, "give changes to every agent at once, also for profiles with a rollout section")
	caPin := fs.String("ca-pin", os.Getenv("VIGIL_CA_PIN"), "fingerprint of the server's certificate authority (or $VIGIL_CA_PIN)")
	caFile := fs.String("ca-file", "", "certificate file to trust instead of the system's authorities")
	fs.Parse(args)
	if *dir == "" || *server == "" || *token == "" {
		return errors.New("apply needs -f DIR, and -server and -token (or VIGIL_SERVER and VIGIL_ADMIN_TOKEN)")
	}
	api := strings.TrimRight(*server, "/") + "/api/v1/fleet"
	base := api + "/profiles"
	hc, err := trustedClient(*server, *caFile, *caPin)
	if err != nil {
		return err
	}
	call := func(method, u, body string) (int, string, error) {
		req, err := http.NewRequest(method, u, strings.NewReader(body))
		if err != nil {
			return 0, "", err
		}
		req.Header.Set("Authorization", "Bearer "+*token)
		req.Header.Set("X-Vigil-Origin", "files")
		resp, err := hc.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(b)), nil
	}

	failed := 0
	intDir, queryDir := filepath.Join(*dir, "integrations"), filepath.Join(*dir, "queries")
	if entries, err := os.ReadDir(intDir); err == nil {
		for _, e := range entries {
			name, isYAML := strings.CutSuffix(e.Name(), ".yaml")
			if !isYAML || e.IsDir() {
				continue
			}
			text, err := os.ReadFile(filepath.Join(intDir, e.Name()))
			if err != nil {
				return err
			}
			if _, err := fleet.ParseIntegration(name, string(text)); err != nil {
				return fmt.Errorf("%s: %w", filepath.Join(intDir, e.Name()), err)
			}
			u := api + "/integrations/" + url.PathEscape(name)
			if code, body, _ := call("GET", u, ""); code == http.StatusOK {
				var cur struct{ Doc string }
				if json.Unmarshal([]byte(body), &cur) == nil && cur.Doc == string(text) {
					fmt.Printf("  unchanged  integration %s\n", name)
					continue
				}
			}
			if *dry {
				fmt.Printf("  would save integration %s\n", name)
				continue
			}
			code, body, err := call("PUT", u, string(text))
			switch {
			case err != nil:
				return err
			case code == http.StatusNoContent:
				fmt.Printf("  saved      integration %s\n", name)
			default:
				fmt.Printf("  REFUSED    integration %s: %s\n", name, body)
				failed++
			}
		}
	}

	// Read and check every file before touching the server.
	type local struct {
		name, doc string
		priority  int
	}
	var want []local
	seen := map[string]string{}
	err = filepath.WalkDir(*dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() && (path == intDir || path == queryDir) {
			return filepath.SkipDir
		}
		if err != nil || d.IsDir() || (!strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yml")) {
			return err
		}
		text, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(path), ".yaml"), ".yml")
		p, perr := fleet.ParseProfile(name, string(text))
		if perr != nil && strings.Contains(perr.Error(), "it is being saved as") {
			// The file names the profile itself; that name wins over the file name.
			var head struct{ Name string }
			yamlName(text, &head.Name)
			name = head.Name
			p, perr = fleet.ParseProfile(name, string(text))
		}
		if perr != nil {
			return fmt.Errorf("%s: %w", path, perr)
		}
		if other, dup := seen[p.Name]; dup {
			return fmt.Errorf("%s and %s both define profile %s", other, path, p.Name)
		}
		seen[p.Name] = path
		want = append(want, local{p.Name, string(text), p.Priority})
		return nil
	})
	if err != nil {
		return err
	}
	// Broad profiles first: a narrower one may build on what they define.
	sort.Slice(want, func(i, j int) bool {
		if want[i].priority != want[j].priority {
			return want[i].priority < want[j].priority
		}
		return want[i].name < want[j].name
	})

	code, body, err := call("GET", base, "")
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("listing profiles: HTTP %d: %s", code, body)
	}
	var have []struct{ Name, Origin string }
	if err := json.Unmarshal([]byte(body), &have); err != nil {
		return err
	}

	pending := want
	for pass := 0; len(pending) > 0; pass++ {
		var retry []local
		progress := false
		for _, l := range pending {
			u := base + "/" + url.PathEscape(l.name)
			if code, body, _ := call("GET", u, ""); code == http.StatusOK {
				var cur struct{ Doc string }
				if json.Unmarshal([]byte(body), &cur) == nil && cur.Doc == l.doc {
					fmt.Printf("  unchanged  %s\n", l.name)
					progress = true
					continue
				}
			}
			if *dry {
				fmt.Printf("  would save %s\n", l.name)
				progress = true
				continue
			}
			put := u
			if *immediate {
				put += "?rollout=skip"
			}
			code, body, err := call("PUT", put, l.doc)
			switch {
			case err != nil:
				return err
			case code == http.StatusOK:
				if strings.Contains(body, `"rollout":true`) {
					fmt.Printf("  saved      %s (rolling out in steps; follow it on the Fleet page)\n", l.name)
				} else {
					fmt.Printf("  saved      %s\n", l.name)
				}
				progress = true
			default:
				retry = append(retry, l)
				if pass > 0 || len(pending) == 1 {
					fmt.Printf("  REFUSED    %s: %s\n", l.name, body)
				}
			}
		}
		// A profile may only become valid once another one is in; try the
		// refused ones again as long as something else got through.
		if !progress || pass > 0 {
			failed += len(retry)
			if pass == 0 {
				for _, l := range retry {
					_, body, _ := call("PUT", base+"/"+url.PathEscape(l.name), l.doc)
					fmt.Printf("  REFUSED    %s: %s\n", l.name, body)
				}
			}
			break
		}
		pending = retry
	}

	if *prune {
		for _, h := range have {
			if _, kept := seen[h.Name]; kept || h.Origin != "files" {
				continue
			}
			if *dry {
				fmt.Printf("  would delete %s\n", h.Name)
				continue
			}
			if code, body, _ := call("DELETE", base+"/"+url.PathEscape(h.Name), ""); code != http.StatusNoContent {
				fmt.Printf("  REFUSED    delete %s: %s\n", h.Name, body)
				failed++
			} else {
				fmt.Printf("  deleted    %s\n", h.Name)
			}
		}
	}
	n, err := applyQueries(call, strings.TrimRight(*server, "/")+"/api/v1/queries", queryDir, *dry, *prune)
	if err != nil {
		return err
	}
	if failed += n; failed > 0 {
		return fmt.Errorf("%d change(s) were refused; the server keeps what it had for those", failed)
	}
	return nil
}

// applyQueries makes the server's shared saved queries match the files in
// DIR/queries. A file holds a list under `queries:`; a query is known by
// its name within its type (metrics or logs), across all the files. With
// prune, shared queries that came from files before and are in none now
// are deleted. Personal queries, and shared ones made in the interface,
// are never deleted. Without the directory nothing is done at all, so a
// repository that keeps only profiles in git is left alone.
func applyQueries(call func(method, u, body string) (int, string, error), api, dir string, dry, prune bool) (failed int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, nil
	}
	want := map[string]fleet.Query{}
	from := map[string]string{}
	var order []string
	for _, e := range entries {
		if e.IsDir() || (!strings.HasSuffix(e.Name(), ".yaml") && !strings.HasSuffix(e.Name(), ".yml")) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		text, err := os.ReadFile(path)
		if err != nil {
			return 0, err
		}
		list, err := fleet.ParseQueries(string(text))
		if err != nil {
			return 0, fmt.Errorf("%s: %w", path, err)
		}
		for _, q := range list {
			if other, dup := from[q.Key()]; dup {
				return 0, fmt.Errorf("%s and %s both define the %s query %q", other, path, q.Type, q.Name)
			}
			from[q.Key()], want[q.Key()] = path, q
			order = append(order, q.Key())
		}
	}
	code, body, err := call("GET", api, "")
	if err != nil {
		return 0, err
	}
	if code != http.StatusOK {
		return 0, fmt.Errorf("listing saved queries: HTTP %d: %s", code, body)
	}
	var have []struct {
		fleet.Query
		ID, Origin string
		Shared     bool
	}
	if err := json.Unmarshal([]byte(body), &have); err != nil {
		return 0, err
	}
	shared := map[string]int{} // key -> index in have
	for i, h := range have {
		if h.Shared && h.ID != "" {
			shared[h.Key()] = i
		}
	}
	for _, key := range order {
		q := want[key]
		label := fmt.Sprintf("query %s (%s)", q.Name, q.Type)
		method, u := "POST", api
		if i, ok := shared[key]; ok {
			if have[i].Query.Query == q.Query {
				fmt.Printf("  unchanged  %s\n", label)
				continue
			}
			method, u = "PUT", api+"/"+url.PathEscape(have[i].ID)
		}
		if dry {
			fmt.Printf("  would save %s\n", label)
			continue
		}
		doc, _ := json.Marshal(map[string]any{"name": q.Name, "type": q.Type, "query": q.Query, "shared": true})
		code, body, err := call(method, u, string(doc))
		switch {
		case err != nil:
			return failed, err
		case code == http.StatusOK:
			fmt.Printf("  saved      %s\n", label)
		default:
			fmt.Printf("  REFUSED    %s: %s\n", label, body)
			failed++
		}
	}
	if !prune {
		return failed, nil
	}
	for _, h := range have {
		if _, kept := want[h.Key()]; kept || !h.Shared || h.ID == "" || h.Origin != "files" {
			continue
		}
		label := fmt.Sprintf("query %s (%s)", h.Name, h.Type)
		if dry {
			fmt.Printf("  would delete %s\n", label)
			continue
		}
		if code, body, _ := call("DELETE", api+"/"+url.PathEscape(h.ID), ""); code != http.StatusNoContent {
			fmt.Printf("  REFUSED    delete %s: %s\n", label, body)
			failed++
		} else {
			fmt.Printf("  deleted    %s\n", label)
		}
	}
	return failed, nil
}

// yamlName pulls the top-level name: field out of a profile document.
func yamlName(text []byte, out *string) {
	for _, line := range strings.Split(string(text), "\n") {
		if v, ok := strings.CutPrefix(line, "name:"); ok {
			*out = strings.Trim(strings.TrimSpace(v), `"'`)
			return
		}
	}
}
