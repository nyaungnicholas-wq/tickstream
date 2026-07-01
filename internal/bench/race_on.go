//go:build race

package bench

// raceEnabled reports whether this binary was built with -race. Recorded
// benchmark numbers MUST come from a non-race build; Validate enforces it.
const raceEnabled = true
