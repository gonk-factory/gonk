// Package catrust holds no code. Its tests pin the one mechanism the chart
// uses to make a private CA trusted WITHOUT dropping the public roots:
//
//	SSL_CERT_DIR=/etc/ssl/certs:<the directory the CA bundle is mounted in>
//
// with SSL_CERT_FILE left unset (helper gonk.caTrustEnv in
// chart/gonk/templates/_helpers.tpl). The images set no trust variable at all
// (internal/buildgate/image_trust_test.go), so this is the only one a gonk
// process ever runs under.
//
// Go reads its roots once per process (crypto/x509, root_unix.go,
// loadSystemRoots): ONE bundle file -- SSL_CERT_FILE if set, else the first of
// a fixed list that exists -- plus EVERY file in a list of directories --
// SSL_CERT_DIR split on ":" if set, else a fixed list. Each variable REPLACES
// its default. The tests run a child process per environment, because the
// roots cannot be re-read in the process that already loaded them.
package catrust
