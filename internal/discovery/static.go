package discovery

import (
	"context"
	"regexp"
	"strings"

	"github.com/ijcodelabs/vigil/internal/config"
)

// Static serves the server lists written in the config file.
type Static []config.StaticJob

func (Static) Name() string { return "static" }

func (s Static) Targets(context.Context) ([]Target, error) {
	var out []Target
	for _, j := range s {
		if config.Off(j.Enabled) {
			continue
		}
		// The expressions were checked when the config was accepted.
		var keep, drop []*regexp.Regexp
		for _, e := range j.Keep {
			if re, err := regexp.Compile(e); err == nil {
				keep = append(keep, re)
			}
		}
		for _, e := range j.Drop {
			if re, err := regexp.Compile(e); err == nil {
				drop = append(drop, re)
			}
		}
		scheme, path := j.Scheme, j.Path
		if scheme == "" {
			scheme = "http"
		}
		if path == "" {
			path = "/metrics"
		}
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		for _, t := range j.Targets {
			tg := Target{
				Job:      j.Job,
				Instance: t,
				URL:      scheme + "://" + t + path,
				Labels:   j.Labels,
				Insecure: j.TLSInsecure,
				Interval: j.Interval.Std(), Timeout: j.Timeout.Std(),
				BearerToken: j.BearerToken, BearerTokenFile: j.BearerTokenFile, Keep: keep, Drop: drop,
			}
			if j.BasicAuth != nil {
				tg.Username, tg.Password = j.BasicAuth.Username, j.BasicAuth.Password
			}
			out = append(out, tg)
		}
	}
	return out, nil
}
