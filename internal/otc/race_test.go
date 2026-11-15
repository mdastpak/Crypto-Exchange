//go:build race

package otc

// raceEnabled reports a build with the race detector, which slows the
// code several times: the load test has a larger budget there.
const raceEnabled = true
