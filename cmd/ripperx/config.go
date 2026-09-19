package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// secretKeys are settings that exist in the file but deliberately have no
// command-line flag: a password or a signing key passed as an argument is
// readable by every user on the machine through /proc. Each is also checked
// against the file's permissions before it is used.
var secretKeys = []string{"smb-password", "auth-password", "jwt-secret"}

var configOnlyKeys = func() map[string]bool {
	m := map[string]bool{}
	for _, k := range secretKeys {
		m[k] = true
	}
	return m
}()

type configFile struct {
	path   string
	values map[string]string
	mode   os.FileMode
}

// loadConfig reads a "key = value" file. Only a line whose first non-blank
// character is # is a comment - values are taken verbatim to the end of the
// line, because a password may well contain a #.
func loadConfig(path string) (*configFile, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	cfg := &configFile{path: path, values: map[string]string{}, mode: fi.Mode().Perm()}

	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected key = value, got %q", path, line, text)
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			return nil, fmt.Errorf("%s:%d: empty setting name", path, line)
		}
		if _, dup := cfg.values[key]; dup {
			return nil, fmt.Errorf("%s:%d: %s is set twice", path, line, key)
		}
		cfg.values[key] = strings.TrimSpace(value)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// apply sets every flag the command line did not, so an explicit argument
// always beats the file. An unknown key is an error: a typo that silently does
// nothing is worse than a refusal to start.
func (c *configFile) apply(fset *flag.FlagSet, explicit map[string]bool) error {
	for key, value := range c.values {
		if configOnlyKeys[key] {
			continue
		}
		if fset.Lookup(key) == nil {
			return fmt.Errorf("%s: unknown setting %q", c.path, key)
		}
		if explicit[key] {
			continue
		}
		if err := fset.Set(key, value); err != nil {
			return fmt.Errorf("%s: %s: %w", c.path, key, err)
		}
	}
	return nil
}

func (c *configFile) get(key string) string { return c.values[key] }

// checkSecret refuses to use credentials out of a file anyone else can read.
// The file is the one place a plaintext password lives, so this is the only
// thing standing between it and every account on the machine.
func (c *configFile) checkSecret(key string) error {
	if c.values[key] == "" {
		return nil
	}
	if c.mode&0o077 != 0 {
		return fmt.Errorf("%s holds %s but is mode %#o, readable by others; "+
			"fix it with: chmod 600 %s", c.path, key, c.mode, c.path)
	}
	return nil
}

// explicitFlags reports which flags were actually given on the command line.
func explicitFlags(fset *flag.FlagSet) map[string]bool {
	set := map[string]bool{}
	fset.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}
