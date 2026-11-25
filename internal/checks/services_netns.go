package checks

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/nesolva/localci/internal/workflow"
)

// A job with services runs them and the job container in one network namespace (the job container owns it, the
// services join it), so `localhost:<port>` and `<service>:<port>` both reach a service, as on GitHub's runners,
// and nothing is published on the host. `ports:` therefore only declares the container port a service listens on.

// portSpec is one `ports:` entry of a service, reduced to what the job needs to know.
type portSpec struct {
	container string // "5432/tcp", the port inside the service
	remapped  string // "5433:5432" as written when the host port differs, else ""
}

// parsePort reads a Docker port entry: `5432`, `5432:5432`, `127.0.0.1:5433:5432`, `5432/udp`. Entries it cannot
// read (ranges, odd forms) are ignored: the service's own log says what is wrong with them.
func parsePort(entry string) (portSpec, bool) {
	entry = strings.TrimSpace(entry)
	proto := "tcp"
	if i := strings.LastIndex(entry, "/"); i >= 0 {
		entry, proto = entry[:i], strings.ToLower(entry[i+1:])
	}
	parts := strings.Split(entry, ":")
	last := parts[len(parts)-1]
	if _, err := strconv.Atoi(last); err != nil {
		return portSpec{}, false
	}
	p := portSpec{container: last + "/" + proto}
	if len(parts) >= 2 {
		host := parts[len(parts)-2]
		if n, err := strconv.Atoi(host); err == nil && host != last && n > 0 {
			p.remapped = host + ":" + last
		}
	}
	return p, true
}

// servicePlan checks the services' declared ports before anything starts. Two services declaring the same container
// port is an error naming both (they would fight for it in the shared namespace); a remap is not honoured and
// comes back as a note saying which port the job must use.
func servicePlan(services []workflow.Service) (notes []string, err error) {
	owner := map[string]string{} // container port -> service that declared it
	for _, svc := range services {
		seen := map[string]bool{}
		for _, entry := range svc.Ports {
			p, ok := parsePort(entry)
			if !ok || seen[p.container] {
				continue
			}
			seen[p.container] = true
			if other, taken := owner[p.container]; taken {
				port, _, _ := strings.Cut(p.container, "/")
				return nil, fmt.Errorf("services %s and %s both declare port %s: the services of a job share one network, so two cannot listen on it", other, svc.Name, port)
			}
			owner[p.container] = svc.Name
			if p.remapped != "" {
				port, _, _ := strings.Cut(p.container, "/")
				notes = append(notes, fmt.Sprintf("note: service %s: ports %s is not published; the job reaches it at localhost:%s (or %s:%s)",
					svc.Name, p.remapped, port, svc.Name, port))
			}
		}
	}
	return notes, nil
}

func serviceNames(services []workflow.Service) []string {
	names := make([]string, len(services))
	for i, s := range services {
		names[i] = s.Name
	}
	return names
}
