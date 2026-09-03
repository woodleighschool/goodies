package blobyhuma_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/woodleighschool/goodies/bloby"
	blobyhuma "github.com/woodleighschool/goodies/bloby/huma"
)

func TestUploadSchemaMatchesSerializedTransferContract(t *testing.T) {
	registry := huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer)
	schema := registry.Schema(reflect.TypeFor[blobyhuma.UploadAction](), true, "UploadAction")
	target := &bloby.UploadTarget{URL: "https://storage.invalid/upload", Method: "PUT"}
	for _, test := range []struct {
		name   string
		action bloby.UploadAction
		valid  bool
	}{
		{name: "direct", action: bloby.UploadAction{Strategy: bloby.StrategyDirectPut, Target: target}, valid: true},
		{name: "multipart", action: bloby.UploadAction{Strategy: bloby.StrategyMultipart}, valid: true},
		{name: "missing direct target", action: bloby.UploadAction{Strategy: bloby.StrategyDirectPut}},
		{name: "unexpected multipart target", action: bloby.UploadAction{Strategy: bloby.StrategyMultipart, Target: target}},
		{name: "unknown strategy", action: bloby.UploadAction{Strategy: "unknown"}},
		{name: "unsupported method", action: bloby.UploadAction{Strategy: bloby.StrategyDirectPut, Target: &bloby.UploadTarget{URL: target.URL, Method: "POST"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, err := json.Marshal(blobyhuma.UploadAction(test.action))
			if err != nil {
				t.Fatal(err)
			}
			var value any
			if err := json.Unmarshal(body, &value); err != nil {
				t.Fatal(err)
			}
			result := &huma.ValidateResult{}
			huma.Validate(registry, schema, huma.NewPathBuffer(nil, 0), huma.ModeReadFromServer, value, result)
			if (len(result.Errors) == 0) != test.valid {
				t.Fatalf("valid=%t, errors=%v, payload=%s", test.valid, result.Errors, body)
			}
		})
	}
}
