package domain

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A git remote is a normal place to find a token, and a registry that stored
// the URL verbatim would be a credential store nobody meant to build. The
// planted values below are not real credentials; what is real is the shape.
func TestNormaliseRepoKeyStripsCredentials(t *testing.T) {
	planted := strings.Repeat("A", 32)
	cases := map[string]string{
		"https://github.com/enowdev/enowx-rag.git":                  "github.com/enowdev/enowx-rag",
		"https://x-access-token:" + planted + "@github.com/a/b.git": "github.com/a/b",
		"git@github.com:enowdev/enowx-rag.git":                      "github.com/enowdev/enowx-rag",
		"ssh://git@gitlab.example.com:2222/group/proj.git":          "gitlab.example.com:2222/group/proj",
		"HTTPS://GitHub.com/Enowdev/Enowx-Rag":                      "github.com/enowdev/enowx-rag",
		"https://github.com/enowdev/enowx-rag/":                     "github.com/enowdev/enowx-rag",
	}
	for in, want := range cases {
		got, err := NormaliseRepoKey(in)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("%s -> %q, want %q", in, got, want)
		}
		if strings.Contains(got, planted) || strings.Contains(got, "@") {
			t.Errorf("%s -> %q still carries userinfo", in, got)
		}
	}
}

// A remote that cannot be reduced to host/path is refused rather than stored
// half-parsed: a key that means nothing would map a project to nothing.
func TestNormaliseRepoKeyRefusesWhatItCannotReduce(t *testing.T) {
	for _, in := range []string{"", "   ", "not a remote", "file:///d:/projects/thing", "https://github.com"} {
		if got, err := NormaliseRepoKey(in); err == nil {
			t.Errorf("%q was accepted as %q", in, got)
		}
	}
}

// The workspace root is identified without being stored: a path contains a user
// name, and the registry is read by every host that shares the project.
func TestRootPathDigestIsStableAcrossPathSpelling(t *testing.T) {
	a := RootPathDigest(`D:\PROJECTS\enowx-rag`)
	b := RootPathDigest(`d:/projects/enowx-rag/`)
	if a != b {
		t.Fatalf("the same root digests differently: %s vs %s", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("digest is %d chars", len(a))
	}
	if RootPathDigest(`D:\PROJECTS\other`) == a {
		t.Fatal("two different roots share a digest")
	}
}

// Slot ids are derived, not allocated: two hosts that promote into the same
// predicate must contend on the same aggregate without agreeing on an id first.
func TestSlotIDIsDeterministicAndProjectScoped(t *testing.T) {
	p1 := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	p2 := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	if SlotID(p1, "enowx-rag", "deployment_target") != SlotID(p1, "enowx-rag", "deployment_target") {
		t.Fatal("slot ids are not deterministic")
	}
	if SlotID(p1, "enowx-rag", "deployment_target") == SlotID(p2, "enowx-rag", "deployment_target") {
		t.Fatal("two projects share a slot id")
	}
	// The separator matters: without it, subject "a" predicate "bc" and
	// subject "ab" predicate "c" would be the same predicate.
	if SlotID(p1, "a", "bc") == SlotID(p1, "ab", "c") {
		t.Fatal("subject and predicate are not separated")
	}
}

// The transition table is the whole state machine: absence is a refusal, and
// the terminal states have no way out.
func TestTransitionTableFailsClosed(t *testing.T) {
	for _, terminal := range []string{"completed", "abandoned"} {
		if len(Transitions[terminal]) != 0 {
			t.Errorf("%s is not terminal", terminal)
		}
	}
	if canTransition("active", "active") {
		t.Error("active -> active is not a transition")
	}
	if canTransition("planned", "completed") {
		t.Error("work cannot be completed without ever being active")
	}
	if canTransition("nonsense", "active") {
		t.Error("an unknown state must reach nothing")
	}
	// Every state a transition leads to must itself be in the table, or the
	// machine can reach a state with no defined exits by accident.
	for from, tos := range Transitions {
		for _, to := range tos {
			if _, ok := Transitions[to]; !ok {
				t.Errorf("%s -> %s leads outside the table", from, to)
			}
		}
	}
}
