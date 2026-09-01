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
		return nil, fmt.Errorf("acp: invalid authentication methods: %w", err)
	}
	for _, method := range owned {
		id, agentHandled, _ := authenticationMethod(method)
		if agentHandled && !hasAgentHandler {
			return nil, fmt.Errorf("acp: authentication method %q requires an Authenticate handler", id)
		}
	}
	return owned, nil
}

func ownAuthenticationMethods(methods []AuthMethod) (authenticationMethods, error) {
	owned := authenticationMethods(deepCopy(methods))
	seen := make(map[AuthMethodID]struct{}, len(owned))
	for index, method := range owned {
		id, _, ok := authenticationMethod(method)
		if !ok {
			return nil, fmt.Errorf("authentication method %d is nil or has an unknown form", index)
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("two authentication methods share the identifier %q", id)
		}
		seen[id] = struct{}{}
	}
	return owned, nil
}

func authenticationMethod(method AuthMethod) (AuthMethodID, bool, bool) {
	switch method := method.(type) {
	case *AuthMethodAgent:
		if method == nil {
			return "", false, false
		}
		return method.ID, true, true
	case *AuthMethodTerminal:
		if method == nil {
			return "", false, false
		}
		return method.ID, false, true
	default:
		return "", false, false
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
			return errors.New("acp: the agent advertised terminal authentication although the client did not enable it")
		}
	}
	return nil
}

func (m authenticationMethods) accepts(methodID AuthMethodID) error {
	for _, method := range m {
		id, agentHandled, ok := authenticationMethod(method)
		if !ok || id != methodID {
			continue
		}
		if agentHandled {
			return nil
		}
		return newError(ErrorCodeInvalidParams,
			"%s is a terminal authentication method, which is performed by running the agent "+
				"in a terminal rather than by calling authenticate", methodID)
	}
	return newError(ErrorCodeInvalidParams,
		"%s is not one of the authentication methods advertised in the initialize response", methodID)
}
