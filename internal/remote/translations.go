package remote

import (
	"context"
	"net/url"
	"strconv"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func (c Client) PutTranslations(ctx context.Context, rows []store.Translation) (int, error) {
	var out struct {
		Count int `json:"count"`
	}
	err := c.call(ctx, "PUT", "/v1/translations", map[string]any{"translations": rows}, &out)
	return out.Count, err
}

func (c Client) MissingTranslations(ctx context.Context, entity, state string, limit int) ([]store.MissingTranslation, error) {
	q := url.Values{"entity": {entity}, "state": {state}, "limit": {strconv.Itoa(limit)}}
	var out struct {
		Translations []store.MissingTranslation `json:"translations"`
	}
	err := c.call(ctx, "GET", "/v1/translations/missing?"+q.Encode(), nil, &out)
	return out.Translations, err
}
