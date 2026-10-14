//go:build race

package fedarisha

// raceDetectorEnabled reports that this binary was built with -race.
//
// It gates the pool-leak measurements in udp_limits_test.go: the race runtime
// effectively disables sync.Pool, so a buffer that IS handed back still costs a
// fresh allocation and "this batch allocated a lot" stops meaning "this batch
// leaked". The measurements run in ordinary builds.
const raceDetectorEnabled = true
