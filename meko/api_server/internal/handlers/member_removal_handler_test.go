package handlers_test

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/handlers"
	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// The owner can't be removed (400), removing someone with no accepted share is
// 404, and a user_id that isn't a UUID is 400 before it reaches the database.
const (
	removalOwner    = "00000000-0000-0000-0000-0000000000a1"
	removalStranger = "00000000-0000-0000-0000-0000000000b2"
)

func TestRemoveMember_StatusMapping(t *testing.T) {
	cases := map[string]struct {
		setup  func(sqlmock.Sqlmock)
		member string
		want   int
	}{
		"the owner":  {member: removalOwner, setup: func(sqlmock.Sqlmock) {}, want: http.StatusBadRequest},
		"not a uuid": {member: "abc", setup: func(sqlmock.Sqlmock) {}, want: http.StatusBadRequest},
		"not a member": {member: removalStranger, want: http.StatusNotFound, setup: func(m sqlmock.Sqlmock) {
			m.ExpectBegin()
			m.ExpectQuery(regexp.QuoteMeta("SET status = 'revoked'")).
				WillReturnRows(sqlmock.NewRows([]string{"recipient_email"}))
			m.ExpectRollback()
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			tc.setup(mock)
			h := handlers.NewSharingHandler(service.NewSharingService(repository.NewSharingRepo(db), nil))

			rec := httptest.NewRecorder()
			c := echo.New().NewContext(httptest.NewRequest(http.MethodDelete, "/", nil), rec)
			c.Set(auth.ContextKeyDatapack, &models.Datapack{ID: "dp", UserID: removalOwner, Grant: models.GrantOwner})
			c.SetParamNames("datapack_id", "user_id")
			c.SetParamValues("dp", tc.member)

			if err := h.RemoveMember(c); err != nil {
				t.Fatal(err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Error(err)
			}
			if rec.Code != tc.want {
				t.Errorf("got %d (%s), want %d", rec.Code, rec.Body.String(), tc.want)
			}
		})
	}
}
