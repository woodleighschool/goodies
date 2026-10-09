package blobyhuma_test

import (
	"encoding/json"
	"reflect"
	"strings"
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

// Applications accept a declaration by embedding bloby.Content in a request
// body, so its schema is what rejects a malformed one at the HTTP boundary.
func TestContentSchemaAdmitsOnlyWellFormedDeclarations(t *testing.T) {
	registry := huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer)
	schema := registry.Schema(reflect.TypeFor[bloby.Content](), true, "Content")
	const sha256 = "15e2b0d3c33891ebb0f1ef609ec419420c20e320ce94c65fbc8c3312448eb225"
	const crc64nvme = "ae8b14860a799888"
	for _, test := range []struct {
		name    string
		payload string
		valid   bool
	}{
		{name: "declaration", payload: `{"size_bytes":9,"sha256":"` + sha256 + `","crc64nvme":"` + crc64nvme + `"}`, valid: true},
		{name: "empty content", payload: `{"size_bytes":0,"sha256":"` + sha256 + `","crc64nvme":"` + crc64nvme + `"}`, valid: true},
		{name: "negative size", payload: `{"size_bytes":-1,"sha256":"` + sha256 + `","crc64nvme":"` + crc64nvme + `"}`},
		{name: "uppercase SHA-256", payload: `{"size_bytes":9,"sha256":"` + strings.ToUpper(sha256) + `","crc64nvme":"` + crc64nvme + `"}`},
		{name: "base64 CRC64NVME", payload: `{"size_bytes":9,"sha256":"` + sha256 + `","crc64nvme":"rosUhgp5mIg="}`},
		{name: "missing CRC64NVME", payload: `{"size_bytes":9,"sha256":"` + sha256 + `"}`},
		{name: "missing size", payload: `{"sha256":"` + sha256 + `","crc64nvme":"` + crc64nvme + `"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var value any
			if err := json.Unmarshal([]byte(test.payload), &value); err != nil {
				t.Fatal(err)
			}
			result := &huma.ValidateResult{}
			huma.Validate(registry, schema, huma.NewPathBuffer(nil, 0), huma.ModeWriteToServer, value, result)
			if (len(result.Errors) == 0) != test.valid {
				t.Fatalf("valid=%t, errors=%v, payload=%s", test.valid, result.Errors, test.payload)
			}
		})
	}
}
