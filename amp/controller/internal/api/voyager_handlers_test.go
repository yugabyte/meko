package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/api"
	"github.com/yugabyte/amp/controller/internal/core"
	"github.com/yugabyte/amp/controller/internal/deploy"
	"github.com/yugabyte/amp/controller/internal/voyager/migration"
)

// fakeMigrationStore is an in-memory migration.Store for the handler tests: it
// covers the reads/writes the handlers touch and leaves the deployer- and
// Voyager-visualizer-backed methods as harmless zero-value stubs (the handlers
// under test never reach them).
type fakeMigrationStore struct {
	mu         sync.Mutex
	migrations map[uuid.UUID]migration.Migration
	activeRun  bool
	assessment *migration.Assessment
}

func newFakeMigrationStore() *fakeMigrationStore {
	return &fakeMigrationStore{migrations: map[uuid.UUID]migration.Migration{}}
}

var _ migration.Store = (*fakeMigrationStore)(nil)

func (f *fakeMigrationStore) seed(m migration.Migration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.migrations[m.ID] = m
}

func (f *fakeMigrationStore) CreateMigration(_ context.Context, id uuid.UUID, name string, source migration.SourceConfig) (migration.Migration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m, ok := f.migrations[id]; ok {
		return m, nil
	}
	now := time.Now()
	m := migration.Migration{
		ID: id, Name: name, Source: source,
		State: migration.StateCreated, Detail: "created",
		Runs: []migration.Run{}, CreatedAt: now, UpdatedAt: now,
	}
	f.migrations[id] = m
	return m, nil
}

func (f *fakeMigrationStore) GetMigration(_ context.Context, id uuid.UUID) (migration.Migration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.migrations[id]
	if !ok {
		return migration.Migration{}, migration.ErrNotFound
	}
	return m, nil
}

func (f *fakeMigrationStore) ListMigrations(_ context.Context, limit, offset int) ([]migration.MigrationSummary, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []migration.MigrationSummary
	for _, m := range f.migrations {
		out = append(out, migration.MigrationSummary{
			ID: m.ID, Name: m.Name, State: m.State, Detail: m.Detail,
			CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
		})
	}
	total := len(out)
	if offset >= len(out) {
		return []migration.MigrationSummary{}, total, nil
	}
	end := offset + limit
	if end > len(out) {
		end = len(out)
	}
	return out[offset:end], total, nil
}

func (f *fakeMigrationStore) HasActiveRun(_ context.Context, _ uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.activeRun, nil
}

func (f *fakeMigrationStore) NextAttempt(_ context.Context, _ uuid.UUID, _ migration.Operation) (int, error) {
	return 1, nil
}

func (f *fakeMigrationStore) GetAssessment(_ context.Context, id uuid.UUID) (migration.Assessment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.migrations[id]; !ok {
		return migration.Assessment{}, migration.ErrNotFound
	}
	if f.assessment == nil {
		return migration.Assessment{}, migration.ErrAssessmentNotFound
	}
	return *f.assessment, nil
}

