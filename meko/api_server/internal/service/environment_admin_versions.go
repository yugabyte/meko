package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

// ErrControllerNotDeclared is returned when a fleet-version is declared on an
// environment with no controller declared (mapped to 409). The agent rolls the fleet
// only once the controller is Ready at a declared version, which an undeclared
// controller never is, so the roll would wait for ever while the agent reports READY.
var ErrControllerNotDeclared = errors.New("fleet-version needs a declared controller: " +
	"PUT the controller version first (the version it already runs is enough)")

// ControllerVersion is the body of PUT /admin/environments/:id/desired/controller, and
// the stored spec: the agent's controllerSpec. The omitempty tags keep a bare version
// encoded as {"version":"X"}, so re-declaring one keeps its generation.
type ControllerVersion struct { //nolint:govet // field order is the spec's documented key order
	Version      string                     `json:"version"`
	ChartVersion string                     `json:"chart_version,omitempty"`
	Values       map[string]json.RawMessage `json:"values,omitempty"`
}

// FleetVersion is the body of PUT /admin/environments/:id/desired/fleet-version, and
// the stored spec: the version the agent rolls every endpoint onto.
type FleetVersion struct {
	Version string `json:"version"`
}

// AdminDesired is an admin-only row as stored. Not models.DesiredResource, whose
// omitempty would drop the generation of a first write.
type AdminDesired struct {
	Kind       string          `json:"kind"`
	Name       string          `json:"name"`
	Spec       json.RawMessage `json:"spec"`
	Generation int64           `json:"generation"`
}

// AdminVersion is what an admin GET answers: the declaration, nil when there is
// none, and the agent's last report of the row, nil when it has made none. Observed
// without Desired is normal, since the agent reports its controller on every tick.
type AdminVersion struct {
	Desired  *AdminDesired   `json:"desired"`
	Observed *ResourceStatus `json:"observed"`
}

// SetControllerVersion declares the version an amp environment's controller release
// runs. Re-declaring the same spec keeps the generation; a change bumps it. found=false
// ⇒ unknown environment, or one the scope does not own.
//
// Only presence and padding are checked. Whether a version or chart exists, and which
// values keys mean anything, is for the agent to find out against its registry.
func (s *EnvironmentService) SetControllerVersion(
	envID string, scope AccountScope, actor string, v ControllerVersion,
) (*AdminDesired, bool, error) {
	if err := checkVersion("version", v.Version, true); err != nil {
		return nil, false, err
	}
	if err := checkVersion("chart_version", v.ChartVersion, false); err != nil {
		return nil, false, err
	}
	e, err := s.adminTarget(envID, scope, repository.KindController)
	if err != nil || e == nil {
		return nil, e != nil, err
	}
	return s.putAdminDesired(e, actor, repository.KindController, repository.ControllerName, v.Version, v)
}

// SetFleetVersion declares the version an amp environment's endpoints are rolled
// onto, and refuses with ErrControllerNotDeclared until a controller is declared.
func (s *EnvironmentService) SetFleetVersion(
	envID string, scope AccountScope, actor string, v FleetVersion,
) (*AdminDesired, bool, error) {
	if err := checkVersion("version", v.Version, true); err != nil {
		return nil, false, err
	}
	e, err := s.adminTarget(envID, scope, repository.KindFleetVersion)
	if err != nil || e == nil {
		return nil, e != nil, err
	}
	// Check-then-write without a lock is safe because nothing deletes a controller row.
	ctl, err := s.repo.GetDesired(e.UUID, repository.KindController, repository.ControllerName)
	if err != nil {
		return nil, true, err
	}
	if ctl == nil {
		return nil, true, ErrControllerNotDeclared
	}
	return s.putAdminDesired(e, actor, repository.KindFleetVersion, repository.FleetName, v.Version, v)
}

// ControllerVersionStatus reports an amp environment's controller declaration and
// the agent's last report of it.
func (s *EnvironmentService) ControllerVersionStatus(envID string, scope AccountScope) (*AdminVersion, bool, error) {
	return s.adminVersionStatus(envID, scope, repository.KindController, repository.ControllerName)
}

// FleetVersionStatus reports an amp environment's fleet-version declaration and the
// agent's last report of it.
func (s *EnvironmentService) FleetVersionStatus(envID string, scope AccountScope) (*AdminVersion, bool, error) {
	return s.adminVersionStatus(envID, scope, repository.KindFleetVersion, repository.FleetName)
}

func (s *EnvironmentService) adminVersionStatus(
	envID string, scope AccountScope, kind, name string,
) (*AdminVersion, bool, error) {
	e, err := s.adminTarget(envID, scope, kind)
	if err != nil || e == nil {
		return nil, e != nil, err
	}
	d, err := s.repo.GetDesired(e.UUID, kind, name)
	if err != nil {
		return nil, true, err
	}
	out := &AdminVersion{}
	if d != nil {
		out.Desired = adminDesired(*d)
	}
	if len(bytes.TrimSpace(e.Resources)) > 0 {
		var obs []ResourceStatus
		if err := json.Unmarshal(e.Resources, &obs); err != nil {
			return nil, true, fmt.Errorf("decode observed resources: %w", err)
		}
		if i := slices.IndexFunc(obs, func(o ResourceStatus) bool { return o.ID == kind+"/"+name }); i >= 0 {
			out.Observed = &obs[i]
		}
	}
	return out, true, nil
}

// adminTarget resolves the environment an admin route names, (nil, nil) when there is
// none, and refuses one whose agent does not serve kind.
func (s *EnvironmentService) adminTarget(envID string, scope AccountScope, kind string) (*models.Environment, error) {
	e, err := s.Get(envID, scope)
	if err != nil || e == nil {
		return nil, err
	}
	if !slices.Contains(resourceKinds[e.Kind], kind) {
		return e, fmt.Errorf("%w: a %s environment has no %s", ErrValidation, e.Kind, kind)
	}
	return e, nil
}

func (s *EnvironmentService) putAdminDesired(
	e *models.Environment, actor, kind, name, version string, spec any,
) (*AdminDesired, bool, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, true, fmt.Errorf("encode %s spec: %w", kind, err)
	}
	d, err := s.repo.UpsertAdminDesired(e.UUID, models.DesiredResource{Kind: kind, Name: name, Spec: raw})
	if err != nil {
		return nil, true, err
	}
	attrs := []any{"env_uuid", e.UUID, "kind", kind, "version", version, "generation", d.Generation}
	if actor != "" {
		attrs = append(attrs, "actor", actor)
	}
	slog.Info("admin declared a software version", attrs...)
	return adminDesired(d), true, nil
}

func adminDesired(d models.DesiredResource) *AdminDesired {
	return &AdminDesired{Kind: d.Kind, Name: d.Name, Spec: d.Spec, Generation: d.Generation}
}

// checkVersion refuses an absent required version, which the agent latches as a
// permanent rejection, and a padded one, which it trims — leaving a declaration that
// never reads the same as the version the agent reports.
func checkVersion(field, v string, required bool) error {
	if required && strings.TrimSpace(v) == "" {
		return fmt.Errorf("%w: %s is required", ErrValidation, field)
	}
	if strings.TrimSpace(v) != v {
		return fmt.Errorf("%w: %s must not carry surrounding whitespace", ErrValidation, field)
	}
	return nil
}
