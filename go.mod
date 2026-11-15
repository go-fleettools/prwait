module github.com/go-fleettools/prwait

// ⛔ The PATCH, because `go 1.27` names none and govulncheck judges the
// standard library by the version the module claims. At `1.27` it answered for
// 1.27.1 and reported advisories whose fix is RELEASED in 1.27.2. None is
// reachable from here -- measured, not assumed -- but the lane below could not
// have said so either way, and this same line is the toolchain that lane runs,
// through `go-version-file: go.mod`.
//
// Within the same minor, so nothing that could build this before cannot now.
//
// Raised to 1.27.3 on 2026-10-25 for GO-2026-6809 — crypto/x509, where
// compromised intermediate certificates are no longer refused by
// Certificate.Verify — fix RELEASED in 1.27.3. Same reasoning, second
// occurrence in a fortnight: this line is not a floor chosen for the language
// features it admits, it is the toolchain the security gate judges.
//
// Raised to 1.27.4 on 2026-11-15 for GO-2026-6859 — net/http, fix RELEASED in
// 1.27.4. ⛔ THIRD OCCURRENCE OF THIS EDIT IN THREE WEEKS, and the three
// paragraphs above are the record of the first two. What found it was neither a
// green check nor an alert: it was `modfloor --floor 1.27.4`, run by hand,
// reading the go directive this repository PUBLISHED (v0.9.0) from the module
// proxy — the only one of the three pins a consumer honours. Eight of the
// eleven PUBLISHED modules of this organisation read BELOW at that moment, so
// this is not a repository that fell behind, it is a maintenance act with no
// clock on it.
// The gate for that is modfloor on the nightly sweep, not a fourth paragraph
// here.
go 1.27.4
