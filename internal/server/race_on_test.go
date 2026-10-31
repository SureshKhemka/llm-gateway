//go:build race

package server_test

// raceEnabled skips wall-clock assertions: the race detector slows code 10–20×.
const raceEnabled = true
