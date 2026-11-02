package config

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// A config from the server may carry settings that a newer server knows
// and this agent does not. Refusing all of it would leave the agent on its
// old config, taking no change at all until it is upgraded; so a setting it
// does not know is skipped and the rest applied. Not under the keys below:
// there a new setting may replace an old one (tls.from_server for
// cert_file), and skipping it could leave the agent less protected than
// the config it runs now, so such a config is refused as before.
var guardedKey = regexp.MustCompile(`tls|token|auth|identity|cert|key|redact|drop|secret|password`)

var unknownField = regexp.MustCompile(`^line (\d+): field (\S+) not found in type .+$`)

// ParseCollectionSkipping is ParseCollection, except that settings this
// agent does not know are skipped (and returned, as dotted paths) instead
// of refusing the whole config, unless one of them is guarded.
func ParseCollectionSkipping(text string) (*Collection, []string, error) {
	c, err := ParseCollection(text)
	var te *yaml.TypeError
	if err == nil || !errors.As(err, &te) {
		return c, nil, err
	}
	var root yaml.Node
	if yaml.Unmarshal([]byte(text), &root) != nil {
		return nil, nil, err
	}
	var skipped []string
	for _, msg := range te.Errors {
		m := unknownField.FindStringSubmatch(msg)
		if m == nil {
			return nil, nil, err // another kind of mistake: refused
		}
		line, _ := strconv.Atoi(m[1])
		p, val := keyPath(&root, line, m[2], nil)
		if p == "" {
			p = m[2]
		}
		if guardedKey.MatchString(strings.ToLower(p)) || guardedInside(val) {
			return nil, nil, fmt.Errorf("%w (%s is a setting this agent does not know, in a part it does not skip: upgrade the agent)", err, p)
		}
		skipped = append(skipped, p)
	}
	c = &Collection{}
	if err := yaml.NewDecoder(strings.NewReader(text)).Decode(c); err != nil && err != io.EOF {
		return nil, nil, err
	}
	if c.Interval != 0 && c.Interval.Std() < 1e9 {
		return nil, nil, fmt.Errorf("interval must be at least 1s")
	}
	c.defaults()
	return c, skipped, nil
}

// keyPath finds the mapping key name on line: its dotted path and value.
func keyPath(n *yaml.Node, line int, name string, path []string) (string, *yaml.Node) {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			if p, v := keyPath(c, line, name, path); p != "" {
				return p, v
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			here := append(append([]string(nil), path...), k.Value)
			if k.Line == line && k.Value == name {
				return strings.Join(here, "."), n.Content[i+1]
			}
			if p, v := keyPath(n.Content[i+1], line, name, here); p != "" {
				return p, v
			}
		}
	}
	return "", nil
}

// guardedInside: a skipped block holds a guarded key somewhere inside.
func guardedInside(n *yaml.Node) bool {
	if n == nil {
		return false
	}
	for i, c := range n.Content {
		if n.Kind == yaml.MappingNode && i%2 == 0 && guardedKey.MatchString(strings.ToLower(c.Value)) {
			return true
		}
		if guardedInside(c) {
			return true
		}
	}
	return false
}
