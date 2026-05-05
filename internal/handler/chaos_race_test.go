//go:build race

package handler

// chaosRaceBuild is set at compile time via build tags so the chaos test
// can relax its p99 threshold under -race (the race detector adds ~3-5×
// overhead, which would push otherwise-fast cache HITs over the 100ms
// SPEC §12.3 bound for reasons unrelated to the bug under test).
const chaosRaceBuild = true
