module github.com/go-fleettools/prwait

// ⛔ The PATCH, because `go 1.27` names none and govulncheck judges the
// standard library by the version the module claims. At `1.27` it answered for
// 1.27.1 and reported advisories whose fix is RELEASED in 1.27.2. None is
// reachable from here -- measured, not assumed -- but the lane below could not
// have said so either way, and this same line is the toolchain that lane runs,
// through `go-version-file: go.mod`.
//
// Within the same minor, so nothing that could build this before cannot now.
go 1.27.2
