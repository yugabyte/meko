package ycp

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/yugabyte/amp/controller/internal/config"
)

// enrollRetryInterval bounds how fast the loop retries a failed enrollment. The
// enrollment token is short-lived, so retrying is worthwhile, but not in a hot
// loop against a control plane that may be briefly unreachable.
const enrollRetryInterval = 5 * time.Second

// identityStore persists the enrollment identity. *Store is the production
// implementation; the interface keeps the loop testable without a database.
type identityStore interface {
	Load(ctx context.Context, envName string) (*Identity, error)
	Save(ctx context.Context, id Identity) error
}

// Loop registers the controller with YCP and then reports its resource inventory
// on a heartbeat. It owns no HTTP server — it is a background goroutine started
// alongside the task worker.
type Loop struct {
	cfg          config.YCPConfig
	client       *Client
	store        identityStore
	inv          Inventory
	facts        ClusterFacts
	agentVersion string
	logger       *slog.Logger
}

// NewLoop wires the report loop from its collaborators.
func NewLoop(cfg config.YCPConfig, client *Client, store identityStore, inv Inventory, facts ClusterFacts, agentVersion string, logger *slog.Logger) *Loop {
	return &Loop{
		cfg:          cfg,
		client:       client,
		store:        store,
		inv:          inv,
		facts:        facts,
		agentVersion: agentVersion,
		logger:       logger,
	}
}

// Run enrolls (or resumes a persisted identity), then reports the inventory
// every ReportInterval until ctx is cancelled. It returns only when ctx is done.
func (l *Loop) Run(ctx context.Context) {
	id := l.ensureIdentity(ctx)
	if id == nil {
		return // ctx cancelled before we ever enrolled
	}

	// Report once immediately so the console reflects the environment as soon as
	// the controller is up, then on the interval.
	l.report(ctx, &id)

	ticker := time.NewTicker(l.cfg.ReportInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			l.logger.Info("ycp report loop stopping", "environment_id", id.EnvironmentID)
			return
		case <-ticker.C:
			l.report(ctx, &id)
		}
	}
}

// ensureIdentity resumes a persisted identity, or enrolls if there is none. It
// retries enrollment until it succeeds or ctx is cancelled (returning nil then).
func (l *Loop) ensureIdentity(ctx context.Context) *Identity {
	if id, err := l.store.Load(ctx, l.cfg.EnvName); err != nil {
		l.logger.Warn("could not read persisted ycp identity; will enroll", "error", err)
	} else if id != nil && id.StatusToken != "" {
		// A restart: resume on the persisted status token and do NOT re-enroll
		// (the enrollment token is a short-lived bootstrap credential).
		l.logger.Info("resuming persisted ycp identity; not enrolling",
			"env", id.EnvName, "environment_id", id.EnvironmentID)
		return id
	}

	for {
		id, err := l.enroll(ctx)
		if err == nil {
			return id
		}
		l.logger.Warn("ycp enroll failed; retrying", "error", err, "retry_in", enrollRetryInterval)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(enrollRetryInterval):
		}
	}
}

// enroll performs the handshake and persists the returned identity.
func (l *Loop) enroll(ctx context.Context) (*Identity, error) {
	resp, err := l.client.Enroll(ctx, EnrollRequest{
		Name:            l.cfg.EnvName,
		EnrollmentToken: l.cfg.EnrollmentToken,
		Placement:       l.cfg.Placement,
		Kind:            Kind,
		ClusterFacts:    l.facts,
	})
	if err != nil {
		return nil, err
	}
	id := &Identity{EnvName: l.cfg.EnvName, EnvironmentID: resp.EnvironmentID, StatusToken: resp.StatusToken}
	if err := l.store.Save(ctx, *id); err != nil {
		// The enroll succeeded but we could not persist it. Report on it anyway
		// this process lifetime; a restart will simply enroll again (the token is
		// reusable within its TTL), which is why persistence failure is not fatal.
		l.logger.Warn("could not persist ycp identity; continuing in-memory", "error", err)
	}
	l.logger.Info("enrolled with ycp", "env", id.EnvName, "environment_id", id.EnvironmentID)
	return id, nil
}

// report builds and posts one inventory heartbeat. On a rejected status token it
// re-enrolls and updates *id in place, so the next tick uses the fresh token.
func (l *Loop) report(ctx context.Context, id **Identity) {
	resources, err := BuildInventory(ctx, l.inv)
	if err != nil {
		l.logger.Warn("could not build ycp inventory; skipping this report", "error", err)
		return
	}
	rep := StatusReport{
		EnvironmentState: StateReady,
		AgentVersion:     l.agentVersion,
		ClusterFacts:     l.facts,
		Resources:        resources,
	}
	err = l.client.PostStatus(ctx, (*id).EnvironmentID, (*id).StatusToken, rep)
	if err == nil {
		l.logger.Debug("reported inventory to ycp", "environment_id", (*id).EnvironmentID, "resources", len(resources))
		return
	}
	if errors.Is(err, ErrUnauthorized) {
		l.logger.Warn("ycp rejected the status token; re-enrolling", "environment_id", (*id).EnvironmentID)
		if fresh, eerr := l.enroll(ctx); eerr != nil {
			l.logger.Warn("ycp re-enroll failed; will retry on the next report", "error", eerr)
		} else {
			*id = fresh
		}
		return
	}
	l.logger.Warn("could not report inventory to ycp", "error", err)
}
