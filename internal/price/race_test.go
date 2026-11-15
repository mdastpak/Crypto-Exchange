//go:build race

package price

// raceEnabled is whether the tests run under the race detector, which
// slows the code down 5 to 10 times (go.dev/doc/articles/race_detector).
const raceEnabled = true
