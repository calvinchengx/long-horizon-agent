// Package safety is the safety boundary, enforced in code below the model (never via a prompt):
// the irreversible / outward-facing command classifier (ClassifyCommand), default-deny network
// egress (EgressPolicy, ParseURL, NormalizeHost, CheckResolvedAddresses), the CredentialBroker
// and Meta's Rule of Two.
//
// It is the Go port of python/src/lha/safety and matches it exactly: reason and error strings are
// byte-identical, and the Python string semantics the checks rely on (str.lower, the re module's
// Unicode classes, shlex, urllib.parse.urlsplit, ipaddress, the idna package and the stdlib idna
// codec) are reproduced, partly from tables generated out of the reference's own data
// (pystr/tables.go, idna_tables.go, idna_overrides.go). spec/safety pins the behaviour.
package safety
