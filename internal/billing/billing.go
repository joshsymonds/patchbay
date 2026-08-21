// Package billing defines the shared vocabulary for how Patchbay classifies
// upstream charges and how a route determines that classification.
package billing

import "fmt"

// Class is the realized billing class attributed to an upstream request.
type Class string

const (
	// ClassSubscription means the request consumes a fixed-price subscription.
	ClassSubscription Class = "subscription"

	// ClassMetered means the request is charged by usage.
	ClassMetered Class = "metered"

	// ClassSelfHosted means the request runs on operator-owned infrastructure.
	ClassSelfHosted Class = "self_hosted"
)

// ParseClass returns the billing class named by raw, rejecting every value
// outside the closed vocabulary.
func ParseClass(raw string) (Class, error) {
	class := Class(raw)
	if !class.Valid() {
		return "", fmt.Errorf("billing class %q is not valid (want %q, %q or %q)",
			raw, ClassSubscription, ClassMetered, ClassSelfHosted)
	}

	return class, nil
}

// Valid reports whether class is in the closed billing-class vocabulary.
func (class Class) Valid() bool {
	switch class {
	case ClassSubscription, ClassMetered, ClassSelfHosted:
		return true
	default:
		return false
	}
}

// String returns the wire form of class.
func (class Class) String() string {
	return string(class)
}

// Policy says how a route determines the billing class of a request.
type Policy string

const (
	// PolicyExplicit means the route declares its billing class directly.
	PolicyExplicit Policy = "explicit"

	// PolicyCallerDerived means the caller's forwarded credential determines
	// the realized billing class outside the route registry.
	PolicyCallerDerived Policy = "credential_derived"

	// PolicySigV4Metered means AWS SigV4 requests are metered by the signed AWS
	// account.
	PolicySigV4Metered Policy = "sigv4_metered"
)

// ParsePolicy returns the billing policy named by raw, rejecting every value
// outside the closed vocabulary.
func ParsePolicy(raw string) (Policy, error) {
	policy := Policy(raw)
	if !policy.Valid() {
		return "", fmt.Errorf("billing policy %q is not valid (want %q, %q or %q)",
			raw, PolicyExplicit, PolicyCallerDerived, PolicySigV4Metered)
	}

	return policy, nil
}

// Valid reports whether policy is in the closed billing-policy vocabulary.
func (policy Policy) Valid() bool {
	switch policy {
	case PolicyExplicit, PolicyCallerDerived, PolicySigV4Metered:
		return true
	default:
		return false
	}
}

// String returns the wire form of policy.
func (policy Policy) String() string {
	return string(policy)
}
