// Package billing defines the shared vocabulary for how Patchbay classifies
// upstream charges and how a Seat determines that classification.
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

// CredentialClass classifies the credential a caller presented without
// retaining the credential itself.
type CredentialClass string

const (
	// CredentialOAuth means the caller presented an OAuth bearer token.
	CredentialOAuth CredentialClass = "oauth"

	// CredentialAPIKey means the caller presented an API key.
	CredentialAPIKey CredentialClass = "api_key"

	// CredentialNone means the caller presented no recognized credential.
	CredentialNone CredentialClass = "none"
)

// ParseCredentialClass returns the credential class named by raw, rejecting
// every value outside the closed vocabulary.
func ParseCredentialClass(raw string) (CredentialClass, error) {
	credential := CredentialClass(raw)
	if !credential.Valid() {
		return "", fmt.Errorf("credential class %q is not valid (want %q, %q or %q)",
			raw, CredentialOAuth, CredentialAPIKey, CredentialNone)
	}

	return credential, nil
}

// Valid reports whether credential is in the closed credential-class vocabulary.
func (credential CredentialClass) Valid() bool {
	switch credential {
	case CredentialOAuth, CredentialAPIKey, CredentialNone:
		return true
	default:
		return false
	}
}

// String returns the wire form of credential.
func (credential CredentialClass) String() string {
	return string(credential)
}

// Policy says how a Seat determines the billing class of a request.
type Policy string

const (
	// PolicyExplicit means the Seat declares its billing class directly.
	PolicyExplicit Policy = "explicit"

	// PolicyCallerDerived means the caller's forwarded credential determines
	// the realized billing class outside the Seat registry.
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

// RealizeClass maps a Seat's billing policy and a validated caller credential
// class to the billing class attributed to the request.
func RealizeClass(policy Policy, explicit Class, credential CredentialClass) (Class, error) {
	if !policy.Valid() {
		return "", fmt.Errorf("cannot realize billing class with invalid policy %q", policy)
	}
	if !credential.Valid() {
		return "", fmt.Errorf("cannot realize billing class with invalid credential class %q", credential)
	}

	switch policy {
	case PolicyExplicit:
		if !explicit.Valid() {
			return "", fmt.Errorf("explicit billing policy requires a valid class, got %q", explicit)
		}

		return explicit, nil
	case PolicyCallerDerived:
		if explicit != "" {
			return "", fmt.Errorf("caller-derived billing policy cannot declare explicit class %q", explicit)
		}

		switch credential {
		case CredentialOAuth:
			return ClassSubscription, nil
		case CredentialAPIKey:
			return ClassMetered, nil
		case CredentialNone:
			return "", fmt.Errorf("caller-derived billing policy requires a credential")
		}
	case PolicySigV4Metered:
		if explicit != "" {
			return "", fmt.Errorf("sigv4-metered billing policy cannot declare explicit class %q", explicit)
		}

		return ClassMetered, nil
	}

	return "", fmt.Errorf("cannot realize billing class with invalid policy %q", policy)
}
