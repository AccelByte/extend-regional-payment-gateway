//go:build proto

package pb

import "google.golang.org/protobuf/proto"

// Action_ACTION_* aliases map the naming used in gateway.go / authServerInterceptor.go
// to the values generated from permission.proto.
const (
	Action_ACTION_CREATE = Action_CREATE
	Action_ACTION_READ   = Action_READ
	Action_ACTION_UPDATE = Action_UPDATE
	Action_ACTION_DELETE = Action_DELETE
)

// GetResourceExtension reads the permission.resource proto extension from method options.
func GetResourceExtension(m proto.Message) string {
	v := proto.GetExtension(m, E_Resource)
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// GetActionExtension reads the permission.action proto extension from method options.
func GetActionExtension(m proto.Message) Action {
	v := proto.GetExtension(m, E_Action)
	if a, ok := v.(Action); ok {
		return a
	}
	return Action_unknown
}
