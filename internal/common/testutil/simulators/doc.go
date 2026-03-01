// Package simulators provides helper functions that simulate the behaviour of
// Kubernetes controllers and operators that are not running during envtest-based
// integration tests.
//
// In a real cluster, custom operators (e.g. MariaDB, Memcached, external-secrets)
// watch their CRDs and update status sub-resources accordingly.  In envtest only
// the API server is present, so these simulators stand in for those absent
// controllers by directly creating resources and patching status fields to the
// expected terminal state.
package simulators
