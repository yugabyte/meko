package cloudlink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// The codes a failed listing answers with (docs/cloud.md), each a 502 on the route.
const (
	CodeNoAPIURL    = "CLOUD_NO_API_URL"
	CodeUnreachable = "CLOUD_UNREACHABLE"
	CodeKeyRefused  = "CLOUD_KEY_REFUSED"
	CodeListFailed  = "CLOUD_LIST_FAILED"
)

// maxListBytes bounds Meko Cloud's reply: a heavy account's 180 datapacks come to about 150 KB.
const maxListBytes = 8 << 20

// Datapack is a Meko Cloud datapack as Meko shows it. Only these fields are copied from Cloud's row,
// which also carries the datapack's database user and connection string.
type Datapack struct {
	DatapackID   string `json:"datapack_id"`
	DatapackName string `json:"datapack_name"`
	Description  string `json:"description"`
	// Grant is owner for the account's own datapacks; any other (maintainer, contributor, viewer)
	// means it was shared with the account.
	Grant     string `json:"grant"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	// WebURL opens it in the link's web app; null when that can't be known.
	WebURL *string `json:"web_url"`
}

// Datapacks is the route's reply: the link's Meko Cloud datapacks, newest first, or linked false.
type Datapacks struct {
	Linked    bool       `json:"linked"`
	APIURL    *string    `json:"api_url,omitempty"`
	WebURL    *string    `json:"web_url,omitempty"`
	Datapacks []Datapack `json:"datapacks"`
}

// ListError is a listing Meko Cloud didn't answer, with the code the route answers.
type ListError struct {
	Code string
	msg  string
}

func (e *ListError) Error() string { return e.msg }

// ListDatapacks asks the REST API at apiURL for the key's datapacks: GET /datapacks with no query,
// which Meko Cloud answers with every row (a limit would switch it to cursor pages). The error is a
// *ListError, and never carries the key.
func ListDatapacks(ctx context.Context, apiURL, key string) ([]Datapack, error) {
	ctx, cancel := context.WithTimeout(ctx, CheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(apiURL, "/")+"/datapacks", nil)
	if err != nil {
		return nil, &ListError{Code: CodeListFailed, msg: "Meko Cloud's API URL isn't one Meko can call."}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := http.DefaultClient.Do(req) //nolint:gosec // G704: the link's API URL, checked when it was saved
	if err != nil {
		why := "it timed out"
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			why = shortError(ctx, err).Error()
		}
		return nil, &ListError{Code: CodeUnreachable, msg: "Meko Cloud didn't answer: " + why + "."}
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		return nil, &ListError{Code: CodeKeyRefused,
			msg: "Meko Cloud refused the key. Link it again in Settings › Meko Cloud."}
	}
	var env struct {
		Data  json.RawMessage `json:"data"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	body, _ := io.ReadAll(io.LimitReader(res.Body, maxListBytes))
	jsonErr := json.Unmarshal(body, &env)
	failed := func(why string) error {
		return &ListError{Code: CodeListFailed, msg: "Meko Cloud couldn't list datapacks: " + why + "."}
	}
	switch {
	case jsonErr == nil && env.Error != nil && env.Error.Message != "":
		return nil, failed(strings.TrimSuffix(shorten(env.Error.Message), "."))
	case res.StatusCode/100 != 2:
		return nil, failed(fmt.Sprintf("it answered %d", res.StatusCode))
	case jsonErr != nil:
		return nil, failed("its reply isn't Meko's API")
	}
	var rows []Datapack
	if err := json.Unmarshal(env.Data, &rows); err != nil {
		return nil, failed("its reply isn't a list of datapacks")
	}
	return rows, nil
}

func shorten(s string) string {
	if r := []rune(s); len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}

// Datapacks lists the linked Meko Cloud's datapacks, newest first, each with its link in the web
// app; linked false when it isn't linked. It doesn't take the change lock: a listing never changes
// the link.
func (s *Service) Datapacks(ctx context.Context) (Datapacks, error) {
	st, err := s.Store.Load()
	if err != nil {
		return Datapacks{}, err
	}
	if !st.Linked {
		return Datapacks{Datapacks: []Datapack{}}, nil
	}
	api := st.Link.EffectiveAPIURL()
	if api == "" {
		return Datapacks{}, &ListError{Code: CodeNoAPIURL, msg: "The Meko Cloud link has no API URL. " +
			"Set the web app or API URL in Settings › Meko Cloud."}
	}
	list := s.List
	if list == nil {
		list = ListDatapacks
	}
	rows, err := list(ctx, api, st.Link.Key)
	if err != nil {
		return Datapacks{}, err
	}
	web := st.Link.EffectiveWebURL()
	for i := range rows {
		rows[i].WebURL = nil
		if web != "" && rows[i].DatapackID != "" {
			u := web + "/datapacks/" + rows[i].DatapackID
			rows[i].WebURL = &u
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return newer(rows[i].CreatedAt, rows[j].CreatedAt) })
	if rows == nil {
		rows = []Datapack{}
	}
	return Datapacks{Linked: true, APIURL: &api, WebURL: strOrNil(web), Datapacks: rows}, nil
}

// newer says whether timestamp a is after b; ones that don't parse sort last.
func newer(a, b string) bool {
	ta, errA := time.Parse(time.RFC3339Nano, a)
	tb, errB := time.Parse(time.RFC3339Nano, b)
	switch {
	case errA != nil:
		return false
	case errB != nil:
		return true
	}
	return ta.After(tb)
}
