package acp

import (
	"errors"
	"fmt"
)

// authenticationMethods owns the identifier namespace exchanged during
// initialize. The peer selects by ID alone, so duplicates and typed nil arms
// would make the advertised flow ambiguous or panic only after the handshake.
type authenticationMethods []AuthMethod

func newAuthenticationMethods(
	methods []AuthMethod,
	hasAgentHandler bool,
) (authenticationMethods, error) {
	owned, err := ownAuthenticationMethods(methods)
	if err != nil {
		return nil, fmt.Errorf("acp: AgentConfig.AuthMethods: %w", err)
	}
	for _, method := range owned {
		advertised, _ := authenticationMethod(method)
		if advertised.agentHandled && !hasAgentHandler {
			return nil, fmt.Errorf(
				"acp: authentication method %q requires AgentConfig.Authenticate", advertised.id)
		}
	}
	return owned, nil
}

func ownAuthenticationMethods(methods []AuthMethod) (authenticationMethods, error) {
	owned := authenticationMethods(deepCopy(methods))
	seen := make(map[AuthMethodID]struct{}, len(owned))
	for index, method := range owned {
		advertised, known := authenticationMethod(method)
		if !known {
			return nil, fmt.Errorf("authentication method %d is nil or has an unknown form", index)
		}
		if _, duplicate := seen[advertised.id]; duplicate {
			return nil, fmt.Errorf(
				"two authentication methods share the identifier %q", advertised.id)
		}
		seen[advertised.id] = struct{}{}
	}
	return owned, nil
}

// An advertisedAuth is what this package needs from one entry in authMethods: the
// identifier a client names, and whether this agent performs the method by
// answering authenticate or by being run again in a terminal.
//
// One value rather than two returned booleans, which a caller binds by position
// and cannot be told apart by the compiler.
type advertisedAuth struct {
	id           AuthMethodID
	agentHandled bool
}

func authenticationMethod(method AuthMethod) (advertisedAuth, bool) {
	switch method := method.(type) {
	case *AuthMethodAgent:
		if method == nil {
			return advertisedAuth{}, false
		}
		return advertisedAuth{id: method.ID, agentHandled: true}, true
	case *AuthMethodTerminal:
		if method == nil {
			return advertisedAuth{}, false
		}
		return advertisedAuth{id: method.ID}, true
	default:
		return advertisedAuth{}, false
	}
}

func (m authenticationMethods) offered(client ClientCapabilities) []AuthMethod {
	offered := make([]AuthMethod, 0, len(m))
	for _, method := range m {
		if _, terminal := method.(*AuthMethodTerminal); terminal && !client.Auth.Terminal {
			continue
		}
		offered = append(offered, deepCopy(method))
	}
	if len(offered) == 0 {
		return nil
	}
	return offered
}

func (m authenticationMethods) validateOffer(client ClientCapabilities) error {
	if client.Auth.Terminal {
		return nil
	}
	for _, method := range m {
		if _, terminal := method.(*AuthMethodTerminal); terminal {
			return errors.New("acp: agent advertised terminal authentication without client support")
		}
	}
	return nil
}

func (m authenticationMethods) accepts(methodID AuthMethodID) error {
	for _, method := range m {
		advertised, known := authenticationMethod(method)
		if !known || advertised.id != methodID {
			continue
		}
		if advertised.agentHandled {
			return nil
		}
		return newError(ErrorCodeInvalidParams,
			"authentication method %q is terminal-based and requires running the agent "+
				"in a terminal rather than by calling authenticate", methodID)
	}
	return newError(ErrorCodeInvalidParams,
		"%q is not one of the authentication methods advertised in the initialize response", methodID)
}
