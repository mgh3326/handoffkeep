package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/youtrack"
)

// Documented exit codes. A Dagu job reads these; the JSON report is on stdout.
const (
	youtrackExitOK     = 0
	youtrackExitFailed = 1
	youtrackExitConfig = 2
	// youtrackExitFieldsOff: fields-backfill refused because
	// HANDOFFKEEP_YT_FIELDS_MODE is not on (hk 4022). Nothing was written.
	youtrackExitFieldsOff = 6
)

func youtrackCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(youtrackUsage)
	}
	switch args[0] {
	case "projects":
		return youtrackProjectsCmd(args[1:], out)
	case "ops":
		return youtrackOpsCmd(args[1:], out)
	case "canary":
		return youtrackCanaryCmd(args[1:], out)
	case "schema":
		return youtrackSchemaCmd(args[1:], out)
	case "comments-preflight":
		return youtrackCommentsCmd(args[1:], out, false)
	case "comments-backfill":
		return youtrackCommentsCmd(args[1:], out, true)
	case "articles-preflight":
		return youtrackArticlesCmd(args[1:], out, false)
	case "articles-backfill":
		return youtrackArticlesCmd(args[1:], out, true)
	case "issues-backfill":
		return youtrackIssuesBackfillCmd(args[1:], out)
	case "issues-retarget-drain":
		return youtrackIssuesRetargetDrainCmd(args[1:], out)
	case "fields-backfill":
		return youtrackFieldsBackfillCmd(args[1:], out)
	case "decisions-backfill":
		return youtrackDecisionsBackfillCmd(args[1:], out)
	case "audit":
		return youtrackAuditCmd(args[1:], out)
	case "authority":
		return youtrackAuthorityCmd(args[1:], out)
	default:
		return errors.New(youtrackUsage)
	}
}

// youtrackUsage also answers `yt`, the short name of this command.
const youtrackUsage = "usage: youtrack projects list|put | youtrack ops [--state S] [--kind K] [--task N] | youtrack canary add|remove|list | youtrack schema plan|apply [--project P] | youtrack comments-preflight [--project P] [--days N] [--all] | youtrack comments-backfill [--project P] [--days N] [--all] [--by NAME] | youtrack articles-preflight [--project P] | youtrack articles-backfill [--project P] [--by NAME] | youtrack audit [--project P] [--task N] [--enqueue] [--limit N] | youtrack issues-backfill [--project P] [--dry-run] | youtrack issues-retarget-drain | youtrack fields-backfill [--project P] [--task N] [--limit N] [--dry-run] | youtrack decisions-backfill [--apply] [--by NAME] [--limit N] | youtrack authority show | youtrack authority set <kind|all> <hk|youtrack> --by NAME --note TEXT"

// uiTaskIssuesFromEnv reads HANDOFFKEEP_UI_TASKS_SOURCE: unset or "hk" (the
// default) keeps the console's task pages on hk's task tables; "youtrack"
// makes the board, the work status page and the Home task counts read task
// issues from YouTrack with the bot token (hk 4430), and then needs
// YOUTRACK_URL and YOUTRACK_BOT_API_KEY. Any other value is an error, so a
// typo fails closed.
func uiTaskIssuesFromEnv() (*youtrack.Client, error) {
	switch raw := strings.TrimSpace(os.Getenv("HANDOFFKEEP_UI_TASKS_SOURCE")); raw {
	case "", "hk":
		return nil, nil
	case "youtrack":
	default:
		return nil, fmt.Errorf("HANDOFFKEEP_UI_TASKS_SOURCE=%q: want hk or youtrack", raw)
	}
	baseURL := strings.TrimSpace(os.Getenv("YOUTRACK_URL"))
	token := strings.TrimSpace(os.Getenv("YOUTRACK_BOT_API_KEY"))
	if baseURL == "" || token == "" {
		return nil, errors.New("HANDOFFKEEP_UI_TASKS_SOURCE=youtrack needs YOUTRACK_URL and YOUTRACK_BOT_API_KEY")
	}
	return youtrack.New(youtrack.Config{BaseURL: baseURL, Token: token, UserAgent: "handoffkeep-console"})
}

// serveYouTrack is what serve wires from the YouTrack configuration.
type serveYouTrack struct {
	// search backs api.Service.YouTrack; nil disables the YouTrack search.
	search *youtrack.Service
	// searchBackend is api.Service.SearchBackend.
	searchBackend string
	// comments is the b1a comment worker (yt_comments.go); nil when its mode
	// is off.
	comments backgroundWorker
}

