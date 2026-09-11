package graphify

import (
	"path"
	"strings"
)

// The exclusion rules.
//
// These are applied to the walk, not to the output. A path that is excluded is
// never opened, never hashed and never named in a manifest -- the point is that
// the bytes do not enter this process, because a secret that has been read into
// a buffer is a secret that can end up in a log line, a panic, or a crash dump.
//
// The rules are deliberately blunt and deliberately not configurable from a
// project file. A repository that could widen them would be a repository that
// could ask to have its own .env indexed.

// excludedDirs are directory names that are skipped wherever they appear.
var excludedDirs = map[string]bool{
	".git":         true,
	".svn":         true,
	".hg":          true,
	"node_modules": true,
	"vendor":       true,
	".venv":        true,
	"venv":         true,
	"__pycache__":  true,
	".idea":        true,
	".vscode":      true,
	"dist":         true,
	"build":        true,
	"target":       true,
	".next":        true,
	".cache":       true,
	// Secret material lives in directories with these names often enough that
	// skipping them wholesale costs nothing and removes a whole class of leak.
	"secrets":     true,
	".secrets":    true,
	"credentials": true,
	".ssh":        true,
	".gnupg":      true,
	".aws":        true,
	".kube":       true,
}

// excludedNames are exact file names never indexed.
var excludedNames = map[string]bool{
	".env":              true,
	".env.local":        true,
	".envrc":            true,
	".netrc":            true,
	"_netrc":            true,
	".npmrc":            true,
	".pypirc":           true,
	".htpasswd":         true,
	"id_rsa":            true,
	"id_dsa":            true,
	"id_ecdsa":          true,
	"id_ed25519":        true,
	"credentials.json":  true,
	"service-account":   true,
	"terraform.tfvars":  true,
	"kubeconfig":        true,
	".pgpass":           true,
	".git-credentials":  true,
	"authorized_keys":   true,
	"known_hosts":       true,
	"secring.gpg":       true,
	"master.key":        true,
	"credentials.yml":   true,
	"credentials.yaml":  true,
	"secrets.yml":       true,
	"secrets.yaml":      true,
	"appsettings.json":  true,
	"local.settings.js": true,
}

// excludedSuffixes are extensions never indexed. Key material and archives:
// the first because it is secret, the second because an archive's contents are
// not visible to a walk and indexing the container tells a reader nothing.
var excludedSuffixes = []string{
	".pem", ".key", ".p12", ".pfx", ".jks", ".keystore", ".asc", ".gpg", ".kdbx",
	".crt", ".cer", ".der",
	".zip", ".tar", ".gz", ".tgz", ".7z", ".rar", ".xz", ".bz2",
	".exe", ".dll", ".so", ".dylib", ".bin", ".obj", ".o", ".a", ".lib", ".pdb",
	".sqlite", ".sqlite3", ".db", ".db-wal", ".db-shm",
	".mp4", ".mov", ".avi", ".mkv", ".iso", ".vhd", ".vhdx", ".vmdk",
}

// excludedPrefixes catch the ".env.production" family, where the interesting
// part of the name is at the front.
var excludedPrefixes = []string{
	".env.", "id_rsa.", "id_ed25519.",
}

// excludedFragments are substrings that make a file suspicious enough to skip.
// This is the rule most likely to exclude something harmless, and that is the
// intended direction of error: a missing entry in a code index is a gap, an
// indexed credential is an incident.
var excludedFragments = []string{
	"secret", "password", "passwd", "token", "apikey", "api_key",
	"private_key", "privatekey", "credential",
}

// ExcludedDir reports whether a directory is skipped entirely.
func ExcludedDir(name string) bool {
	return excludedDirs[strings.ToLower(name)]
}

// ExcludedFile reports whether a file is skipped. rel is the repository-
// relative slash-separated path.
func ExcludedFile(rel string) bool {
	name := strings.ToLower(path.Base(rel))
	if excludedNames[name] {
		return true
	}
	for _, p := range excludedPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	for _, s := range excludedSuffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	// A lock file is not a secret and a "secrets.md" describing the policy is
	// not either, but distinguishing them requires reading the file, which is
	// the thing being avoided. The fragments rule wins.
	for _, f := range excludedFragments {
		if strings.Contains(name, f) {
			return true
		}
	}
	return false
}
