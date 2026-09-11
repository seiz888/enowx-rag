package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/memgw/principal"
)

// The principal and credential command line.
//
// Issuing a credential is an operator act, not something a running server does
// for a caller. It happens here, deliberately, with the target database named
// in the environment and the secret printed once to the operator's terminal --
// never logged, never stored, never written into an event.
//
// The secret goes to stdout and everything else to stderr, so an operator can
// pipe it into a password manager without the surrounding prose coming with it,
// and so a redirected log file never contains it.

func runMemgwPrincipal(args []string) {
	fs := flag.NewFlagSet("memgw principal", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: enowx-rag memgw principal <command>

  create <agent-id> [--type agent|subagent|service|human|projection_worker] [--parent <principal-id>]
  grant  <principal-id> <role> --project <project-id> [--workspace <id>] [--expires <RFC3339>]
  issue  <principal-id> [--label <text>] [--expires <RFC3339>]
  list   [<principal-id>]
  revoke-credential <credential-id>
  revoke <principal-id>

Roles: history_read work_read checkpoint_write candidate_write fact_promote projection_worker admin
Environment: MEMGW_DSN (required), MEMGW_SCHEMA (default "memgw"), MEMGW_ENV (development|test).

The secret from "issue" is printed once, on stdout, and is not recoverable.
`)
	}
	// The outer level takes no flags of its own; everything after the command
	// word belongs to the subcommand, which is the only thing that knows what
	// its flags mean.
	if len(args) < 1 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fs.Usage()
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	pool, err := openMemgwPool(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memgw: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()
	store := principal.NewStore(pool)

	rest := args[1:]
	switch cmd := args[0]; cmd {
	case "create":
		err = principalCreate(ctx, store, rest)
	case "grant":
		err = principalGrant(ctx, store, rest)
	case "issue":
		err = credentialIssue(ctx, store, rest)
	case "list":
		err = principalList(ctx, store, rest)
	case "revoke-credential":
		err = credentialRevoke(ctx, store, rest)
	case "revoke":
		err = principalRevoke(ctx, store, rest)
	default:
		fmt.Fprintf(os.Stderr, "memgw principal: unknown command %q\n", cmd)
		fs.Usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "memgw principal: %v\n", err)
		os.Exit(1)
	}
}

func principalCreate(ctx context.Context, store *principal.Store, args []string) error {
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	kind := fs.String("type", "agent", "principal type")
	parent := fs.String("parent", "", "parent principal id, for a subagent")
	host := fs.String("host", "", "host id (a UUID); generated when omitted")
	name := fs.String("display-name", "", "human-readable name")
	parseArgs(fs, args)
	if fs.NArg() != 1 {
		return errors.New("create needs exactly one agent id")
	}

	p := principal.Principal{
		Type:        principal.Type(*kind),
		AgentID:     fs.Arg(0),
		DisplayName: *name,
	}
	if p.DisplayName == "" {
		p.DisplayName = p.AgentID
	}
	if *host == "" {
		p.HostID = uuid.New()
	} else {
		id, err := uuid.Parse(*host)
		if err != nil {
			return fmt.Errorf("host id: %w", err)
		}
		p.HostID = id
	}
	if *parent != "" {
		id, err := uuid.Parse(*parent)
		if err != nil {
			return fmt.Errorf("parent id: %w", err)
		}
		p.ParentID = &id
	}

	created, err := store.CreatePrincipal(ctx, p)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "principal %s created: type=%s agent_id=%s host=%s\n",
		created.ID, created.Type, created.AgentID, created.HostID)
	fmt.Println(created.ID)
	return nil
}

func principalGrant(ctx context.Context, store *principal.Store, args []string) error {
	fs := flag.NewFlagSet("grant", flag.ExitOnError)
	project := fs.String("project", "", "project id (required)")
	workspace := fs.String("workspace", "", "narrow the grant to one workspace")
	work := fs.String("work", "", "narrow the grant to one work")
	expires := fs.String("expires", "", "expiry, RFC3339")
	why := fs.String("provenance", "operator", "why this grant exists")
	parseArgs(fs, args)
	if fs.NArg() != 2 {
		return errors.New("grant needs a principal id and a role")
	}

	principalID, err := uuid.Parse(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("principal id: %w", err)
	}
	if *project == "" {
		return errors.New("a grant must name a project; there is no grant over everything")
	}
	projectID, err := uuid.Parse(*project)
	if err != nil {
		return fmt.Errorf("project id: %w", err)
	}

	g := principal.Grant{
		PrincipalID: principalID,
		Role:        principal.Role(fs.Arg(1)),
		ProjectID:   projectID,
		Provenance:  *why,
	}
	if *workspace != "" {
		id, err := uuid.Parse(*workspace)
		if err != nil {
			return fmt.Errorf("workspace id: %w", err)
		}
		g.WorkspaceID = &id
	}
	if *work != "" {
		id, err := uuid.Parse(*work)
		if err != nil {
			return fmt.Errorf("work id: %w", err)
		}
		g.WorkID = &id
	}
	if *expires != "" {
		at, err := time.Parse(time.RFC3339, *expires)
		if err != nil {
			return fmt.Errorf("expires: %w", err)
		}
		g.ExpiresAt = &at
	}

	out, err := store.GrantScope(ctx, g)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "grant %s: %s over project %s\n", out.ID, out.Role, out.ProjectID)
	fmt.Println(out.ID)
	return nil
}

func credentialIssue(ctx context.Context, store *principal.Store, args []string) error {
	fs := flag.NewFlagSet("issue", flag.ExitOnError)
	label := fs.String("label", "", "what this credential is for")
	expires := fs.String("expires", "", "expiry, RFC3339")
	parseArgs(fs, args)
	if fs.NArg() != 1 {
		return errors.New("issue needs exactly one principal id")
	}
	principalID, err := uuid.Parse(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("principal id: %w", err)
	}
	var expiresAt *time.Time
	if *expires != "" {
		at, err := time.Parse(time.RFC3339, *expires)
		if err != nil {
			return fmt.Errorf("expires: %w", err)
		}
		expiresAt = &at
	}

	c, secret, err := store.Issue(ctx, principalID, *label, expiresAt)
	if err != nil {
		return err
	}
	// The prose goes to stderr and the secret to stdout, alone on its line.
	fmt.Fprintf(os.Stderr,
		"credential %s issued for principal %s (key %s).\n"+
			"The secret below is printed once and is not stored anywhere; it cannot be recovered.\n"+
			"Put it where that host reads its credential from. Do not paste it into a session, a commit or a log.\n",
		c.ID, c.PrincipalID, c.KeyID)
	fmt.Println(secret)
	return nil
}

func principalList(ctx context.Context, store *principal.Store, args []string) error {
	if len(args) == 0 {
		return errors.New("list needs a principal id")
	}
	id, err := uuid.Parse(strings.TrimSpace(args[0]))
	if err != nil {
		return fmt.Errorf("principal id: %w", err)
	}
	p, err := store.Get(ctx, id)
	if err != nil {
		return err
	}
	fmt.Printf("principal %s  type=%s  agent_id=%s  status=%s  host=%s\n",
		p.ID, p.Type, p.AgentID, p.Status, p.HostID)

	grants, err := store.Grants(ctx, id)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "\nGRANT\tROLE\tPROJECT\tEXPIRES\tREVOKED")
	for _, g := range grants {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", g.ID, g.Role, g.ProjectID,
			orDash(g.ExpiresAt), orDash(g.RevokedAt))
	}
	credentials, err := store.Credentials(ctx, id)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "\nCREDENTIAL\tKEY\tLABEL\tISSUED\tEXPIRES\tREVOKED")
	for _, c := range credentials {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", c.ID, c.KeyID, c.Label,
			c.IssuedAt.Format(time.RFC3339), orDash(c.ExpiresAt), orDash(c.RevokedAt))
	}
	return w.Flush()
}

func credentialRevoke(ctx context.Context, store *principal.Store, args []string) error {
	if len(args) != 1 {
		return errors.New("revoke-credential needs exactly one credential id")
	}
	id, err := uuid.Parse(args[0])
	if err != nil {
		return fmt.Errorf("credential id: %w", err)
	}
	if err := store.RevokeCredential(ctx, id); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "credential %s revoked\n", id)
	return nil
}

func principalRevoke(ctx context.Context, store *principal.Store, args []string) error {
	if len(args) != 1 {
		return errors.New("revoke needs exactly one principal id")
	}
	id, err := uuid.Parse(args[0])
	if err != nil {
		return fmt.Errorf("principal id: %w", err)
	}
	if err := store.Revoke(ctx, id); err != nil {
		return err
	}
	// Revoking the principal ends every credential it holds, which is the
	// point: an incident response that had to enumerate credentials would
	// eventually miss one.
	fmt.Fprintf(os.Stderr, "principal %s revoked; every credential it holds no longer authenticates\n", id)
	return nil
}

func orDash(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Format(time.RFC3339)
}

// parseArgs parses flags that may appear after the positional arguments.
//
// Go's flag package stops at the first non-flag word, which would make
// "create claude-code --type agent" silently drop the type. Rather than
// insisting operators write flags first -- a rule nobody remembers and whose
// violation fails confusingly -- the flags are lifted to the front here, using
// the flag set itself to tell which of them consume the next word.
func parseArgs(fs *flag.FlagSet, args []string) {
	boolFlag := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) {
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
			boolFlag[f.Name] = true
		}
	})

	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") || boolFlag[name] {
			continue
		}
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	_ = fs.Parse(append(flags, positional...))
}
