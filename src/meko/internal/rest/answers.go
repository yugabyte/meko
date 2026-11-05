package rest

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/src/meko/internal/agents"
	"github.com/yugabyte/meko/src/meko/internal/envelope"
	"github.com/yugabyte/meko/src/meko/internal/memory"
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
// with no command, the server finds the harness's itself, and only the admin token may name one.
func (a *api) setAnswers(c echo.Context) error {
	if !a.owner(c) {
		return envelope.Fail(c, http.StatusForbidden, "FORBIDDEN",
			"Only Meko's owner can set up search answers: meko answers use claude, or Settings.")
	}
	var body memory.AnswerSettings
	ok, berr := bind(c, &body)
	if !ok {
		return berr
	}
	if body.Command, ok = a.harnessCommand(c, body.Harness, body.Command); !ok {
		return namedCommand(c)
	}
	if err := a.Service.SetAnswers(ctx(c), body); err != nil {
		return serviceError(c, err)
	}
	return a.answersStatus(c)
}

// harnessCommand is the program to run for a harness. Only the CLI's admin token names one (the CLI
// finds it on the user's PATH): ok is false for a command anyone else names, since local access is
// every user of the computer's. Without one it's what Meko finds as it finds the agent it connects,
// which may be "" (not installed), for the service to refuse.
func (a *api) harnessCommand(c echo.Context, harness, command string) (string, bool) {
	if command != "" {
		return command, a.Credential(c) == "admin"
	}
	if ag, found := agents.Lookup(harness); found {
		if p, ok := ag.Find(); ok {
			return p, true
		}
	}
	return "", true
}

// namedCommand refuses a command named by anyone but the CLI.
func namedCommand(c echo.Context) error {
	return envelope.Fail(c, http.StatusForbidden, "FORBIDDEN",
		"Only the CLI names the program Meko runs: leave command out, and Meko finds it itself.")
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
