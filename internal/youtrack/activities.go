package youtrack

import (
	"context"
	"net/url"
	"strconv"
)

// Activity categories the sync subscribes to. CustomFieldCategory carries
// state, priority and lane changes; CommentsCategory carries operator
// comments; IssueCreatedCategory carries issues the operator filed in hk's
// project.
var ActivityCategories = []string{"CustomFieldCategory", "CommentsCategory", "IssueCreatedCategory"}

const activityFields = "id,timestamp,author(id,login,name,$type),target(id,idReadable,summary,description,$type,project(id,shortName,name,$type),customFields(id,name,$type,value))," +
	"categories($type,field(id,name,$type),added($type,name,id,text,value,presentation),removed($type,name,id,text,value,presentation))," +
	"added($type,name,id,text,value,presentation),removed($type,name,id,text,value,presentation)"

// ActivitiesPage reads one page after cursor. An empty cursor starts at the
// oldest recorded activity, so callers establish a baseline before applying it.
func (c *Client) ActivitiesPage(ctx context.Context, cursor string, limit int) (ActivitiesPage, error) {
	if limit < 1 || limit > 1000 {
		limit = 100
	}
	q := url.Values{}
	q.Set("fields", activityFields)
	q.Set("$top", strconv.Itoa(limit))
	if cursor != "" {
		q.Set("after", cursor)
	}
	for _, category := range ActivityCategories {
		q.Add("categories", category)
	}
	var out ActivitiesPage
	if err := c.get(ctx, "/api/activitiesPage", q, &out); err != nil {
		return ActivitiesPage{}, err
	}
	return out, nil
}
