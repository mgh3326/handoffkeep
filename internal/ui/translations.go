package ui

import (
	"context"
	"encoding/json"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func (h *Handler) translateDecisionViews(ctx context.Context, views []decisionRequestView) error {
	sources := make([]store.TranslationSource, 0, len(views)*2)
	for _, v := range views {
		sources = append(sources, store.TranslationSource{EntityID: v.ID, Field: "question", Original: v.Question})
		if len(v.Options) > 0 {
			original, err := json.Marshal(store.DecisionOptions{Options: v.Options, AllowFree: v.AllowFree})
			if err != nil {
				return err
			}
			sources = append(sources, store.TranslationSource{EntityID: v.ID, Field: "options", Original: string(original)})
		}
	}
	translations, err := h.store.ListTranslations(ctx, "decision_request", sources)
	if err != nil {
		return err
	}
	for i := range views {
		if views[i].QuestionKO != "" {
			continue
		}
		for _, tr := range translations {
			if tr.EntityID != views[i].ID {
				continue
			}
			switch tr.Field {
			case "question":
				views[i].QuestionKO = tr.Text
				views[i].QuestionKOStale = tr.Stale
			case "options":
				views[i].OptionsKOStale = tr.Stale
				var options store.DecisionOptions
				if json.Unmarshal([]byte(tr.Text), &options) != nil || len(options.Options) != len(views[i].Options) {
					views[i].OptionsKOStale = true
					continue
				}
				for j, o := range options.Options {
					if o.Key != views[i].Options[j].Key {
						views[i].OptionsKOStale = true
					}
				}
				views[i].OptionsKO = options.Options
			}
		}
	}
	return nil
}
