package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"github.com/yugabyte/meko/backend/internal/cloudlink"
	"github.com/yugabyte/meko/backend/internal/paths"
)

// cloudSide is mekosh's way to the linked Meko Cloud's datapacks: the running server's own /api/v1
// routes, which send a datapack this Meko doesn't have to Meko Cloud with the link's key, which
// mekosh never reads (docs/cloud.md).
type cloudSide struct{ c *client }

func openCloud(l paths.Layout) (*cloudSide, error) {
	c, err := newClient(l)
	if err != nil {
		return nil, err
	}
	c.http.Timeout = toolTimeout
	return &cloudSide{c}, nil
}

// api calls one of the running server's /api/v1 routes and returns its reply's data.
func (c *cloudSide) api(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	var data json.RawMessage
	err := c.c.send(ctx, method, "/api/v1"+path, body, &data)
	return data, err
}

// datapacks are the linked Meko Cloud's datapacks, the account's own and those shared with it;
// errNotLinked when there's no link.
func (c *cloudSide) datapacks(ctx context.Context) ([]cloudlink.Datapack, error) {
	var rows []cloudlink.Datapack
	err := c.c.send(ctx, http.MethodGet, "/api/v1/datapacks?location=cloud", nil, &rows)
	if ae := (*apiError)(nil); errors.As(err, &ae) && ae.code == cloudlink.CodeNotLinked {
		return nil, errNotLinked
	}
	return rows, err
}

var errNotLinked = errors.New("this Meko isn't linked to Meko Cloud: meko cloud_link links it")

// datapackPath is a datapack's /api/v1 path, plus rest.
func datapackPath(id, rest string) string {
	return "/datapacks/" + url.PathEscape(id) + rest
}
