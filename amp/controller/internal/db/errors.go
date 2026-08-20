package db

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// IsAbortedTransaction reports whether err is a transaction the database rolled
// back in full: 40001 serialization_failure, a lost write-write conflict.
//
// The full rollback is the point — nothing landed, so the caller may re-run. Do
// not extend this to codes that merely suggest another attempt is worth making (a
// connection timeout, a failed deploy): those can leave an effect behind, and a
// re-run would double it.
func IsAbortedTransaction(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40001"
}
