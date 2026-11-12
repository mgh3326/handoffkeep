package youtrack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// ArticleContent reads one article's content in one request, for the
// restore drill (`youtrack audit --rebuild`, hk 4121). Like GetIssue it makes
// exactly one attempt: a 429 is a *ytwrite.RemoteError with the server's
// Retry-After, which the caller waits out. A missing article is ErrNotFound.
func (r *WriteRemote) ArticleContent(ctx context.Context, articleID string) (string, error) {
	q := url.Values{}
	q.Set("fields", "id,content")
	path := "/api/articles/" + url.PathEscape(articleID)
	data, readErr, err := r.send(ctx, http.MethodGet, path, q, nil)
	var re *ytwrite.RemoteError
	if errors.As(err, &re) && re.Status == http.StatusNotFound {
		return "", fmt.Errorf("%w: article %s", ErrNotFound, articleID)
	}
	if err != nil {
		return "", err
	}
	if readErr != nil {
		return "", readErr
	}
	var out struct {
		Content *string `json:"content"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("youtrack GET article %s: decode: %w", articleID, err)
	}
	if out.Content == nil {
		return "", nil
	}
	return *out.Content, nil
}
