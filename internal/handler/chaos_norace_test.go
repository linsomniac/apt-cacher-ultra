//go:build !race

package handler

// chaosRaceBuild is the !race counterpart of chaos_race_test.go's
// constant. See that file for why the chaos test gates its threshold on
// build tag.
const chaosRaceBuild = false