// serveYouTrackFromEnv builds serve's YouTrack pieces from the environment.
// The comment worker reads its own credentials.
func serveYouTrackFromEnv(st *store.Store) (serveYouTrack, error) {
	search, err := youTrackSearchFromEnv(st)
	if err != nil {
		return serveYouTrack{}, err
	}
	searchBackend := searchBackendFromEnv()
	if searchBackend == youtrackSearchBackend && search == nil {
		log.Printf("search.backend youtrack ignored reason=youtrack_credentials_unset")
		searchBackend = ""
	}
	comments, err := ytCommentsWorkerFromEnv(st)
	if err != nil {
		return serveYouTrack{}, err
	}
	return serveYouTrack{search: search, searchBackend: searchBackend, comments: comments}, nil
}

// youTrackSearchFromEnv builds the YouTrack half of search. It exists only for
// HANDOFFKEEP_SEARCH_BACKEND=youtrack with YOUTRACK_URL and
// YOUTRACK_BOT_API_KEY both set; otherwise it is nil and search stays on
// PostgreSQL. It reads no mode switch: the search service holds no state
// beyond its client.
func youTrackSearchFromEnv(st *store.Store) (*youtrack.Service, error) {
	if searchBackendFromEnv() != youtrackSearchBackend {
		return nil, nil
	}
	baseURL := strings.TrimSpace(os.Getenv("YOUTRACK_URL"))
	token := strings.TrimSpace(os.Getenv("YOUTRACK_BOT_API_KEY"))
	if baseURL == "" || token == "" {
		return nil, nil
	}
	client, err := youtrack.New(youtrack.Config{BaseURL: baseURL, Token: token})
	if err != nil {
		return nil, err
	}
	return youtrack.NewService(st, client, youtrackServiceOptions()), nil
}

// youtrackProjectsCmd keeps the hk ↔ YouTrack project mapping. put resolves
// the short name to the project id once and stores both; list reads PG only.
func youtrackProjectsCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: youtrack projects list|put <hk project> <youtrack short name>")
	}
	dbURL := strings.TrimSpace(os.Getenv("HANDOFFKEEP_DB_URL"))
	if dbURL == "" {
		return exitCodeError{code: youtrackExitConfig, err: errors.New("HANDOFFKEEP_DB_URL is required")}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		return err
	}
	defer st.Close()
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return errors.New("usage: youtrack projects list")
		}
		projects, err := st.ListYouTrackProjects(ctx)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"projects": projects})
	case "put":
		if len(args) != 3 {
			return errors.New("usage: youtrack projects put <hk project> <youtrack short name>")
		}
		baseURL := strings.TrimSpace(os.Getenv("YOUTRACK_URL"))
		token := strings.TrimSpace(os.Getenv("YOUTRACK_BOT_API_KEY"))
		if baseURL == "" || token == "" {
			return exitCodeError{code: youtrackExitConfig, err: errors.New("YOUTRACK_URL and YOUTRACK_BOT_API_KEY are required")}
		}
		client, err := youtrack.New(youtrack.Config{BaseURL: baseURL, Token: token})
		if err != nil {
			return exitCodeError{code: youtrackExitConfig, err: err}
		}
		id, err := client.ProjectID(ctx, args[2])
		if err != nil {
			return err
		}
		project := youtrackProjectRow(args[1], id, args[2])
		if err := st.PutYouTrackProject(ctx, project); err != nil {
			return err
		}
		return printJSON(out, project)
	default:
		return errors.New("usage: youtrack projects list|put <hk project> <youtrack short name>")
	}
}

func youtrackProjectRow(hkProject, id, shortName string) store.YouTrackProject {
	return store.YouTrackProject{HKProject: hkProject, ProjectID: id, ShortName: shortName}
}

func youtrackServiceOptions() youtrack.Options {
	return youtrack.Options{SearchTimeout: youtrackSearchTimeoutFromEnv()}
}

// youtrackSearchTimeoutFromEnv reads HANDOFFKEEP_SEARCH_YOUTRACK_TIMEOUT, a Go
// duration such as "3s". Unset or invalid means the service default.
func youtrackSearchTimeoutFromEnv() time.Duration {
	raw := strings.TrimSpace(os.Getenv("HANDOFFKEEP_SEARCH_YOUTRACK_TIMEOUT"))
	if raw == "" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("search.youtrack_timeout_invalid value=%q using=default", raw)
		return 0
	}
	return d
}

// youtrackSearchBackend is the HANDOFFKEEP_SEARCH_BACKEND value that answers
// /v1/search from YouTrack.
const youtrackSearchBackend = "youtrack"

// searchBackendFromEnv reads HANDOFFKEEP_SEARCH_BACKEND. Only "youtrack"
// changes the answer; an unknown value is logged and means PostgreSQL.
func searchBackendFromEnv() string {
	raw := strings.TrimSpace(os.Getenv("HANDOFFKEEP_SEARCH_BACKEND"))
	switch raw {
	case "", "postgres":
		return ""
	case "youtrack":
		return raw
	}
	log.Printf("search.backend_unknown value=%q using=postgres", raw)
	return ""
}
