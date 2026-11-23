package acp

import "fmt"

// permissionOptions is the set of option identifiers one request offered. It is
// taken before a handler runs: a handler may edit the request it was given, but
// cannot rewrite what the peer offered after a choice was made.
type permissionOptions map[PermissionOptionID]struct{}

func offeredOptions(options []PermissionOption) permissionOptions {
	offered := make(permissionOptions, len(options))
	for _, option := range options {
		offered[option.OptionID] = struct{}{}
	}
	return offered
}

// distinctOptions refuses an offer naming one identifier twice, which no answer
// could select unambiguously.
func distinctOptions(options []PermissionOption) error {
	seen := make(map[PermissionOptionID]bool, len(options))
	for _, option := range options {
		if seen[option.OptionID] {
			return fmt.Errorf("acp: permission option %q is offered more than once", option.OptionID)
		}
		seen[option.OptionID] = true
	}
	return nil
}

// check refuses an outcome selecting an option that was not offered.
func (o permissionOptions) check(outcome RequestPermissionOutcome) error {
	selected, ok := outcome.(*SelectedPermissionOutcome)
	if !ok || selected == nil {
		return nil
	}
	if _, offered := o[selected.OptionID]; !offered {
		return fmt.Errorf("acp: permission outcome selects option %q that was not offered", selected.OptionID)
	}
	return nil
}
