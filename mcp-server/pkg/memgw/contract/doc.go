// Package contract holds the frozen shared-memory-gateway contract and the
// fixtures that pin it down.
//
// There is deliberately no implementation here. Phase 2 freezes what the ledger
// must do; the ledger itself does not exist yet. The contract lives in
// testdata/contract.json and the scenarios in testdata/fixtures/*.json rather
// than in Go types, because the adapters that have to honour the same contract
// are written in TypeScript (OMP, OpenCode), Python (Hermes, Claude Code hooks)
// and Go. A contract encoded in Go structs would only ever be enforceable in one
// of the three.
//
// contract_test.go validates the fixtures against the frozen contract. It checks
// the observable contract -- envelope completeness, known event types and error
// classes, bounds, digest relations, invariant coverage -- and nothing about how
// a server might implement it.
//
// See docs/architecture/shared-memory-gateway.md for the prose contract and
// docs/adr/0001-shared-memory-authority.md for why PostgreSQL is the authority.
package contract
