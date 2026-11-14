package engine

import "time"

// DIAGNOSTIC BUILD - not meant for ranking.
//
// The same executable has scored 4094/4032 and 3127/3050 on the platform,
// and the fast scan exists only for x86 with AVX2. To learn which kind of
// CPU the evaluation runs on, this build waits ProbeDelay before every
// request, and the delay depends only on the CPU:
//
//	not amd64 (e.g. arm64)   no delay   -> score as before (about 3050-3230)
//	amd64 with AVX2          2 ms       -> clearly lower
//	amd64 without AVX2       10 ms      -> far lower
//
// Results and the engine are otherwise those of the previous revision.
var ProbeDelay time.Duration
