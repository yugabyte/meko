package rest

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/backend/internal/envelope"
	"github.com/yugabyte/meko/backend/internal/memory"
)

// answersStatus is GET /settings/answers: whether Search writes answers, and through what (Meko's own).
func (a *api) answersStatus(c echo.Context) error {
	st, err := a.Service.AnswerStatus(ctx(c))
	if err != nil {
		return serviceError(c, err)
	}
	return envelope.OK(c, st)
}

// setAnswers is PUT /settings/answers {harness, command, model}, for the CLI's admin token only: it
// points the server at a program to run. harness "" turns answers off.
func (a *api) setAnswers(c echo.Context) error {
	if a.Credential(c) != "admin" {
		return envelope.Fail(c, http.StatusForbidden, "FORBIDDEN", "Set up search answers with meko answers use claude.")
	}
	var body memory.AnswerSettings
	if ok, berr := bind(c, &body); !ok {
		return berr
	}
	if err := a.Service.SetAnswers(ctx(c), body); err != nil {
		return serviceError(c, err)
	}
	return a.answersStatus(c)
}

// answer is POST /datapacks/:id/answer {query, passages: [{source, id, text}]}: an answer to query
// written from the passages a search returned, {answer, cited, model}.
func (a *api) answer(c echo.Context) error {
	id, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	var body struct {
		Query    string           `json:"query"`
		Passages []memory.Passage `json:"passages"`
	}
	if ok, berr := bind(c, &body); !ok {
		return berr
	}
	out, err := a.Service.Answer(ctx(c), id, body.Query, body.Passages)
	switch {
	case errors.Is(err, memory.ErrAnswersOff):
		return envelope.Fail(c, http.StatusConflict, "ANSWERS_OFF", err.Error())
	case errors.Is(err, memory.ErrAnswer):
		return envelope.Fail(c, http.StatusBadGateway, "ANSWER_FAILED", err.Error())
	case err != nil:
		return serviceError(c, err)
	}
	return envelope.OK(c, out)
}
