package rest

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/backend/internal/agents"
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

// setAnswers is PUT /settings/answers {harness, command, model}, for the owner only (the CLI's admin
// token, or the web UI's Settings): it makes the server run a program. harness "" turns answers off;
// with no command, the server finds the harness's itself.
func (a *api) setAnswers(c echo.Context) error {
	if !a.owner(c) {
		return envelope.Fail(c, http.StatusForbidden, "FORBIDDEN",
			"Only Meko's owner can set up search answers: meko answers use claude, or Settings.")
	}
	var body memory.AnswerSettings
	if ok, berr := bind(c, &body); !ok {
		return berr
	}
	body.Command = harnessCommand(body.Harness, body.Command)
	if err := a.Service.SetAnswers(ctx(c), body); err != nil {
		return serviceError(c, err)
	}
	return a.answersStatus(c)
}

// harnessCommand is the program to run for a harness: command when given (the CLI finds it on the
// user's PATH), else the one Meko finds as it finds the agent it connects, which may be "" (not
// installed), for the service to refuse.
func harnessCommand(harness, command string) string {
	if harness == "" || command != "" {
		return command
	}
	if ag, ok := agents.Lookup(harness); ok {
		if p, found := ag.Find(); found {
			return p
		}
	}
	return ""
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
