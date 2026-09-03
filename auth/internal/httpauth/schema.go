// Package httpauth shares authentication and authorization HTTP schema declarations.
package httpauth

import (
	"net/http"
	"reflect"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
)

// DeclareErrorResponse adds Huma's standard problem response for status.
func DeclareErrorResponse(api huma.API, op *huma.Operation, status int) {
	if op.Responses == nil {
		op.Responses = map[string]*huma.Response{}
	}
	key := strconv.Itoa(status)
	if op.Responses[key] != nil {
		return
	}
	op.Responses[key] = &huma.Response{
		Description: http.StatusText(status),
		Content: map[string]*huma.MediaType{
			"application/problem+json": {
				Schema: api.OpenAPI().Components.Schemas.Schema(
					reflect.TypeFor[huma.ErrorModel](),
					true,
					"Error",
				),
			},
		},
	}
}
