package archer

import (
	"encoding/json"
	"fmt"
	"github.com/Parallax-Web-Services/archer/internal/catalog"
	"github.com/Parallax-Web-Services/archer/internal/store"
	"regexp"
	"strings"
)

type Requirement struct {
	Capability string `json:"capability"`
	Delivery   string `json:"delivery,omitempty"`
	Constraint string `json:"constraint,omitempty"`
}

func (r *Requirement) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &r.Capability)
	}
	type plain Requirement
	return json.Unmarshal(b, (*plain)(r))
}

type Probe struct {
	Argv     []string `json:"argv"`
	Contains string   `json:"contains,omitempty"`
}
type LocalCapability struct {
	ID       string   `json:"id"`
	Provides []string `json:"provides"`
	Probe    Probe    `json:"probe"`
}
type Recipe struct {
	ID        string         `json:"id"`
	Provides  []string       `json:"provides"`
	Filename  string         `json:"filename"`
	Platforms []string       `json:"platforms,omitempty"`
	Artifact  store.Artifact `json:"artifact"`
}
type Project struct {
	Schema       string            `json:"schema"`
	Scope        string            `json:"scope"`
	Requirements []Requirement     `json:"requirements"`
	Services     []catalog.Service `json:"services,omitempty"`
	Local        []LocalCapability `json:"local,omitempty"`
	Registries   []string          `json:"registries,omitempty"`
	Recipes      []Recipe          `json:"recipes,omitempty"`
}
type Decision struct {
	Requirement Requirement `json:"requirement"`
	Status      string      `json:"status"`
	Provider    string      `json:"provider,omitempty"`
	Reason      string      `json:"reason"`
	Evidence    string      `json:"evidence,omitempty"`
	Path        string      `json:"path,omitempty"`
	Cached      bool        `json:"cached,omitempty"`
	AIEligible  bool        `json:"ai_eligible,omitempty"`
	Discovery   string      `json:"discovery"`
}
type Plan struct {
	Schema     string             `json:"schema"`
	ID         string             `json:"id"`
	Scope      string             `json:"scope"`
	Platform   string             `json:"platform"`
	Decisions  []Decision         `json:"decisions"`
	Catalogs   []catalog.Snapshot `json:"catalogs,omitempty"`
	Remaining  []Requirement      `json:"remaining"`
	AIRequests int                `json:"ai_requests"`
}
type Context struct {
	Schema   string            `json:"schema"`
	Scope    string            `json:"scope"`
	Revision int               `json:"revision"`
	Bindings map[string]string `json:"bindings"`
	Outcomes []Outcome         `json:"outcomes"`
}
type Outcome struct {
	Capability string `json:"capability"`
	Provider   string `json:"provider"`
	Status     string `json:"status"`
	At         string `json:"at"`
}

var slug = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+\-]*$`)
var filename = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._\-]*$`)

func (p Project) Validate() error {
	if p.Schema != "archer.project/v1" {
		return fmt.Errorf("unsupported project schema %q", p.Schema)
	}
	if strings.TrimSpace(p.Scope) == "" {
		return fmt.Errorf("project scope is required")
	}
	for _, r := range p.Requirements {
		if !slug.MatchString(r.Capability) {
			return fmt.Errorf("invalid capability %q; use an explicit capability slug", r.Capability)
		}
		if r.Delivery != "" && r.Delivery != "service" && r.Delivery != "local" && r.Delivery != "package" {
			return fmt.Errorf("invalid delivery %q", r.Delivery)
		}
	}
	ids := map[string]bool{}
	for _, s := range p.Services {
		if !slug.MatchString(s.ID) || ids["service:"+s.ID] {
			return fmt.Errorf("invalid or duplicate service ID %q", s.ID)
		}
		ids["service:"+s.ID] = true
	}
	for _, l := range p.Local {
		if !slug.MatchString(l.ID) || len(l.Probe.Argv) == 0 {
			return fmt.Errorf("local capability %q needs an ID and an explicit read-only probe", l.ID)
		}
	}
	for _, r := range p.Recipes {
		if !slug.MatchString(r.ID) || ids["recipe:"+r.ID] {
			return fmt.Errorf("invalid or duplicate recipe ID %q", r.ID)
		}
		ids["recipe:"+r.ID] = true
		if !filename.MatchString(r.Filename) || r.Filename == "." || r.Filename == ".." {
			return fmt.Errorf("unsafe artifact filename %q", r.Filename)
		}
	}
	return nil
}
