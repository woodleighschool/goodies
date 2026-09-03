package authhuma

import (
	"reflect"

	"github.com/danielgtaylor/huma/v2"
	"github.com/woodleighschool/goodies/auth/authz"
)

// RegisterSchemas preserves authorization enums without coupling domain types to Huma.
// Call it before registering operations on the schema registry.
func RegisterSchemas(registry huma.Registry) {
	registry.RegisterTypeAlias(reflect.TypeFor[authz.Access](), reflect.TypeFor[access]())
}

type access string

func (access) Schema(huma.Registry) *huma.Schema {
	return &huma.Schema{Type: "string", Enum: []any{string(authz.None), string(authz.View), string(authz.Edit)}}
}
