package acp

import (
	"fmt"
	"log/slog"
)

// authenticationMethods is the identifier namespace exchanged during initialize.
// The peer selects by ID alone, so an identifier names at most one method.
type authenticationMethods []AuthMethod

// newAuthenticationMethods validates an agent's configured methods, which are its
// own to get right.
func newAuthenticationMethods(methods []AuthMethod, hasAgentHandler bool) (authenticationMethods, error) {
	owned := authenticationMethods(deepCopy(methods))
	seen := make(map[AuthMethodID]bool, len(owned))
	for index, method := range owned {
		advertised, known := authenticationMethod(method)
		switch {
		case !known:
			return nil, fmt.Errorf("acp: AgentConfig.AuthMethods[%d] is nil or has an unknown form", index)
		case seen[advertised.id]:
			return nil, fmt.Errorf("acp: AgentConfig.AuthMethods shares the identifier %q", advertised.id)
		case advertised.agentHandled && !hasAgentHandler:
			return nil, fmt.Errorf("acp: authentication method %q requires AgentConfig.Authenticate", advertised.id)
		}
		seen[advertised.id] = true
	}
	return owned, nil
}

// receivedAuthenticationMethods keeps the methods an agent offered that a client
// can use. The schema skips invalid items of this list rather than failing the
// response, and the obligations it states — unique identifiers, terminal methods
// only for a client that enabled them — are the agent's; so an entry breaking one
// is dropped and logged rather than costing the connection.
func receivedAuthenticationMethods(
	methods []AuthMethod,
	client ClientCapabilities,
	logger *slog.Logger,
) authenticationMethods {
	kept := make(authenticationMethods, 0, len(methods))
	seen := make(map[AuthMethodID]bool, len(methods))
	for _, method := range methods {
		advertised, known := authenticationMethod(method)
		_, terminal := method.(*AuthMethodTerminal)
		switch {
		case !known:
			continue
		case seen[advertised.id]:
			logger.Warn("the agent offered an authentication method identifier twice; keeping the first",
				slog.String("methodId", string(advertised.id)))
		case terminal && !client.Auth.Terminal:
			logger.Warn("the agent offered terminal authentication to a client that did not enable it",
				slog.String("methodId", string(advertised.id)))
		default:
			seen[advertised.id] = true
			kept = append(kept, method)
		}
	}
	return kept
}

// An advertisedAuth is what this package needs from one offered method: the
// identifier a client names, and whether the agent performs it by answering
// authenticate rather than by being run again in a terminal.
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

// offeredTo returns the methods to offer one client: a terminal method only to a
// client that enabled terminal authentication.
func (m authenticationMethods) offeredTo(client ClientCapabilities) []AuthMethod {
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

// accepts checks an identifier passed to authenticate: it must be one the agent
// offered, and a terminal method is not passed to authenticate at all.
func (m authenticationMethods) accepts(methodID AuthMethodID) *Error {
	for _, method := range m {
		advertised, known := authenticationMethod(method)
		if !known || advertised.id != methodID {
			continue
		}
		if advertised.agentHandled {
			return nil
		}
		return newError(ErrorCodeInvalidParams,
			"authentication method %q is terminal-based and is performed by running the agent in a terminal, "+
				"not by calling authenticate", methodID)
	}
	return newError(ErrorCodeInvalidParams,
		"%q is not one of the authentication methods offered in the initialize response", methodID)
}
