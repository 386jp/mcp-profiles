package config

import (
	"errors"
	"fmt"
	"strings"
)

// expand resolves ${VAR} and ${VAR:-default} in the values that may carry secrets or paths.
func (c *Config) expand(lookupEnv func(string) (string, bool)) error {
	var errs []error
	str := func(where string, s *string) {
		v, err := expandString(*s, lookupEnv)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", where, err))
			return
		}
		*s = v
	}
	strMap := func(where string, m map[string]string) {
		for k, v := range m {
			str(where+"."+k, &v)
			m[k] = v
		}
	}

	strMap("listen.headers", c.Listen.Headers)
	for name, p := range c.Profiles {
		prefix := "profiles." + name
		str(prefix+".url", &p.URL)
		str(prefix+".command", &p.Command)
		for i := range p.Args {
			str(fmt.Sprintf("%s.args[%d]", prefix, i), &p.Args[i])
		}
		strMap(prefix+".headers", p.Headers)
		strMap(prefix+".env", p.Env)
		c.Profiles[name] = p
	}
	return errors.Join(errs...)
}

// expandString replaces ${VAR} and ${VAR:-default}. An unset VAR without a default is an error.
func expandString(s string, lookupEnv func(string) (string, bool)) (string, error) {
	var b strings.Builder
	for {
		start := strings.Index(s, "${")
		if start < 0 {
			b.WriteString(s)
			return b.String(), nil
		}
		end := strings.Index(s[start:], "}")
		if end < 0 {
			return "", fmt.Errorf("unterminated ${ in %q", s)
		}
		end += start
		b.WriteString(s[:start])

		expr := s[start+2 : end]
		name, def, hasDef := strings.Cut(expr, ":-")
		if name == "" {
			return "", fmt.Errorf("empty variable name in %q", s)
		}
		if v, ok := lookupEnv(name); ok {
			b.WriteString(v)
		} else if hasDef {
			b.WriteString(def)
		} else {
			return "", fmt.Errorf("environment variable %s is not set", name)
		}
		s = s[end+1:]
	}
}
