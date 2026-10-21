package mcp

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"

	"github.com/yugabyte/meko/backend/internal/memory"
	"github.com/yugabyte/meko/backend/internal/wire"
)

func artifactTools() []*tool {
	return []*tool{
		{
			name: "artifact_put", title: "Save Artifact", kind: write, failCode: "artifact_put_failed",
			desc: "Keep a file (a report, a design, an export) in the datapack, up to 5 MiB. The same bytes are " +
				"kept once; artifact_get reads it back by the content_hash this returns.",
			params: []param{
				{name: "filename", typ: "string", required: true, desc: "Its name, with its extension."},
				{name: "content_base64", typ: "string", required: true, desc: "Its bytes, base64."},
				{name: "content_type", typ: "string", required: true, desc: "Its MIME type, such as text/html."},
				pConversation, pDatapack, pAgent,
			},
			run: artifactPut,
		},
		{
			// Read-only as Cloud annotates it: the one write, the file's last-read time, is
			// bookkeeping, not a change to the artifact.
			name: "artifact_get", title: "Get Artifact", kind: readOnly, failCode: "artifact_get_failed",
			desc: "Read back a file kept with artifact_put, by its content_hash. The bytes come back as " +
				"content_base64.",
			params: []param{
				{name: "content_hash", typ: "string", required: true, desc: "The SHA-256 artifact_put returned."},
				pConversation, pDatapack, pAgent,
			},
			run: artifactGet,
		},
	}
}

func artifactPut(ctx context.Context, c *call, a args) (any, error) {
	if _, err := c.conversationID(a); err != nil {
		return nil, err
	}
	var filename, b64, contentType string
	var err error
	for name, dst := range map[string]*string{"filename": &filename, "content_base64": &b64,
		"content_type": &contentType} {
		if *dst, err = a.str(name); err != nil {
			return nil, err
		}
	}
	agent, err := c.agent(a)
	if err != nil {
		return nil, err
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, fail("invalid_base64", "content_base64 is not valid base64.")
	}
	dp, err := c.datapack(ctx, a)
	if err != nil {
		return nil, err
	}
	art, err := c.svc.PutArtifact(ctx, dp, agent, filename, contentType, b)
	if errors.Is(err, memory.ErrTooLarge) {
		return nil, fail("artifact_too_large", "The file is larger than 5 MiB, the most an artifact can be.")
	}
	if err != nil {
		return nil, err
	}
	return wire.ArtifactOf(art), nil
}

func artifactGet(ctx context.Context, c *call, a args) (any, error) {
	if _, err := c.conversationID(a); err != nil {
		return nil, err
	}
	hash, err := a.str("content_hash")
	if err != nil {
		return nil, err
	}
	if _, err = c.agent(a); err != nil {
		return nil, err
	}
	dp, err := c.datapack(ctx, a)
	if err != nil {
		return nil, err
	}
	art, err := c.svc.GetArtifact(ctx, dp, strings.ToLower(strings.TrimSpace(hash)))
	if errors.Is(err, memory.ErrNotFound) {
		return nil, fail("ARTIFACT_NOT_FOUND", "No artifact with that content_hash in this datapack.")
	}
	if err != nil {
		return nil, err
	}
	out := wire.ArtifactOf(art)
	out.ContentBase64 = base64.StdEncoding.EncodeToString(art.Bytes)
	return out, nil
}
