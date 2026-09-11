package pgstore

import (
	"strings"
	"testing"
)

// TestRedactDSNDropsCredentials is the test that matters most in this file:
// every connection error in this package is formatted through RedactDSN, so a
// leak here is a password in a log line.
func TestRedactDSNDropsCredentials(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
		want string
	}{
		{"url form", "postgres://alice:hunter2@127.0.0.1:55433/memgw_test?sslmode=disable", "127.0.0.1:55433/memgw_test"},
		{"url without password", "postgres://127.0.0.1:5432/dev_ledger", "127.0.0.1:5432/dev_ledger"},
		{"url without database", "postgres://bob:s3cret@localhost:5432/", "localhost:5432/(default)"},
		{"keyword form", "host=127.0.0.1 port=55433 user=postgres password=hunter2 dbname=memgw_test", "127.0.0.1/memgw_test"},
		{"empty", "", "(empty)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactDSN(tc.dsn)
			if got != tc.want {
				t.Fatalf("RedactDSN = %q, want %q", got, tc.want)
			}
			for _, secret := range []string{"hunter2", "s3cret", "password="} {
				if strings.Contains(got, secret) {
					t.Fatalf("RedactDSN leaked %q in %q", secret, got)
				}
			}
		})
	}
}

// TestDisposableDatabaseNames pins the naming rule. It is written as a table of
// real names from this machine on purpose: "enowx" and "axonhub" are the two
// databases a mistyped DSN would most plausibly reach.
func TestDisposableDatabaseNames(t *testing.T) {
	accepted := []string{"memgw_test", "memgw_dev", "test_ledger", "dev_ledger", "enowx_test", "enowx_devdb", "axonhub_test"}
	refused := []string{"enowx", "axonhub", "postgres", "rag", "enowx_rag", "production", "memgw", "testing"}

	for _, name := range accepted {
		if !nonProductionDatabase.MatchString(name) {
			t.Errorf("database %q should be accepted as disposable", name)
		}
	}
	for _, name := range refused {
		if nonProductionDatabase.MatchString(name) {
			t.Errorf("database %q must not be accepted as disposable", name)
		}
	}
}

func TestLoopbackTarget(t *testing.T) {
	cases := []struct {
		dsn        string
		serverAddr string
		want       bool
	}{
		{"postgres://postgres@127.0.0.1:55433/memgw_test", "172.17.0.2", true},
		{"postgres://postgres@localhost:55433/memgw_test", "local", true},
		{"postgres://postgres@[::1]:5432/memgw_test", "::1", true},
		{"host=127.0.0.1 port=55433 dbname=memgw_test", "local", true},
		{"postgres://postgres@10.0.0.7:5432/memgw_test", "10.0.0.7", false},
		{"postgres://postgres@db.internal:5432/memgw_test", "10.0.0.7", false},
		{"host=db.internal dbname=memgw_test", "10.0.0.7", false},
	}
	for _, tc := range cases {
		if got := isLoopbackTarget(tc.dsn, tc.serverAddr); got != tc.want {
			t.Errorf("isLoopbackTarget(%q, %q) = %v, want %v", RedactDSN(tc.dsn), tc.serverAddr, got, tc.want)
		}
	}
}

// TestOpenRefusesEmptyDSN keeps the failure explicit. An empty DSN previously
// meant "whatever libpq guesses from the environment", which on a developer
// machine is a real database.
func TestOpenRefusesEmptyDSN(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Env = EnvTest
	if _, err := Open(t.Context(), cfg); err == nil {
		t.Fatal("Open with an empty DSN should fail")
	}
}

func TestQuoteIdent(t *testing.T) {
	if got := QuoteIdent(`memgw`); got != `"memgw"` {
		t.Fatalf("QuoteIdent = %s", got)
	}
	if got := QuoteIdent(`we"ird`); got != `"we""ird"` {
		t.Fatalf("QuoteIdent did not double the quote: %s", got)
	}
}
