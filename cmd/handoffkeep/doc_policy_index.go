package main

import (
	"context"
	"errors"
	"io"

	"github.com/mgh3326/handoffkeep/internal/policyindex"
	"github.com/mgh3326/handoffkeep/internal/remote"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// docPolicyIndex renders policy/index from the policy/index-list document.
// Without apply it prints the rendered index; with apply it writes the
// policy/index document.
func docPolicyIndex(ctx context.Context, c *remote.Client, apply bool, out io.Writer) error {
	list, ok, err := c.GetDocument(ctx, policyindex.ListKey)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("policy/index-list not found")
	}
	body, err := policyindex.Build(list.Body)
	if err != nil {
		return err
	}
	if !apply {
		_, err := io.WriteString(out, body)
		return err
	}
	v, changed, err := c.PutDocument(ctx, "", store.Document{Key: policyindex.IndexKey, Kind: "note", Body: body})
	if err != nil {
		return err
	}
	return printJSON(out, map[string]any{"document": v, "changed": changed})
}
