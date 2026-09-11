package adapter

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"
)

// Config is what an adapter needs to know that a hook cannot tell it.
//
// It holds no secret. The gateway credential is named by a path, never by a
// value, so the file can be read by anybody debugging a hook, checked into a
// dotfiles repository by somebody who should not have, or pasted into an issue,
// without any of those being a credential incident. The one thing that reads
// the token is the process that submits, and it reads it from a file whose
// permissions are the operator's to set.
type Config struct {
	// ProjectID and WorkspaceID say where events land. They are ids and not
	// slugs because a folder rename must not silently start a second project.
	ProjectID   uuid.UUID `json:"project_id"`
	WorkspaceID uuid.UUID `json:"workspace_id"`

	// Resolve, when set, replaces the fixed ProjectID/WorkspaceID/WorkID with a
	// per-working-directory mapping. The two are alternatives: a config may name
	// fixed ids (single-purpose host) or a resolution (ordinary workstation),
	// never both, and must name at least one.
	Resolve Resolution `json:"resolve,omitempty"`

	// WriterEpoch is the epoch this host was admitted under. It is configured
	// rather than discovered because an adapter must keep working while the
	// gateway is unreachable, and an epoch it could not fetch would mean no
	// events at all. A stale epoch is refused by the gateway as
	// writer_epoch_fenced, which is the intended outcome: a host that was
	// fenced finds out at its next write instead of continuing to write.
	WriterEpoch int64 `json:"writer_epoch"`

	// Sensitivity is the class stamped on every event this adapter submits.
	// It defaults to internal. It is not per-event: a hook has no way to judge
	// the sensitivity of a session, and an adapter that guessed would
	// eventually guess "public" about something that was not.
	Sensitivity string `json:"sensitivity_class,omitempty"`

	// Collector is the local durable queue. When it is configured, everything
	// goes there and nothing goes anywhere else: the whole point of the
	// collector is that a hook returns as soon as the event is on disk.
	Collector CollectorConfig `json:"collector,omitempty"`

	// Work is the unit of work the checkpoints on this workstation belong to.
	//
	// It is configured rather than discovered because it is the thing the two
	// hosts have to agree on: a handover only works if the session that writes
	// the checkpoint and the session that reads it name the same work. A
	// work_id derived per host from a path or a branch name would differ the
	// first time one of them was opened from a different directory, and the
	// symptom would be an empty handover rather than an error.
	Work WorkConfig `json:"work,omitempty"`

	// Gateway is the fallback for hosts with no local collector. Submitting
	// straight to the gateway means the event is durable only if the gateway
	// answered, so a hook on such a host loses events while the gateway is
	// down. That is a real gap and is reported as one rather than papered over.
	// The collector exists for Windows and Linux; a host on either that is
	// configured gateway-direct has chosen that gap, it is not forced on it.
	Gateway GatewayConfig `json:"gateway,omitempty"`
}

