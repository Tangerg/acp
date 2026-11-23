package acp

import "fmt"

// The option identities belong to one exchange. A callback may change its UI
// model, but cannot rewrite what the peer offered after a choice was made.
type permissionOptions map[PermissionOptionID]struct{}

func ownPermissionOptions(options []PermissionOption) (permissionOptions, error) {
	identities := make(permissionOptions, len(options))
	for _, option := range options {
		if _, duplicate := identities[option.OptionID]; duplicate {
			return nil, fmt.Errorf("permission option %q is offered more than once", option.OptionID)
		}
		identities[option.OptionID] = struct{}{}
	}
	return identities, nil
}

func (o permissionOptions) validate(outcome RequestPermissionOutcome) error {
	selected, ok := outcome.(*SelectedPermissionOutcome)
	if !ok || selected == nil {
		return nil
	}
	if _, offered := o[selected.OptionID]; !offered {
		return fmt.Errorf("permission outcome selects option %q that was not offered", selected.OptionID)
	}
	return nil
}
