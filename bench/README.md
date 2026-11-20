# Native amd64 A/B runner — publication disabled

Baseline: `75ddd531a113fb5e5dfc745594e74d280c20e2d1`.

This branch adds only benchmark tooling. It does not change runtime code, update main, submit entries, call model providers, upload transcripts or change final_pick. Workflow permissions are read-only. The workflow is restricted to the public repository, the bench-amd64 branch and the standard ubuntu-latest runner; no larger runner or artifact storage is used. Evidence is emitted to job logs.

On an initial push, candidate equals baseline and the run is labeled A/A calibration only. A real candidate must name an exact separate commit and must be chosen from measured evidence; no candidate is supplied while the server workload breakdown is unavailable.

The runner downloads one official kit, verifies its complete manifest, stages both exact source commits, builds native Linux amd64, runs Go tests/vet, and runs sequential A1/B1/B2/A2/A3/B3 grades. Each grade has three instances and requires 120/120 checks, three replay matches and byte-for-byte L1 regrade. The comparison checks state export hashes across matched instances, every paired composite latency and all workload regressions. Identical exported states prove check-outcome parity, not arbitrary HTTP equivalence.

The runner always leaves promotion_allowed=false: actual post-cutoff official preflight with genuine native build history, thinking_own_work and authorship must be independently verified before any owner-approved publication. A local checker skip is not a pass. No transcript alterations or synthetic build records are permitted.

Preparation validation on Termux: gate unit tests, CLI help, workflow YAML parse and parsing of an existing real three-instance grade. These are not Linux benchmark execution. Actual Actions execution is blocked until the GitHub token has the workflow scope; the available token currently has public_repo only. No GitHub workflow push or remote main change was attempted.