// CollectorConfig names the local collector.
//
// Two keys for one thing, because the thing has two names. "pipe" is what every
// Windows configuration written before the Linux collector existed says, and
// silently ignoring it would disable durability on the hosts that have it
// today; "endpoint" is what a unix socket path should be called. Endpoint wins
// when both are set, and Address is the only thing that reads either.
type CollectorConfig struct {
	Pipe     string `json:"pipe,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
}

// Address is the local endpoint to connect to, or "" when no collector is
// configured.
func (c CollectorConfig) Address() string {
	if c.Endpoint != "" {
		return c.Endpoint
	}
	return c.Pipe
}

// WorkConfig names the shared unit of work.
//
// Title is used only when the work has to be created; once it exists the
// ledger's copy stands, because a second host with a different title in its
// configuration must not rename work that is already under way.
type WorkConfig struct {
	WorkID uuid.UUID `json:"work_id"`
	Title  string    `json:"title,omitempty"`
}

// GatewayConfig names the gateway and where its credential lives.
type GatewayConfig struct {
	BaseURL string `json:"base_url,omitempty"`
	// TokenPath is a path, never a token. See the Config comment.
	TokenPath string `json:"token_path,omitempty"`
}

// ConfigEnv is where the adapter looks when no --config was given. Hooks are
// invoked by the host with a command line the host owns, so an environment
// variable is often the only place an operator can put this.
const ConfigEnv = "MEMGW_ADAPTER_CONFIG"

// LoadConfig reads and checks an adapter configuration.
func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("adapter: the configuration could not be read: %w", err)
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("adapter: %s is not a usable configuration: %w", path, err)
	}
	if err := c.check(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c *Config) check() error {
	// Exactly one of the two ways to name a target must be present: fixed ids
	// (single-purpose host) or a resolution (ordinary workstation). Both would
	// mean the resolution is dead weight, and neither means events have nowhere
	// to land.
	hasFixed := c.ProjectID != uuid.Nil || c.WorkspaceID != uuid.Nil
	hasResolve := c.Resolve.MapPath != "" || c.Resolve.GitRemote
	if hasFixed && hasResolve {
		return fmt.Errorf("adapter: the configuration names fixed ids and a resolve block; choose one")
	}
	if !hasFixed && !hasResolve {
		return fmt.Errorf("adapter: the configuration must name a project_id/workspace_id or a resolve block")
	}
	if hasFixed && (c.ProjectID == uuid.Nil || c.WorkspaceID == uuid.Nil) {
		return fmt.Errorf("adapter: the configuration must name a project_id and a workspace_id")
	}
	if hasResolve && c.Resolve.GitRemote && c.Resolve.HostID == uuid.Nil {
		return fmt.Errorf("adapter: git-remote resolution needs a host_id")
	}
	if c.WriterEpoch <= 0 {
		return fmt.Errorf("adapter: the configuration must name the writer_epoch this host was admitted under")
	}
	if c.Sensitivity == "" {
		c.Sensitivity = "internal"
	}
	switch c.Sensitivity {
	case "public", "internal", "confidential", "restricted":
	default:
		return fmt.Errorf("adapter: %q is not a sensitivity class", c.Sensitivity)
	}
	if c.Collector.Address() == "" && c.Gateway.BaseURL == "" {
		return fmt.Errorf("adapter: the configuration names neither a collector endpoint nor a gateway")
	}
	if c.Gateway.BaseURL != "" && c.Gateway.TokenPath == "" {
		return fmt.Errorf("adapter: a gateway needs a token_path; this configuration may not hold the token itself")
	}
	// A token sitting in the configuration is the mistake this shape exists to
	// prevent, so it is refused by name rather than ignored.
	return nil
}

// ResolveForCwd returns a copy of c with ProjectID, WorkspaceID and Work.WorkID
// named by the resolution for cwd. When c has no resolve block it returns c
// unchanged (the fixed ids stand). The resolved WorkID is not applied to a
// checkpoint: a checkpoint names its own work, and the producer sets it.
func (c *Config) ResolveForCwd(cwd string) (Config, error) {
	if c.Resolve.MapPath == "" && !c.Resolve.GitRemote {
		return *c, nil
	}
	r, err := ResolveTarget(cwd, c.Resolve)
	if err != nil {
		return *c, err
	}
	out := *c
	out.ProjectID = r.ProjectID
	out.WorkspaceID = r.WorkspaceID
	if r.WorkID != uuid.Nil {
		out.Work = WorkConfig{WorkID: r.WorkID}
	}
	return out, nil
}

// ReadToken reads a credential from a file, trimming the trailing newline an
// editor adds. Nothing here logs it, echoes it, or puts it in an error.
func ReadToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("adapter: the gateway credential could not be read from %s: %w", path, err)
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return "", fmt.Errorf("adapter: %s holds no credential", path)
	}
	return tok, nil
}
