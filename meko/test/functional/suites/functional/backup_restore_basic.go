package functional

import (
	"fmt"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/framework/universe"
)

func init() {
	core.Register(core.Test{
		Name: "functional/backup_restore_basic", Suite: "functional", Category: core.Functional, Tags: tags("universe"), OwningTeam: core.TeamPlatform, Unguarded: true,
		Env: env(), Timeout: 45 * time.Minute,
		Description: "A database is backed up to the stack's MinIO from one universe and restored into a second, and the second holds the same rows.",
		Params:      []string{"db"},
		Run:         backupRestoreBasic,
	})
}

func backupRestoreBasic(t *core.T) error {
	var b universe.Backups
	src, dst, err := pair(t, func(src universe.Universe) { b = need[universe.Backups](t, src) })
	if err != nil {
		return finish(t, err)
	}
	r := need[universe.Backups](t, dst)
	s3 := stackS3(t)

	if err := step(t, "seed_source", func(h *core.StepHandle) error {
		if _, err := src.SQL(t, "", "", "CREATE DATABASE app"); err != nil {
			return err
		}
		_, err := src.SQL(t, "", "app", "CREATE TABLE kv(k int primary key, v text); INSERT INTO kv SELECT i, 'row-' || i FROM generate_series(1, 200) i")
		return err
	}); err != nil {
		return err
	}
	var bk universe.Backup
	if err := step(t, "backup", func(h *core.StepHandle) error {
		var err error
		bk, err = b.Backup(t, "app", s3)
		h.Set("location", bk.Location)
		return (err)
	}); err != nil {
		return err
	}
	if err := step(t, "restore_into_second", func(*core.StepHandle) error {
		return r.Restore(t, bk, s3)
	}); err != nil {
		return err
	}
	return step(t, "data_equal", func(*core.StepHandle) error {
		const q = "SELECT count(*) || ':' || sum(k) || ':' || min(v) || ':' || max(v) FROM kv"
		want, err := src.SQL(t, "", "app", q)
		if err != nil {
			return err
		}
		got, err := dst.SQL(t, "", "app", q)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("restored %q, source has %q", got, want)
		}
		return nil
	})
}
