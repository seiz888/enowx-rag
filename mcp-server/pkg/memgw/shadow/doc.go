// Package shadow runs the frozen-dataset evaluation.
//
// Shadow evaluation is an observation, never a gate. Nothing here can block a
// write, delay a rebuild or change what the ledger accepts; it reads a frozen
// dataset, runs it, and writes a report. If this package were deleted the
// gateway would behave identically.
//
// Two lanes, kept apart on purpose:
//
//   - Deterministic correctness. Given this input, the code must produce
//     exactly this decision. There is no threshold to argue about and no
//     sampling error: a case either matches or it does not, and one mismatch is
//     a failure. These cases run anywhere, with no network and no provider.
//
//   - Retrieval quality. Given this query, how much of what should have come
//     back did. This is a measurement, it needs an embedding provider and a
//     corpus, and its numbers are only comparable between runs over the same
//     frozen dataset with the same provider.
//
// Reporting them as one number would be dishonest in both directions: it would
// let a retrieval regression hide behind passing correctness cases, and it
// would let a correctness bug look like a tuning problem.
//
// The dataset and the thresholds are frozen together and digested together. A
// threshold chosen after seeing the result is not a threshold, so the digest
// covers the cases and the numbers they will be judged against, and a run
// against an edited dataset is refused rather than reported.
//
// A lane that cannot run is reported as blocked, with the reason. It is never
// reported as passing, never reported as zero, and no number is ever produced
// for a measurement that did not happen. An evaluation whose retrieval lane is
// blocked yields a report whose verdict is "incomplete" -- which is the honest
// answer, and the one that keeps somebody from citing it as evidence that
// retrieval was checked.
package shadow
