package pg_test

import (
	"strings"
	"testing"

	"github.com/yugabyte/yb-amp/test/system/framework/apps/pg"
)

func TestDSNDefaults(t *testing.T) {
	dsn := pg.ConnInfo{Host: "127.0.0.1"}.DSN()
	for _, want := range []string{"host=127.0.0.1", "port=5432", "user=postgres", "dbname=postgres", "sslmode=disable"} {
		if !strings.Contains(dsn, want) {
			t.Errorf("dsn %q missing %q", dsn, want)
		}
	}
	if strings.Contains(dsn, "password=") {
		t.Errorf("no password should be emitted when unset: %q", dsn)
	}
}

func TestDSNOverrides(t *testing.T) {
	dsn := pg.ConnInfo{
		Host:     "db.local",
		Port:     55433,
		User:     "cloud_admin",
		Password: "cloud_admin",
		Database: "postgres",
		SSLMode:  "require",
	}.DSN()
	for _, want := range []string{"host=db.local", "port=55433", "user=cloud_admin", "password=cloud_admin", "sslmode=require"} {
		if !strings.Contains(dsn, want) {
			t.Errorf("dsn %q missing %q", dsn, want)
		}
	}
}

func TestDSNQuotesSpaces(t *testing.T) {
	dsn := pg.ConnInfo{Host: "127.0.0.1", Password: "a b'c"}.DSN()
	if !strings.Contains(dsn, `password='a b\'c'`) {
		t.Errorf("password with space/quote not escaped: %q", dsn)
	}
}

func TestAddr(t *testing.T) {
	if got := (pg.ConnInfo{Host: "h", Port: 55433}).Addr(); got != "h:55433" {
		t.Errorf("addr = %q, want h:55433", got)
	}
	if got := (pg.ConnInfo{Host: "h"}).Addr(); got != "h:5432" {
		t.Errorf("addr default = %q, want h:5432", got)
	}
}
