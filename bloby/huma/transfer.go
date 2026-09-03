// Package blobyhuma describes Bloby transfer actions for Huma APIs and clients.
package blobyhuma

import (
	"reflect"

	"github.com/danielgtaylor/huma/v2"
	"github.com/woodleighschool/goodies/bloby"
)

// UploadAction exposes Bloby's transfer contract as a discriminated Huma schema.
type UploadAction bloby.UploadAction

// Schema describes the two valid upload actions to Huma and generated clients.
func (UploadAction) Schema(registry huma.Registry) *huma.Schema {
	return &huma.Schema{OneOf: []*huma.Schema{
		{
			Type: huma.TypeObject,
			Properties: map[string]*huma.Schema{
				"strategy": {Type: huma.TypeString, Enum: []any{bloby.StrategyDirectPut}},
				"target":   registry.Schema(reflect.TypeFor[bloby.UploadTarget](), true, "UploadTarget"),
			},
			Required:             []string{"strategy", "target"},
			AdditionalProperties: false,
		},
		{
			Type: huma.TypeObject,
			Properties: map[string]*huma.Schema{
				"strategy": {Type: huma.TypeString, Enum: []any{bloby.StrategyMultipart}},
			},
			Required:             []string{"strategy"},
			AdditionalProperties: false,
		},
	}}
}
