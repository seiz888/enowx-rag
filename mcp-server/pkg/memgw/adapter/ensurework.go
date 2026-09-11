package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// EnsuredWork is what the gateway's work-ensure route answers: the work a
// session belongs to, whether it was already open or created fresh.
type EnsuredWork struct {
	WorkID      uuid.UUID `json:"work_id"`
	ProjectID   uuid.UUID `json:"project_id"`
	WorkspaceID uuid.UUID `json:"workspace_id"`
	Title       string    `json:"title"`
	State       string    `json:"state"`
	Revision    int64     `json:"revision"`
	Created     bool      `json:"created"`
}

// EnsureWork names the work a working directory belongs to, creating it when
// the project and workspace have no work currently open.
//
// It is the piece that removes the manual uuid from the loop: a resolution map
// names project and workspace (stable ids an operator sets once), and this call
// turns that into "the work under way right now". A checkpoint writer calls it
// when its resolution names no work, so a session in a fresh repo starts a new
// unit of work without anybody editing a work id per task.
//
// The token is read by the caller and passed in; nothing here prints it, and the
// credential appears in no error. The gateway decides what is "current", never
// this process: the adapter cannot see other open work, and guessing locally
// would let two hosts create two works for one repo.
func EnsureWork(ctx context.Context, cfg Config, token string, timeout time.Duration) (EnsuredWork, error) {
	if cfg.Gateway.BaseURL == "" {
		return EnsuredWork{}, fmt.Errorf("ensure-work needs a configured gateway")
	}
	if cfg.ProjectID == uuid.Nil || cfg.WorkspaceID == uuid.Nil {
		return EnsuredWork{}, fmt.Errorf("ensure-work needs a resolved project and workspace")
	}
	body, _ := json.Marshal(map[string]any{
		"project_id": cfg.ProjectID, "workspace_id": cfg.WorkspaceID,
	})

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		strings.TrimRight(cfg.Gateway.BaseURL, "/")+"/memgw/v1/works/ensure", bytes.NewReader(body))
	if err != nil {
		return EnsuredWork{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return EnsuredWork{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return EnsuredWork{}, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return EnsuredWork{}, fmt.Errorf("ensure-work returned HTTP %d", resp.StatusCode)
	}
	var out EnsuredWork
	if err := json.Unmarshal(raw, &out); err != nil {
		return EnsuredWork{}, fmt.Errorf("ensure-work response is not JSON: %w", err)
	}
	if out.WorkID == uuid.Nil {
		return EnsuredWork{}, fmt.Errorf("ensure-work returned no work id")
	}
	return out, nil
}