// The remaining Store methods are not exercised by the handler tests.
func (f *fakeMigrationStore) GetRun(context.Context, uuid.UUID) (migration.Run, error) {
	return migration.Run{}, migration.ErrNotFound
}
func (f *fakeMigrationStore) LatestRuns(context.Context, uuid.UUID) ([]migration.Run, error) {
	return nil, nil
}
func (f *fakeMigrationStore) SetTargetTask(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (f *fakeMigrationStore) ClearTargetTask(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}
func (f *fakeMigrationStore) AttachTarget(context.Context, uuid.UUID, uuid.UUID, migration.TargetConfig) error {
	return nil
}
func (f *fakeMigrationStore) EnsureRun(context.Context, migration.RunSpec) (migration.Run, error) {
	return migration.Run{}, nil
}
func (f *fakeMigrationStore) MarkRunRunning(context.Context, uuid.UUID, uuid.UUID, string, json.RawMessage) error {
	return nil
}
func (f *fakeMigrationStore) MarkRunTerminal(context.Context, uuid.UUID, uuid.UUID, migration.RunStatus, string) error {
	return nil
}
func (f *fakeMigrationStore) LatestPhases(context.Context, uuid.UUID) ([]migration.Phase, error) {
	return nil, nil
}
func (f *fakeMigrationStore) LatestDataProgress(context.Context, uuid.UUID) (*migration.DataProgress, error) {
	return nil, nil
}
func (f *fakeMigrationStore) ReadVoyagerStatus(context.Context, uuid.UUID) (*migration.VoyagerStatus, error) {
	return nil, nil
}
func (f *fakeMigrationStore) LatestCompletedAssessmentPayload(context.Context, uuid.UUID) (json.RawMessage, error) {
	return nil, migration.ErrAssessmentNotFound
}

// fakeVoyagerRunner satisfies deploy.VoyagerJobRunner so the migration feature
// gate opens. The launcher tests only plan tasks (they don't drive the worker),
// so these methods are never called.
type fakeVoyagerRunner struct{}

func (fakeVoyagerRunner) StartRun(context.Context, deploy.VoyagerRunSpec) (deploy.RunRef, error) {
	return deploy.RunRef{}, nil
}
func (fakeVoyagerRunner) RunStatus(context.Context, deploy.RunRef) (deploy.RunState, error) {
	return "", nil
}

// newMigrationServer builds a server with the migration feature enabled over the
// given store (plus the derive-on-read service and a fake Voyager runner).
func newMigrationServer(t *testing.T, store *fakeMigrationStore) *api.ClientWithResponses {
	t.Helper()
	prov := api.Provisioning{
		VoyagerMigrations:       store,
		VoyagerMigrationService: migration.NewService(store, nil),
		VoyagerRunner:           fakeVoyagerRunner{},
	}
	client, _, _ := newTestServerWith(t, core.NewMemBackend(), prov)
	return client
}

func validSourceRequest(password string) api.MigrationSourceRequest {
	return api.MigrationSourceRequest{
		DbType:   api.MigrationSourceRequestDbTypePostgresql,
		Host:     "db.internal",
		Port:     5432,
		Username: "app_user",
		Password: password,
		Database: "prod",
		Schemas:  []string{"public"},
	}
}

func TestCreateMigrationValidation(t *testing.T) {
	client := newMigrationServer(t, newFakeMigrationStore())
	ctx := context.Background()

	// Empty name -> 400.
	badName, err := client.CreateMigrationWithResponse(ctx, api.CreateMigrationRequest{Name: "  ", Source: validSourceRequest("secret")})
	if err != nil {
		t.Fatalf("CreateMigration(empty name): %v", err)
	}
	if badName.StatusCode() != http.StatusBadRequest || badName.JSON400 == nil {
		t.Fatalf("empty name status = %d, body = %s", badName.StatusCode(), badName.Body)
	}

	// Unsupported db_type -> 400.
	badSource := validSourceRequest("secret")
	badSource.DbType = "mysql"
	badType, err := client.CreateMigrationWithResponse(ctx, api.CreateMigrationRequest{Name: "m", Source: badSource})
	if err != nil {
		t.Fatalf("CreateMigration(bad db_type): %v", err)
	}
	if badType.StatusCode() != http.StatusBadRequest || badType.JSON400 == nil {
		t.Fatalf("bad db_type status = %d, body = %s", badType.StatusCode(), badType.Body)
	}

	// Valid -> 201.
	ok, err := client.CreateMigrationWithResponse(ctx, api.CreateMigrationRequest{Name: "Migrate prod", Source: validSourceRequest("secret")})
	if err != nil {
		t.Fatalf("CreateMigration: %v", err)
	}
	if ok.StatusCode() != http.StatusCreated || ok.JSON201 == nil {
		t.Fatalf("create status = %d, body = %s", ok.StatusCode(), ok.Body)
	}
	if ok.JSON201.Name != "Migrate prod" || ok.JSON201.State != api.MigrationStateCreated {
		t.Errorf("created migration = {name:%q state:%q}, want {Migrate prod created}", ok.JSON201.Name, ok.JSON201.State)
	}
}

// TestGetMigrationRedactsPassword: the source password is stored internally but
// must never appear in a response — the response Source type has no password
// field, and the raw body must not carry the secret.
func TestGetMigrationRedactsPassword(t *testing.T) {
	store := newFakeMigrationStore()
	const secret = "sup3r-s3cret-pw"
	id := uuid.New()
	store.seed(migration.Migration{
		ID:   id,
		Name: "Migrate prod",
		Source: migration.SourceConfig{
			DBType: "postgresql", Host: "db.internal", Port: 5432,
			Username: "app_user", Password: secret, Database: "prod", Schemas: []string{"public"},
		},
		State: migration.StateCreated, Detail: "created",
		Runs: []migration.Run{}, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	client := newMigrationServer(t, store)

	got, err := client.GetMigrationWithResponse(context.Background(), id)
	if err != nil {
		t.Fatalf("GetMigration: %v", err)
	}
	if got.StatusCode() != http.StatusOK || got.JSON200 == nil {
		t.Fatalf("GetMigration status = %d, body = %s", got.StatusCode(), got.Body)
	}
	if got.JSON200.Source.Username != "app_user" || got.JSON200.Source.Database != "prod" {
		t.Errorf("source projection = %+v, want app_user@prod", got.JSON200.Source)
	}
	if strings.Contains(string(got.Body), secret) {
		t.Fatal("getMigration response body contains the source password; it must be redacted")
	}
}

func TestGetMigrationNotFound(t *testing.T) {
	client := newMigrationServer(t, newFakeMigrationStore())
	got, err := client.GetMigrationWithResponse(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("GetMigration(missing): %v", err)
	}
	if got.StatusCode() != http.StatusNotFound || got.JSON404 == nil {
		t.Fatalf("GetMigration(missing) status = %d, body = %s", got.StatusCode(), got.Body)
	}
}

// TestStartMigrationWithoutTarget: migrate requires an attached target; without
// one the handler returns 409 target_not_attached before planning a task.
func TestStartMigrationWithoutTarget(t *testing.T) {
	store := newFakeMigrationStore()
	id := uuid.New()
	store.seed(migration.Migration{
		ID: id, Name: "m", Source: migration.SourceConfig{DBType: "postgresql", Database: "prod"},
		State: migration.StateCreated, Runs: []migration.Run{}, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	client := newMigrationServer(t, store)

	resp, err := client.StartMigrationWithResponse(context.Background(), id, api.StartMigrationRequest{})
	if err != nil {
		t.Fatalf("StartMigration: %v", err)
	}
	if resp.StatusCode() != http.StatusConflict || resp.JSON409 == nil {
		t.Fatalf("StartMigration(no target) status = %d, body = %s", resp.StatusCode(), resp.Body)
	}
	if resp.JSON409.Error.Code != "target_not_attached" {
		t.Errorf("error code = %q, want target_not_attached", resp.JSON409.Error.Code)
	}
}

// TestStartMigrationAssessmentActiveRun: a second launcher is rejected with 409
// while a run is already in flight (the handler's cheap active-run precheck).
func TestStartMigrationAssessmentActiveRun(t *testing.T) {
	store := newFakeMigrationStore()
	store.activeRun = true
	id := uuid.New()
	store.seed(migration.Migration{
		ID: id, Name: "m", Source: migration.SourceConfig{DBType: "postgresql", Database: "prod"},
		State: migration.StateAssessing, Runs: []migration.Run{}, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	client := newMigrationServer(t, store)

	resp, err := client.StartMigrationAssessmentWithResponse(context.Background(), id)
	if err != nil {
		t.Fatalf("StartMigrationAssessment: %v", err)
	}
	if resp.StatusCode() != http.StatusConflict || resp.JSON409 == nil {
		t.Fatalf("StartMigrationAssessment(active) status = %d, body = %s", resp.StatusCode(), resp.Body)
	}
	if resp.JSON409.Error.Code != "run_in_progress" {
		t.Errorf("error code = %q, want run_in_progress", resp.JSON409.Error.Code)
	}
}

// TestStartMigrationAssessmentReturnsTask: with no active run, assess returns
// 202 + a pending start_migration_run task with a Location header.
func TestStartMigrationAssessmentReturnsTask(t *testing.T) {
	store := newFakeMigrationStore()
	id := uuid.New()
	store.seed(migration.Migration{
		ID: id, Name: "m", Source: migration.SourceConfig{DBType: "postgresql", Database: "prod"},
		State: migration.StateCreated, Runs: []migration.Run{}, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	client := newMigrationServer(t, store)

	resp, err := client.StartMigrationAssessmentWithResponse(context.Background(), id)
	if err != nil {
		t.Fatalf("StartMigrationAssessment: %v", err)
	}
	if resp.StatusCode() != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body = %s", resp.StatusCode(), resp.Body)
	}
	assertTaskAccepted(t, resp.JSON202, resp.HTTPResponse.Header.Get("Location"), "start_migration_run")
}

func TestEndMigrationRequiresCompletedMigration(t *testing.T) {
	store := newFakeMigrationStore()
	id := uuid.New()
	store.seed(migration.Migration{
		ID: id, Name: "m", State: migration.StateTargetReady,
		Runs: []migration.Run{}, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	client := newMigrationServer(t, store)

	resp, err := client.EndMigrationWithResponse(context.Background(), id)
	if err != nil {
		t.Fatalf("EndMigration: %v", err)
	}
	if resp.StatusCode() != http.StatusConflict || resp.JSON409 == nil {
		t.Fatalf("EndMigration(incomplete) status = %d, body = %s", resp.StatusCode(), resp.Body)
	}
	if resp.JSON409.Error.Code != "migration_not_completed" {
		t.Errorf("error code = %q, want migration_not_completed", resp.JSON409.Error.Code)
	}
}

func TestEndMigrationReturnsTask(t *testing.T) {
	store := newFakeMigrationStore()
	id := uuid.New()
	store.seed(migration.Migration{
		ID: id, Name: "m", State: migration.StateCompleted,
		Runs: []migration.Run{{
			Operation: migration.OperationMigrate, Attempt: 1, Status: migration.RunSucceeded,
		}}, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	client := newMigrationServer(t, store)

	resp, err := client.EndMigrationWithResponse(context.Background(), id)
	if err != nil {
		t.Fatalf("EndMigration: %v", err)
	}
	if resp.StatusCode() != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body = %s", resp.StatusCode(), resp.Body)
	}
	assertTaskAccepted(t, resp.JSON202, resp.HTTPResponse.Header.Get("Location"), "start_migration_run")
}

func TestEndMigrationRejectsActiveAndEnded(t *testing.T) {
	for _, test := range []struct {
		name   string
		state  migration.MigrationState
		active bool
		code   string
	}{
		{name: "active", state: migration.StateEnding, active: true, code: "run_in_progress"},
		{name: "ended", state: migration.StateEnded, code: "already_ended"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newFakeMigrationStore()
			store.activeRun = test.active
			id := uuid.New()
			store.seed(migration.Migration{
				ID: id, Name: "m", State: test.state,
				Runs: []migration.Run{}, CreatedAt: time.Now(), UpdatedAt: time.Now(),
			})
			client := newMigrationServer(t, store)
			resp, err := client.EndMigrationWithResponse(context.Background(), id)
			if err != nil {
				t.Fatalf("EndMigration: %v", err)
			}
			if resp.StatusCode() != http.StatusConflict || resp.JSON409 == nil {
				t.Fatalf("status = %d, body = %s", resp.StatusCode(), resp.Body)
			}
			if resp.JSON409.Error.Code != test.code {
				t.Errorf("error code = %q, want %q", resp.JSON409.Error.Code, test.code)
			}
		})
	}
}

func TestEndMigrationRetryAfterFailedEnd(t *testing.T) {
	store := newFakeMigrationStore()
	id := uuid.New()
	store.seed(migration.Migration{
		ID: id, Name: "m", State: migration.StateFailed,
		Runs: []migration.Run{
			{Operation: migration.OperationMigrate, Attempt: 1, Status: migration.RunSucceeded},
			{Operation: migration.OperationEnd, Attempt: 1, Status: migration.RunFailed},
		}, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	client := newMigrationServer(t, store)

	resp, err := client.EndMigrationWithResponse(context.Background(), id)
	if err != nil {
		t.Fatalf("EndMigration: %v", err)
	}
	if resp.StatusCode() != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (retry allowed), body = %s", resp.StatusCode(), resp.Body)
	}
}

func TestEndMigrationRejectsFailedMigrateWithoutSuccess(t *testing.T) {
	store := newFakeMigrationStore()
	id := uuid.New()
	store.seed(migration.Migration{
		ID: id, Name: "m", State: migration.StateFailed,
		Runs: []migration.Run{
			{Operation: migration.OperationMigrate, Attempt: 1, Status: migration.RunFailed},
		}, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	client := newMigrationServer(t, store)

	resp, err := client.EndMigrationWithResponse(context.Background(), id)
	if err != nil {
		t.Fatalf("EndMigration: %v", err)
	}
	if resp.StatusCode() != http.StatusConflict || resp.JSON409 == nil {
		t.Fatalf("status = %d, want 409, body = %s", resp.StatusCode(), resp.Body)
	}
	if resp.JSON409.Error.Code != "migration_not_completed" {
		t.Errorf("error code = %q, want migration_not_completed", resp.JSON409.Error.Code)
	}
}

func TestAssessAndMigrateRejectedAfterEnd(t *testing.T) {
	store := newFakeMigrationStore()
	id := uuid.New()
	store.seed(migration.Migration{
		ID: id, Name: "m", State: migration.StateEnded,
		Target: &migration.TargetConfig{},
		Runs: []migration.Run{
			{Operation: migration.OperationMigrate, Attempt: 1, Status: migration.RunSucceeded},
			{Operation: migration.OperationEnd, Attempt: 1, Status: migration.RunSucceeded},
		}, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	client := newMigrationServer(t, store)

	assessResp, err := client.StartMigrationAssessmentWithResponse(context.Background(), id)
	if err != nil {
		t.Fatalf("StartMigrationAssessment: %v", err)
	}
	if assessResp.StatusCode() != http.StatusConflict || assessResp.JSON409 == nil {
		t.Fatalf("assess status = %d, want 409, body = %s", assessResp.StatusCode(), assessResp.Body)
	}
	if assessResp.JSON409.Error.Code != "migration_ended" {
		t.Errorf("assess error code = %q, want migration_ended", assessResp.JSON409.Error.Code)
	}
	if assessResp.JSON409.Error.Message != "end migration has already been called" {
		t.Errorf("assess error message = %q, want end migration has already been called", assessResp.JSON409.Error.Message)
	}

	migrateResp, err := client.StartMigrationWithResponse(context.Background(), id, api.StartMigrationRequest{})
	if err != nil {
		t.Fatalf("StartMigration: %v", err)
	}
	if migrateResp.StatusCode() != http.StatusConflict || migrateResp.JSON409 == nil {
		t.Fatalf("migrate status = %d, want 409, body = %s", migrateResp.StatusCode(), migrateResp.Body)
	}
	if migrateResp.JSON409.Error.Code != "migration_ended" {
		t.Errorf("migrate error code = %q, want migration_ended", migrateResp.JSON409.Error.Code)
	}
	if migrateResp.JSON409.Error.Message != "end migration has already been called" {
		t.Errorf("migrate error message = %q, want end migration has already been called", migrateResp.JSON409.Error.Message)
	}
}

func TestAssessRejectedWhileEndInFlight(t *testing.T) {
	store := newFakeMigrationStore()
	id := uuid.New()
	store.seed(migration.Migration{
		ID: id, Name: "m", State: migration.StateEnding,
		Runs: []migration.Run{
			{Operation: migration.OperationMigrate, Attempt: 1, Status: migration.RunSucceeded},
			{Operation: migration.OperationEnd, Attempt: 1, Status: migration.RunPending},
		}, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	client := newMigrationServer(t, store)

	resp, err := client.StartMigrationAssessmentWithResponse(context.Background(), id)
	if err != nil {
		t.Fatalf("StartMigrationAssessment: %v", err)
	}
	if resp.StatusCode() != http.StatusConflict || resp.JSON409 == nil {
		t.Fatalf("status = %d, want 409, body = %s", resp.StatusCode(), resp.Body)
	}
	if resp.JSON409.Error.Code != "migration_ended" || resp.JSON409.Error.Message != "end migration has already been called" {
		t.Errorf("error = %q %q, want migration_ended / end migration has already been called", resp.JSON409.Error.Code, resp.JSON409.Error.Message)
	}
}

// TestConcurrentEndMigrationRejected: before the first launcher writes its run
// row, HasActiveRun cannot see it, so the second request must be rejected by the
// launcher task lock rather than accepted and failed later in the worker.
func TestConcurrentEndMigrationRejected(t *testing.T) {
	store := newFakeMigrationStore()
	id := uuid.New()
	store.seed(migration.Migration{
		ID: id, Name: "m", State: migration.StateCompleted,
		Runs: []migration.Run{
			{Operation: migration.OperationMigrate, Attempt: 1, Status: migration.RunSucceeded},
		}, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	client := newMigrationServer(t, store)

	first, err := client.EndMigrationWithResponse(context.Background(), id)
	if err != nil {
		t.Fatalf("first EndMigration: %v", err)
	}
	if first.StatusCode() != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202, body = %s", first.StatusCode(), first.Body)
	}

	second, err := client.EndMigrationWithResponse(context.Background(), id)
	if err != nil {
		t.Fatalf("second EndMigration: %v", err)
	}
	if second.StatusCode() != http.StatusConflict || second.JSON409 == nil {
		t.Fatalf("second status = %d, want 409, body = %s", second.StatusCode(), second.Body)
	}
	if second.JSON409.Error.Code != "run_in_progress" {
		t.Errorf("second error code = %q, want run_in_progress", second.JSON409.Error.Code)
	}
}

func TestEndMigrationEligibleRegardlessOfRunOrder(t *testing.T) {
	store := newFakeMigrationStore()
	id := uuid.New()
	store.seed(migration.Migration{
		ID: id, Name: "m", State: migration.StateCompleted,
		Runs: []migration.Run{
			{Operation: migration.OperationMigrate, Attempt: 1, Status: migration.RunSucceeded},
			{Operation: migration.OperationAssess, Attempt: 2, Status: migration.RunSucceeded},
		}, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	client := newMigrationServer(t, store)

	resp, err := client.EndMigrationWithResponse(context.Background(), id)
	if err != nil {
		t.Fatalf("EndMigration: %v", err)
	}
	if resp.StatusCode() != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body = %s", resp.StatusCode(), resp.Body)
	}
}

// TestMigrationAPIDisabled: without a Voyager store the routes refuse with 501.
func TestMigrationAPIDisabled(t *testing.T) {
	client, _ := newTestServerWithProv(t, api.Provisioning{})
	resp, err := client.ListMigrationsWithResponse(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListMigrations: %v", err)
	}
	if resp.StatusCode() != http.StatusNotImplemented {
		t.Fatalf("ListMigrations(disabled) status = %d, want 501, body = %s", resp.StatusCode(), resp.Body)
	}
}
