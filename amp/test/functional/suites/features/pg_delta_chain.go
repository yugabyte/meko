package features

import (
	"fmt"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

const (
	deltaRows   = 64
	deltaRounds = 50
)

func init() {
	core.Register(core.Test{
		Name:        "features/pg_delta_chain",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Shared, Shape: core.SingleContainer},
		Tags:        []string{"regression", "nightly"},
		OwningTeam:  core.TeamCompute,
		Description: "Churn a small key set with many UPDATE rounds (long per-page version chains), CHECKPOINT, cold-reconnect, and verify every value reconstructs correctly.",
		Run:         runDeltaChain,
	})
}

func runDeltaChain(t *core.T) error {
	env, ok := t.Env.(pgEnv)
	if !ok {
		t.Skip("requires a docker single-container env")
		return nil
	}
	client, err := connect(t, env)
	if err != nil {
		return err
	}

	table := "delta_" + suffix(t)

	// Expected final value per id after all rounds: v starts at id, each round r
	// (1..deltaRounds) adds r to every row.
	var roundSum int64
	for r := 1; r <= deltaRounds; r++ {
		roundSum += int64(r)
	}
	want := make([]int64, deltaRows)
	var wantSum int64
	for id := 0; id < deltaRows; id++ {
		want[id] = int64(id) + roundSum
		wantSum += want[id]
	}

	if err := t.Step("churn_and_checkpoint", func(s *core.StepHandle) error {
		if _, err := client.Exec(t, fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, v bigint)", table)); err != nil {
			return err
		}
		for id := 0; id < deltaRows; id++ {
			if _, err := client.Exec(t, fmt.Sprintf("INSERT INTO %s (id, v) VALUES ($1, $2)", table), id, int64(id)); err != nil {
				return err
			}
		}
		for r := 1; r <= deltaRounds; r++ {
			if _, err := client.Exec(t, fmt.Sprintf("UPDATE %s SET v = v + $1", table), int64(r)); err != nil {
				return err
			}
		}
		s.Set("rounds", deltaRounds)
		s.Set("versions_per_page", deltaRounds+1)
		_, err := client.Exec(t, "CHECKPOINT")
		return err
	}); err != nil {
		client.Close()
		return err
	}
	client.Close()

	return t.Step("verify_cold", func(s *core.StepHandle) error {
		cold, err := connect(t, env)
		if err != nil {
			return err
		}
		defer cold.Close()
		defer func() { _, _ = cold.Exec(t, "DROP TABLE IF EXISTS "+table) }()

		count, err := cold.QueryInt(t, "SELECT count(*) FROM "+table)
		if err != nil {
			return err
		}
		if int(count) != deltaRows {
			return fmt.Errorf("cold reconnect after %d UPDATE rounds: table %s has %d rows, want %d (rows were lost across the version-chain rebuild)", deltaRounds, table, count, deltaRows)
		}
		gotSum, err := cold.QueryInt(t, "SELECT coalesce(sum(v),0) FROM "+table)
		if err != nil {
			return err
		}
		if gotSum != wantSum {
			return fmt.Errorf("cold reconnect after %d UPDATE rounds: sum(v) over %s = %d, want %d (the long per-page version chain did not reconstruct to the fully-updated values)", deltaRounds, table, gotSum, wantSum)
		}
		for _, id := range []int{0, deltaRows / 2, deltaRows - 1} {
			gotV, err := cold.QueryInt(t, "SELECT v FROM "+table+" WHERE id = $1", id)
			if err != nil {
				return err
			}
			if gotV != want[id] {
				return fmt.Errorf("cold reconnect after %d UPDATE rounds: row id=%d has v=%d, want %d (the per-page version chain reconstructed a stale or wrong value)", deltaRounds, id, gotV, want[id])
			}
		}
		s.Set("checksum", gotSum)
		return nil
	})
}
