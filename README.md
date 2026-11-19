# github-core-clone-forge

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/fenel-git/github-core-clone-forge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/fenel-git/github-core-clone-forge/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A standard-library Go, in-memory GitHub-compatible forge with native Git Smart HTTP, REST v3, GraphQL v4, and six MCP tools. The runtime never invokes a Git executable.

Read-only issue requests and Git upload-pack requests share an RWMutex. Issue JSON bytes are cached and invalidated under the mutation lock; new object compression uses zlib BestSpeed with per-object cache synchronization. IDs, timestamps and Git objects retain lite-v2 semantics.

The committed deployment executable is static Linux amd64 (`CGO_ENABLED=0`, `-trimpath`, `-ldflags='-s -w -buildid='`) and below 7.8 MiB. The ARM Termux executable is kept outside the public repository.

## Verification

The optimized Go source passed the unmodified official process-driver grade with three instances: 120/120 checks each, replay 3/3, and canonical state exports byte-identical to lite-v2. The export records check outcomes, not the complete internal repository state or raw HTTP/pack bytes. Independent HTTP parity tests passed 87/87; BestSpeed deliberately changes newly compressed pack encodings without changing decoded Git objects.

Local performance measured 68.33/200 (868.33 total), versus 69.33/200 for the prior Go build and 49.67/200 for the freshly reproduced TypeScript baseline. This hot-path round did not establish an additional speedup. These are unisolated Termux process measurements, not production scores or an amd64 execution test.

Full clone v0/v2 and shallow clone v2 are supported by interoperability tests. Forced v0 shallow clone retains lite-v2's known `expected shallow list` limitation; the official Git checks remain 30/30.

## Build

Only Go's standard library is needed. `python build.py` runs tests and builds both targets, enforcing the 7.8 MiB per-binary cap. The local ARM build uses the Termux host's actual page alignment. `forge-local` is gitignored and must not be committed; keep a development copy outside the public repository. For an ARM trial staging directory, copy that local executable beside `main.ts` before testing.

The public deployment path uses `forge-linux-amd64`; `main.ts` propagates the official environment, standard streams, signals and exit status. No dependency installation is required at runtime.

## Run it

```sh
node main.ts  # In a make.fun trial; MAKEFUN_ENV_URL and MAKEFUN_SHOP_TOKEN are injected.
```

---

<sub>Entered on [make.fun](https://make.fun/?ref=mgwt61fx&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
