package suite

import (
	"fmt"
	"regexp"
	"strings"
)

// Commands and URLs of a suite may use these placeholders, resolved per shard:
//
//	{{port "name"}}   a free port, the same for the same name within the shard
//	{{db.url}}        where the shard's database is reached
//	{{db.name}}       the shard's database name
//	{{shard}}         the shard number (0 for the serial group)
//	{{run}}           the run id
//	{{dir "repo"}}    where the repo is checked out
var placeholder = regexp.MustCompile(`\{\{\s*([^{}]*?)\s*\}\}`)

// env is everything a placeholder can ask for; a nil function means "not available here".
type env struct {
	shard  int
	run    string
	dbName string
	dbURL  string
	port   func(name string) (int, error)
	dir    func(repo string) (string, error)
}

// expand resolves the placeholders of s. {{template}} and {{name}} belong to the database driver and are
// left as they are.
func (e env) expand(s string) (string, error) {
	var firstErr error
	out := placeholder.ReplaceAllStringFunc(s, func(m string) string {
		tok := placeholder.FindStringSubmatch(m)[1]
		v, keep, err := e.resolve(tok)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if keep {
			return m
		}
		return v
	})
	return out, firstErr
}

func (e env) resolve(tok string) (val string, keep bool, err error) {
	name, arg, _ := strings.Cut(tok, " ")
	arg = strings.Trim(strings.TrimSpace(arg), `"`)
	switch name {
	case "template", "name":
		return "", true, nil
	case "shard":
		return fmt.Sprint(e.shard), false, nil
	case "run":
		return e.run, false, nil
	case "db.url":
		if e.dbURL == "" {
			return "", false, fmt.Errorf("{{db.url}} is used but the suite has no database")
		}
		return e.dbURL, false, nil
	case "db.name":
		if e.dbName == "" {
			return "", false, fmt.Errorf("{{db.name}} is used but the suite has no database")
		}
		return e.dbName, false, nil
	case "port":
		if arg == "" {
			return "", false, fmt.Errorf(`{{port}} needs a name: {{port "api"}}`)
		}
		p, err := e.port(arg)
		return fmt.Sprint(p), false, err
	case "dir":
		if arg == "" {
			return "", false, fmt.Errorf(`{{dir}} needs a repository: {{dir "acme/api"}}`)
		}
		d, err := e.dir(arg)
		return d, false, err
	}
	return "", false, fmt.Errorf("unknown placeholder {{%s}}", tok)
}

// checkTemplates expands every templated string once with stand-in values, so a typo fails when the
// configuration is read and not in the middle of a run.
func (s *Suite) checkTemplates() error {
	dry := env{shard: 1, run: "0", dbName: "db", dbURL: "db",
		port: func(string) (int, error) { return 1, nil },
		dir: func(repo string) (string, error) {
			if !s.hasRepo(repo) {
				return "", fmt.Errorf("{{dir %q}}: repository is not in repos", repo)
			}
			return "/", nil
		},
	}
	check := func(where, v string) error {
		if _, err := dry.expand(v); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		return nil
	}
	for _, sv := range s.Stack {
		for where, v := range map[string]string{"stack " + sv.Name + ".run": sv.Run, "stack " + sv.Name + ".ready.http": sv.Ready.HTTP,
			"stack " + sv.Name + ".ready.tcp": sv.Ready.TCP} {
			if err := check(where, v); err != nil {
				return err
			}
		}
		for k, v := range sv.Env {
			if err := check("stack "+sv.Name+".env."+k, v); err != nil {
				return err
			}
		}
	}
	if err := check("test.run", s.Test.Run); err != nil {
		return err
	}
	for k, v := range s.Test.Env {
		if err := check("test.env."+k, v); err != nil {
			return err
		}
	}
	if d := s.Database; d != nil {
		if err := check("database.url", d.URL); err != nil {
			return err
		}
		if err := check("database.template.seed", d.Template.Seed); err != nil {
			return err
		}
	}
	return nil
}
