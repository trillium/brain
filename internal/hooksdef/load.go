package hooksdef

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// rawDefinition is the TOML shape of a hook file. Field policy strings are
// kept raw so a bad value can be reported by name instead of silently
// defaulted to a different behavior than the author typed.
type rawDefinition struct {
	Name        string `toml:"name"`
	Run         string `toml:"run"`
	When        string `toml:"when"`
	Policy      string `toml:"policy"`
	Timeout     string `toml:"timeout"`
	RefusalExit int    `toml:"refusal_exit"`
}

// HooksDir returns the hook declaration directory for a .beads directory.
// It is the single name both the loader and the docs agree on.
func HooksDir(beadsDir string) string {
	return filepath.Join(beadsDir, DefaultHooksDirName)
}

// LoadAll loads every hook definition in dir, sorted by hook name.
//
// dir that does not exist is an empty set, not an error: a store with no
// hooks declared has no hooks, and that absence is not a failure.
//
// Anything else that could resolve to "no hook" — a directory that cannot be
// read, a file that cannot be parsed, a file that is not .toml — is returned
// as an error alongside whatever loaded. LoadAll refuses rather than
// guessing: a hooks.d containing one good hook and one unreadable one is a
// refusal, not a store running with one hook quietly missing.
func LoadAll(dir string) ([]Definition, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, &LoadError{Path: dir, Problem: fmt.Sprintf("reading hook directory: %v", err)}
	}

	var defs []Definition
	var errs []*LoadError
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(dir, name)
		if entry.IsDir() {
			errs = append(errs, &LoadError{Path: path, Problem: "is a directory; hooks.d holds one flat file per hook"})
			continue
		}
		if !strings.HasSuffix(name, ".toml") {
			errs = append(errs, &LoadError{Path: path, Problem: "not a .toml file; hooks.d holds one flat file per hook and anything else here is a mistake, not a hook"})
			continue
		}
		def, lerr := LoadFile(path)
		if lerr != nil {
			var le *LoadError
			if e, ok := lerr.(*LoadError); ok {
				le = e
			} else {
				le = &LoadError{Path: path, Problem: lerr.Error()}
			}
			errs = append(errs, le)
			continue
		}
		defs = append(defs, def)
	}

	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })

	byName := map[string]string{}
	for _, def := range defs {
		if prev, dup := byName[def.Name]; dup {
			errs = append(errs, &LoadError{Path: def.Path, Hook: def.Name,
				Problem: fmt.Sprintf("duplicate hook name: %q is already defined by %s", def.Name, prev)})
			continue
		}
		byName[def.Name] = def.Path
	}

	var outErr error
	for _, e := range errs {
		if outErr == nil {
			outErr = e
		} else {
			outErr = fmt.Errorf("%w; %s", outErr, e.Error())
		}
	}
	return defs, outErr
}

// LoadFile loads one hook definition. Every problem is a *LoadError naming
// the file and, once known, the hook.
func LoadFile(path string) (Definition, error) {
	def := Definition{Path: path}
	base := filepath.Base(path)
	name := strings.TrimSuffix(base, ".toml")
	if name == "" || name == base {
		return def, &LoadError{Path: path, Problem: "file does not end in .toml"}
	}
	def.Name = name

	raw, err := os.ReadFile(path) //nolint:gosec // operator-owned hooks.d path
	if err != nil {
		return def, &LoadError{Path: path, Hook: name, Problem: fmt.Sprintf("reading file: %v", err)}
	}

	var parsed rawDefinition
	md, err := toml.Decode(string(raw), &parsed)
	if err != nil {
		return def, &LoadError{Path: path, Hook: name, Problem: fmt.Sprintf("parsing TOML: %v", err)}
	}
	// A misspelled key must refuse, not resolve to a default the author never
	// typed ("polcy = \"guard\"" would otherwise leave the hook unset).
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return def, &LoadError{Path: path, Hook: name,
			Problem: fmt.Sprintf("unknown key(s) %s; a hook declares only name, run, when, policy, timeout, refusal_exit", strings.Join(keys, ", "))}
	}

	if parsed.Run == "" {
		return def, &LoadError{Path: path, Hook: name, Problem: "no run command declared; a hook with nothing to run is not a hook and is not silently skipped"}
	}
	if parsed.Name != "" && parsed.Name != name {
		return def, &LoadError{Path: path, Hook: name,
			Problem: fmt.Sprintf("declared name %q does not match the file name; hooks are named by their file, so rename the file or fix the [name] key", parsed.Name)}
	}

	def.Run = parsed.Run
	def.When = parsed.When
	def.Policy = parsed.Policy

	switch def.When {
	case "", EventCreate, EventUpdate, EventClose, EventDelete, EventAll:
		// ok
	default:
		return def, &LoadError{Path: path, Hook: name,
			Problem: fmt.Sprintf("unknown when %q; must be one of create|update|close|delete|*", def.When)}
	}

	switch def.Policy {
	case PolicyUnset, PolicyGuard, PolicyObserver:
		// ok
	default:
		return def, &LoadError{Path: path, Hook: name,
			Problem: fmt.Sprintf("unknown policy %q; must be unset (warns), \"guard\", or \"observer\"", def.Policy)}
	}

	def.Timeout = DefaultTimeout
	if parsed.Timeout != "" {
		d, err := time.ParseDuration(parsed.Timeout)
		if err != nil {
			return def, &LoadError{Path: path, Hook: name,
				Problem: fmt.Sprintf("bad timeout %q: %v", parsed.Timeout, err)}
		}
		if d <= 0 {
			return def, &LoadError{Path: path, Hook: name,
				Problem: fmt.Sprintf("timeout %q must be positive", parsed.Timeout)}
		}
		def.Timeout = d
	}

	def.RefusalExit = DefaultRefusalExit
	if parsed.RefusalExit != 0 {
		if parsed.RefusalExit < 1 || parsed.RefusalExit > 255 {
			return def, &LoadError{Path: path, Hook: name,
				Problem: fmt.Sprintf("refusal_exit %d out of range 1..255 (0 means success)", parsed.RefusalExit)}
		}
		def.RefusalExit = parsed.RefusalExit
	}

	return def, nil
}
